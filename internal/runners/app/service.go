// Package app holds the runners use cases: the Owner issues enrollment tokens and approves, disables or revokes
// runners; a runner enrolls itself with a token and its Ed25519 public key (KIN-A-17).
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Audit event kinds recorded by the runners context. Tokens never appear in events.
const (
	KindTokenCreated    = "runner.token_created"
	KindTokenRevoked    = "runner.token_revoked"
	KindEnrolled        = "runner.enrolled"
	KindEnrollRejected  = "runner.enroll_rejected"
	KindApproved        = "runner.approved"
	KindProjectsChanged = "runner.projects_changed"
	KindDisabled        = "runner.disabled"
	KindEnabled         = "runner.enabled"
	KindRevoked         = "runner.revoked"
)

// Repository stores enrollment tokens and runners. Every method runs on the caller's transaction (db.Conn).
type Repository interface {
	CreateToken(ctx context.Context, t domain.EnrollmentToken) error
	// ListTokens returns every token pending at now plus the newest limit others, newest first.
	ListTokens(ctx context.Context, limit int, now time.Time) ([]domain.EnrollmentToken, error)
	TokenByID(ctx context.Context, id string) (domain.EnrollmentToken, error)     // domain.ErrNotFound
	TokenByHash(ctx context.Context, hash []byte) (domain.EnrollmentToken, error) // domain.ErrNotFound
	// ConsumeToken marks the token used by runnerID only if it is still pending at now; false if it was not.
	ConsumeToken(ctx context.Context, id, runnerID string, now time.Time) (bool, error)
	// RevokeToken sets revoked_at only if the token is still pending at now; false if it was not.
	RevokeToken(ctx context.Context, id string, now time.Time) (bool, error)

	CreateRunner(ctx context.Context, r domain.Runner) error // domain.ErrKeyEnrolled if the key exists
	Runner(ctx context.Context, id string) (domain.Runner, error)
	Runners(ctx context.Context) ([]domain.Runner, error)
	// SaveRunner writes the status fields and replaces the project permissions; domain.ErrNotFound for an
	// unknown project or runner.
	SaveRunner(ctx context.Context, r domain.Runner) error
}

// TxRunner runs fn in one database transaction; repositories called with fn's ctx join it.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// AuditLog is the audit context's API; Record joins the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// ProjectRef is how the runners context sees a project (owned by the projects context).
type ProjectRef struct {
	ID   string
	Name string
}

// Projects is the projects context's API as runners need it.
type Projects interface {
	Lookup(ctx context.Context, projectID string) (ProjectRef, error) // domain.ErrNotFound if missing
}

// Service implements the runners use cases.
type Service struct {
	Repo     Repository
	Projects Projects
	Tx       TxRunner
	Audit    AuditLog
	Rand     io.Reader
	Now      func() time.Time
	NewID    func() string
	Logger   *slog.Logger
	// Sessions closes live gateway connections when a runner is disabled or revoked (nil: no gateway wired).
	Sessions Disconnector
}

// TokenView is an enrollment token with its state at the time of the call.
type TokenView struct {
	Token domain.EnrollmentToken
	State domain.TokenState
}

// RunnerView is a runner with its projects' names.
type RunnerView struct {
	Runner   domain.Runner
	Projects []ProjectRef // in Runner.ProjectIDs order; Name is "" for a project that disappeared
}

// EnrollRequest is what a runner sends to register itself.
type EnrollRequest struct {
	Token     string
	PublicKey string // standard base64 of the raw 32-byte Ed25519 public key
	Hostname  string
	OS        string
	Arch      string
	Version   string
}

// EnrollResult is the registration; Created is false when a retry found the runner this token already made.
type EnrollResult struct {
	Runner  domain.Runner
	Created bool
}

const tokenListLimit = 50

var (
	errNotConfigured     = errors.New("runners: audit log or transactions not configured")
	errProjectNotFound   = &domain.ValidationError{Field: "projectIds", Code: "project_not_found"}
	errTokenNotRevocable = &domain.ValidationError{Field: "token", Code: "token_not_pending"}
)

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func ownerOnly(p auth.Principal) error {
	if !p.Owner {
		return domain.ErrForbidden
	}
	return nil
}

func tokenSubject(id string) string  { return "runner_enrollment:" + id }
func runnerSubject(id string) string { return "runner:" + id }

// inTx runs write and the audit record in one transaction; a wiring mistake must not silently drop the record.
func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.Audit == nil || s.Tx == nil {
		return errNotConfigured
	}
	return s.Tx.InTx(ctx, fn)
}

// CreateToken issues an enrollment token (Owner only) and returns its plaintext once.
func (s *Service) CreateToken(ctx context.Context, actor auth.Principal) (domain.EnrollmentToken, string, error) {
	if err := ownerOnly(actor); err != nil {
		return domain.EnrollmentToken{}, "", err
	}
	tok, plain, err := domain.NewEnrollmentToken(s.Rand, s.NewID(), actor.UserID, s.Now())
	if err != nil {
		return domain.EnrollmentToken{}, "", err
	}
	err = s.inTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.CreateToken(ctx, tok); err != nil {
			return fmt.Errorf("saving enrollment token: %w", err)
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTokenCreated,
			Subject: tokenSubject(tok.ID), Details: map[string]string{"expiresAt": tok.ExpiresAt.UTC().Format(time.RFC3339)}})
	})
	if err != nil {
		return domain.EnrollmentToken{}, "", err
	}
	s.logger().Info("runner enrollment token created", "enrollment", tok.ID, "by", actor.UserID)
	return tok, plain, nil
}

// ListTokens returns pending and recent enrollment tokens (Owner only), newest first.
func (s *Service) ListTokens(ctx context.Context, actor auth.Principal) ([]TokenView, error) {
	if err := ownerOnly(actor); err != nil {
		return nil, err
	}
	now := s.Now()
	ts, err := s.Repo.ListTokens(ctx, tokenListLimit, now)
	if err != nil {
		return nil, fmt.Errorf("listing enrollment tokens: %w", err)
	}
	out := make([]TokenView, 0, len(ts))
	for _, t := range ts {
		out = append(out, TokenView{Token: t, State: t.State(now)})
	}
	return out, nil
}

// RevokeToken withdraws a pending enrollment token (Owner only).
func (s *Service) RevokeToken(ctx context.Context, actor auth.Principal, id string) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	now := s.Now()
	return s.inTx(ctx, func(ctx context.Context) error {
		tok, err := s.Repo.TokenByID(ctx, id)
		if err != nil {
			return err
		}
		if tok.Usable(now) != nil {
			return errTokenNotRevocable
		}
		if ok, err := s.Repo.RevokeToken(ctx, id, now); err != nil {
			return fmt.Errorf("revoking enrollment token: %w", err)
		} else if !ok {
			return errTokenNotRevocable
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTokenRevoked, Subject: tokenSubject(id)})
	})
}

// Enroll registers a pending runner: the token is consumed in the same transaction that creates the runner, so
// of two concurrent enrollments with one token exactly one succeeds. A retry with the same token and key returns
// the runner it already created; an expired, revoked, used (by another key) or unknown token creates nothing.
func (s *Service) Enroll(ctx context.Context, req EnrollRequest) (EnrollResult, error) {
	key, err := domain.ParsePublicKey(req.PublicKey)
	if err != nil {
		return EnrollResult{}, err
	}
	meta, err := domain.NewMetadata(req.Hostname, req.OS, req.Arch, req.Version)
	if err != nil {
		return EnrollResult{}, err
	}
	if req.Token == "" {
		return EnrollResult{}, domain.ErrTokenInvalid
	}
	hash := domain.HashEnrollmentToken(req.Token)
	var res EnrollResult
	var rejected *domain.EnrollmentToken
	err = s.inTx(ctx, func(ctx context.Context) error {
		tok, err := s.Repo.TokenByHash(ctx, hash)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrTokenInvalid
		}
		if err != nil {
			return fmt.Errorf("looking up enrollment token: %w", err)
		}
		now := s.Now()
		if tok.RunnerID != "" { // used: a retry by the same machine gets its registration back
			r, err := s.Repo.Runner(ctx, tok.RunnerID)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return fmt.Errorf("loading runner: %w", err)
			}
			if err == nil && bytes.Equal(r.PublicKey, key) {
				res = EnrollResult{Runner: r}
				return nil
			}
		}
		if err := tok.Usable(now); err != nil {
			rejected = &tok
			return err
		}
		r := domain.Enroll(s.NewID(), tok, key, meta, now)
		if err := s.Repo.CreateRunner(ctx, r); err != nil {
			if errors.Is(err, domain.ErrKeyEnrolled) {
				rejected = &tok
			}
			return err
		}
		if ok, err := s.Repo.ConsumeToken(ctx, tok.ID, r.ID, now); err != nil {
			return fmt.Errorf("consuming enrollment token: %w", err)
		} else if !ok {
			return domain.ErrTokenUsed
		}
		res = EnrollResult{Runner: r, Created: true}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.Runner(r.ID, meta.Hostname), Kind: KindEnrolled,
			Subject: runnerSubject(r.ID), Details: map[string]string{
				"enrollmentId": tok.ID, "fingerprint": r.Fingerprint, "hostname": meta.Hostname,
				"os": meta.OS, "arch": meta.Arch, "version": meta.Version,
			}})
	})
	if rejected != nil {
		s.auditRejected(ctx, *rejected, err, meta)
	}
	if err != nil {
		return EnrollResult{}, err
	}
	if res.Created {
		s.logger().Info("runner enrolled", "runner", res.Runner.ID, "fingerprint", res.Runner.Fingerprint, "hostname", meta.Hostname)
	}
	return res, nil
}

// auditRejected records an attempt with a known token that was refused: the token was unusable, or the key is
// already enrolled (a leaked or replayed token or key is worth seeing).
// Unknown tokens are only logged: they are guesses, and the rate limit bounds them.
func (s *Service) auditRejected(ctx context.Context, tok domain.EnrollmentToken, cause error, m domain.Metadata) {
	reason := map[error]string{domain.ErrTokenUsed: "used", domain.ErrTokenExpired: "expired", domain.ErrTokenRevoked: "revoked",
		domain.ErrKeyEnrolled: "key_enrolled"}[cause]
	err := s.inTx(ctx, func(ctx context.Context) error {
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.Anonymous(), Kind: KindEnrollRejected,
			Subject: tokenSubject(tok.ID), Details: map[string]string{"reason": reason, "hostname": m.Hostname}})
	})
	if err != nil {
		s.logger().Error("recording rejected enrollment", "err", err)
	}
	s.logger().Warn("runner enrollment rejected", "enrollment", tok.ID, "reason", reason)
}

func (s *Service) view(ctx context.Context, r domain.Runner, names map[string]string) (RunnerView, error) {
	v := RunnerView{Runner: r, Projects: make([]ProjectRef, 0, len(r.ProjectIDs))}
	for _, id := range r.ProjectIDs {
		name, ok := names[id]
		if !ok {
			p, err := s.Projects.Lookup(ctx, id)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return RunnerView{}, fmt.Errorf("looking up project: %w", err)
			}
			name, names[id] = p.Name, p.Name
		}
		v.Projects = append(v.Projects, ProjectRef{ID: id, Name: name})
	}
	return v, nil
}

// List returns every runner (Owner only), pending first, then newest.
func (s *Service) List(ctx context.Context, actor auth.Principal) ([]RunnerView, error) {
	if err := ownerOnly(actor); err != nil {
		return nil, err
	}
	rs, err := s.Repo.Runners(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing runners: %w", err)
	}
	names := map[string]string{}
	out := make([]RunnerView, 0, len(rs))
	for _, r := range rs {
		v, err := s.view(ctx, r, names)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Get returns one runner (Owner only).
func (s *Service) Get(ctx context.Context, actor auth.Principal, id string) (RunnerView, error) {
	if err := ownerOnly(actor); err != nil {
		return RunnerView{}, err
	}
	r, err := s.Repo.Runner(ctx, id)
	if err != nil {
		return RunnerView{}, err
	}
	return s.view(ctx, r, map[string]string{})
}

// change loads the runner, applies op, saves it and records the event in one transaction (Owner only). op gets
// the transaction's ctx: any lookup it makes must join the transaction (the pool has a single connection).
func (s *Service) change(ctx context.Context, actor auth.Principal, id, kind string, op func(ctx context.Context, r *domain.Runner, now time.Time) (map[string]string, error)) (RunnerView, error) {
	if err := ownerOnly(actor); err != nil {
		return RunnerView{}, err
	}
	var r domain.Runner
	err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		if r, err = s.Repo.Runner(ctx, id); err != nil {
			return err
		}
		details, err := op(ctx, &r, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.SaveRunner(ctx, r); errors.Is(err, domain.ErrNotFound) {
			return errProjectNotFound // a project was deleted after the check
		} else if err != nil {
			return fmt.Errorf("saving runner: %w", err)
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: kind,
			Subject: runnerSubject(id), Details: details})
	})
	if err != nil {
		return RunnerView{}, err
	}
	s.logger().Info("runner changed", "runner", id, "event", kind, "by", actor.UserID)
	if s.Sessions != nil { // after the commit, so a reconnect already sees the new status
		switch kind {
		case KindDisabled:
			s.Sessions.Disconnect(id, ReasonDisabled)
		case KindRevoked:
			s.Sessions.Disconnect(id, ReasonRevoked)
		}
	}
	return s.view(ctx, r, map[string]string{})
}

func (s *Service) checkProjects(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := s.Projects.Lookup(ctx, id); errors.Is(err, domain.ErrNotFound) {
			return errProjectNotFound
		} else if err != nil {
			return fmt.Errorf("looking up project: %w", err)
		}
	}
	return nil
}

func projectDetails(r domain.Runner) map[string]string {
	return map[string]string{"projects": strings.Join(r.ProjectIDs, ","), "fingerprint": r.Fingerprint}
}

// Approve lets a pending runner connect and run tasks of the given projects (Owner only).
func (s *Service) Approve(ctx context.Context, actor auth.Principal, id string, projectIDs []string) (RunnerView, error) {
	return s.change(ctx, actor, id, KindApproved, func(ctx context.Context, r *domain.Runner, now time.Time) (map[string]string, error) {
		if err := r.Approve(projectIDs, actor.UserID, now); err != nil {
			return nil, err
		}
		return projectDetails(*r), s.checkProjects(ctx, r.ProjectIDs)
	})
}

// SetProjects replaces the projects of an approved or disabled runner (Owner only).
func (s *Service) SetProjects(ctx context.Context, actor auth.Principal, id string, projectIDs []string) (RunnerView, error) {
	return s.change(ctx, actor, id, KindProjectsChanged, func(ctx context.Context, r *domain.Runner, _ time.Time) (map[string]string, error) {
		if err := r.SetProjects(projectIDs); err != nil {
			return nil, err
		}
		return projectDetails(*r), s.checkProjects(ctx, r.ProjectIDs)
	})
}

// Disable blocks an approved runner (Owner only).
func (s *Service) Disable(ctx context.Context, actor auth.Principal, id string) (RunnerView, error) {
	return s.change(ctx, actor, id, KindDisabled, func(_ context.Context, r *domain.Runner, now time.Time) (map[string]string, error) {
		return nil, r.Disable(now)
	})
}

// Enable lets a disabled runner connect again (Owner only).
func (s *Service) Enable(ctx context.Context, actor auth.Principal, id string) (RunnerView, error) {
	return s.change(ctx, actor, id, KindEnabled, func(_ context.Context, r *domain.Runner, now time.Time) (map[string]string, error) {
		return nil, r.Enable(now)
	})
}

// Revoke permanently rejects the runner's key (Owner only). For a pending runner this rejects the enrollment.
func (s *Service) Revoke(ctx context.Context, actor auth.Principal, id string) (RunnerView, error) {
	return s.change(ctx, actor, id, KindRevoked, func(_ context.Context, r *domain.Runner, now time.Time) (map[string]string, error) {
		from := r.Status
		if err := r.Revoke(actor.UserID, now); err != nil {
			return nil, err
		}
		return map[string]string{"from": string(from), "fingerprint": r.Fingerprint}, nil
	})
}
