// Package app holds the integrations use cases: the Owner configures the installation's YouTrack connection (its
// token kept in the encrypted Vault), tests it asynchronously through the outbox, binds projects to YouTrack
// projects and keeps each project's repository registry (KIN-26). Later adapters read settings and credentials
// through the ports YouTrackSettings, YouTrackProjectOf, RepositoriesOf and Credentials.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/redact"
)

// Audit event kinds. Credentials never appear in events; a changed token is recorded as tokenChanged=true.
const (
	KindYouTrackConfigured     = "integration.youtrack_configured"
	KindYouTrackDisconnected   = "integration.youtrack_disconnected"
	KindYouTrackCheckRequested = "integration.youtrack_check_requested"
	KindYouTrackChecked        = "integration.youtrack_checked"
	KindYouTrackProjectBound   = "integration.youtrack_project_bound"
	KindYouTrackProjectUnbound = "integration.youtrack_project_unbound"
	KindRepositoryAdded        = "integration.repository_added"
	KindRepositoryUpdated      = "integration.repository_updated"
	KindRepositoryRemoved      = "integration.repository_removed"
)

// Outbox adapter and topic of the connection test.
const (
	AdapterYouTrack    = "youtrack"
	TopicYouTrackCheck = "youtrack.check_connection"
)

// Store keeps the integration settings. Every method runs on the caller's transaction (db.Conn).
type Store interface {
	YouTrack(ctx context.Context) (domain.YouTrackConnection, error) // domain.ErrNotConfigured
	SaveYouTrack(ctx context.Context, c domain.YouTrackConnection) error
	DeleteYouTrack(ctx context.Context) error

	ProjectYouTrack(ctx context.Context, projectID string) (string, error) // "" when unbound
	// SetProjectYouTrack binds the project ("" unbinds); domain.ErrShortNameTaken if another project has it.
	SetProjectYouTrack(ctx context.Context, projectID, shortName string, at time.Time) error

	Repositories(ctx context.Context, projectID string) ([]domain.Repository, error)
	Repository(ctx context.Context, projectID, id string) (domain.Repository, error) // domain.ErrNotFound
	CreateRepository(ctx context.Context, r domain.Repository) error                 // domain.ErrRepositoryExists
	SaveRepository(ctx context.Context, r domain.Repository) error
	DeleteRepository(ctx context.Context, projectID, id string) error // domain.ErrNotFound
}

// TxRunner runs fn in one database transaction; repositories called with fn's ctx join it.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// AuditLog is the audit context's API; Record joins the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Outbox stores outgoing messages in the caller's transaction (messaging.Outbox).
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// ProjectRef is how the integrations context sees a project (owned by the projects context).
type ProjectRef struct {
	ID   string
	Name string
}

// Projects is the projects context's API as integrations need it.
type Projects interface {
	Lookup(ctx context.Context, projectID string) (ProjectRef, error) // domain.ErrNotFound if missing
}

// YouTrackProbe tests a YouTrack connection. It reports failures as a result with a stable code and a short
// detail that must not contain the token or a response body.
type YouTrackProbe interface {
	Probe(ctx context.Context, baseURL string, token domain.Secret) domain.CheckResult
}

// Service implements the integrations use cases.
type Service struct {
	Vault    *Vault
	Store    Store
	Projects Projects
	Tx       TxRunner
	Audit    AuditLog
	Outbox   Outbox
	Probe    YouTrackProbe
	Now      func() time.Time
	NewID    func() string
	Logger   *slog.Logger
	// RepositoryAdded runs inside the transaction that registers a repository (GitHubService.RepositoryAdded reads
	// it from GitHub); nil does nothing.
	RepositoryAdded func(ctx context.Context, r domain.Repository) error
}

// YouTrackView is the connection as the Owner sees it: the token only as metadata.
type YouTrackView struct {
	Connection domain.YouTrackConnection
	Token      domain.SecretMeta
}

// YouTrackSettings is what the YouTrack adapter (KIN-27) needs to call the API.
type YouTrackSettings struct {
	BaseURL string
	Token   domain.Secret
}

// ProjectIntegrations are a project's integration settings.
type ProjectIntegrations struct {
	Project         ProjectRef
	YouTrackProject string // "" when unbound
	Repositories    []domain.Repository
}

// RepositoryInput registers a repository; an empty IntegrationBranch means "dev".
type RepositoryInput struct {
	Owner             string
	Name              string
	IntegrationBranch string
}

var errNotConfigured = errors.New("integrations: audit log, outbox or transactions not configured")

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

// inTx runs fn in one transaction; a wiring mistake must not silently drop the audit record.
func (s *Service) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.Audit == nil || s.Tx == nil || s.Outbox == nil {
		return errNotConfigured
	}
	return s.Tx.InTx(ctx, fn)
}

func (s *Service) record(ctx context.Context, actor auditapp.Actor, kind, projectID, subject string, details map[string]string) error {
	return s.Audit.Record(ctx, auditapp.Entry{Actor: actor, Kind: kind, ProjectID: projectID, Subject: subject, Details: details})
}

const youtrackSubject = "integration:youtrack"

func (s *Service) youtrackView(ctx context.Context) (*YouTrackView, error) {
	c, err := s.Store.YouTrack(ctx)
	if errors.Is(err, domain.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading youtrack connection: %w", err)
	}
	meta, err := s.Vault.Meta(ctx, domain.SecretYouTrackToken)
	if err != nil {
		return nil, fmt.Errorf("loading youtrack token metadata: %w", err)
	}
	c.Check = c.CheckAt(s.Now())
	return &YouTrackView{Connection: c, Token: meta}, nil
}

// YouTrack returns the connection (Owner only); nil when none is configured.
func (s *Service) YouTrack(ctx context.Context, actor auth.Principal) (*YouTrackView, error) {
	if err := ownerOnly(actor); err != nil {
		return nil, err
	}
	return s.youtrackView(ctx)
}

// ConfigureYouTrack sets the base URL and, when token is non-empty, replaces the stored token (Owner only). The
// first configuration needs a token. A changed URL or token resets the last connection test.
func (s *Service) ConfigureYouTrack(ctx context.Context, actor auth.Principal, baseURL, token string) (YouTrackView, error) {
	if err := ownerOnly(actor); err != nil {
		return YouTrackView{}, err
	}
	u, err := domain.NewBaseURL(baseURL)
	if err != nil {
		return YouTrackView{}, err
	}
	var tok domain.Secret
	if token != "" {
		if tok, err = domain.NewYouTrackToken(token); err != nil {
			return YouTrackView{}, err
		}
	}
	var view *YouTrackView
	err = s.inTx(ctx, func(ctx context.Context) error {
		now := s.Now()
		c, err := s.Store.YouTrack(ctx)
		switch {
		case errors.Is(err, domain.ErrNotConfigured):
			if tok.IsZero() {
				return &domain.ValidationError{Field: "token", Code: "required"}
			}
			c = domain.NewYouTrackConnection(u, now)
		case err != nil:
			return fmt.Errorf("loading youtrack connection: %w", err)
		default:
			// The stored token is never sent to another host: a new URL needs the token again.
			if c.BaseURL != u && tok.IsZero() {
				return &domain.ValidationError{Field: "token", Code: "token_required_for_url"}
			}
			if c.BaseURL != u || !tok.IsZero() {
				c.ResetCheck()
			}
			c.BaseURL, c.UpdatedAt = u, now
		}
		if !tok.IsZero() {
			if err := s.Vault.Put(ctx, domain.SecretYouTrackToken, tok, actor.UserID); err != nil {
				return err
			}
		}
		if err := s.Store.SaveYouTrack(ctx, c); err != nil {
			return fmt.Errorf("saving youtrack connection: %w", err)
		}
		if view, err = s.youtrackView(ctx); err != nil {
			return err
		}
		return s.record(ctx, auditapp.UserActor(actor), KindYouTrackConfigured, "", youtrackSubject,
			map[string]string{"baseUrl": u, "tokenChanged": strconv.FormatBool(!tok.IsZero())})
	})
	if err != nil {
		return YouTrackView{}, err
	}
	s.logger().Info("youtrack connection configured", "base_url", u, "token_changed", !tok.IsZero(), "by", actor.UserID)
	return *view, nil
}

// DisconnectYouTrack removes the connection and its token (Owner only); project bindings stay.
func (s *Service) DisconnectYouTrack(ctx context.Context, actor auth.Principal) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		if _, err := s.Store.YouTrack(ctx); err != nil {
			return err
		}
		if err := s.Store.DeleteYouTrack(ctx); err != nil {
			return fmt.Errorf("deleting youtrack connection: %w", err)
		}
		if err := s.Vault.Delete(ctx, domain.SecretYouTrackToken); err != nil {
			return fmt.Errorf("deleting youtrack token: %w", err)
		}
		return s.record(ctx, auditapp.UserActor(actor), KindYouTrackDisconnected, "", youtrackSubject, nil)
	})
}

type checkPayload struct {
	CheckID string `json:"checkId"`
}

// CheckYouTrack starts a connection test (Owner only): the connection is marked pending and the test is enqueued
// in the outbox in the same transaction; HandleYouTrackCheck runs it.
func (s *Service) CheckYouTrack(ctx context.Context, actor auth.Principal) (YouTrackView, error) {
	if err := ownerOnly(actor); err != nil {
		return YouTrackView{}, err
	}
	id := s.NewID()
	payload, err := json.Marshal(checkPayload{CheckID: id})
	if err != nil {
		return YouTrackView{}, fmt.Errorf("encoding check: %w", err)
	}
	var view *YouTrackView
	err = s.inTx(ctx, func(ctx context.Context) error {
		c, err := s.Store.YouTrack(ctx)
		if err != nil {
			return err
		}
		if c.CheckRunning(s.Now()) { // a test is already on its way; do not queue another
			view, err = s.youtrackView(ctx)
			return err
		}
		c.StartCheck(id, s.Now())
		if err := s.Store.SaveYouTrack(ctx, c); err != nil {
			return fmt.Errorf("saving youtrack connection: %w", err)
		}
		if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterYouTrack, Topic: TopicYouTrackCheck,
			Key: "youtrack-check/" + id, Payload: payload}); err != nil {
			return fmt.Errorf("enqueueing connection test: %w", err)
		}
		if view, err = s.youtrackView(ctx); err != nil {
			return err
		}
		return s.record(ctx, auditapp.UserActor(actor), KindYouTrackCheckRequested, "", youtrackSubject, map[string]string{"checkId": id})
	})
	if err != nil {
		return YouTrackView{}, err
	}
	return *view, nil
}

// HandleYouTrackCheck is the outbox handler of TopicYouTrackCheck: it probes YouTrack with the stored token and
// records the result. A test that was superseded (new test, changed settings, disconnect) or already finished is
// dropped. A failed probe is a result shown to the Owner, not a delivery error, so it is not retried.
func (s *Service) HandleYouTrackCheck(ctx context.Context, d messaging.Delivery) error {
	var p checkPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.CheckID == "" {
		return messaging.Permanent(errors.New("malformed connection test payload"))
	}
	c, err := s.Store.YouTrack(ctx)
	if errors.Is(err, domain.ErrNotConfigured) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading youtrack connection: %w", err)
	}
	if c.Check.Status != domain.CheckPending || c.Check.ID != p.CheckID {
		return nil
	}
	var res domain.CheckResult
	settings, err := s.YouTrackSettings(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured):
		return nil // disconnected meanwhile: nothing left to report on
	case errors.Is(err, domain.ErrSecretNotSet):
		res = domain.CheckResult{Code: "not_configured"}
	case err != nil:
		return err
	default:
		res = s.Probe.Probe(ctx, settings.BaseURL, settings.Token)
	}
	// Safety net: the probe must not put credentials into the detail; mask anything that looks like one.
	res.Detail = redact.Text(res.Detail)
	res.Code = redact.Text(res.Code)
	var applied bool
	err = s.inTx(ctx, func(ctx context.Context) error {
		c, err := s.Store.YouTrack(ctx)
		if errors.Is(err, domain.ErrNotConfigured) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loading youtrack connection: %w", err)
		}
		if applied = c.FinishCheck(p.CheckID, res, s.Now()); !applied {
			return nil
		}
		if err := s.Store.SaveYouTrack(ctx, c); err != nil {
			return fmt.Errorf("saving youtrack connection: %w", err)
		}
		return s.record(ctx, auditapp.System(), KindYouTrackChecked, "", youtrackSubject,
			map[string]string{"checkId": p.CheckID, "status": string(c.Check.Status), "code": c.Check.Code})
	})
	if err != nil {
		return err
	}
	if applied {
		s.logger().Info("youtrack connection tested", "check", p.CheckID, "ok", res.OK, "code", res.Code)
	}
	return nil
}

// YouTrackSettings returns the connection with its decrypted token, for the YouTrack adapter (no actor: callers
// are trusted adapters). domain.ErrNotConfigured when there is no connection.
func (s *Service) YouTrackSettings(ctx context.Context) (YouTrackSettings, error) {
	c, err := s.Store.YouTrack(ctx)
	if err != nil {
		return YouTrackSettings{}, err
	}
	tok, err := s.Vault.Credential(ctx, domain.SecretYouTrackToken)
	if err != nil {
		return YouTrackSettings{}, err
	}
	return YouTrackSettings{BaseURL: c.BaseURL, Token: tok}, nil
}

// YouTrackProjectOf returns the YouTrack short name bound to a project ("" when unbound), for adapters.
func (s *Service) YouTrackProjectOf(ctx context.Context, projectID string) (string, error) {
	return s.Store.ProjectYouTrack(ctx, projectID)
}

// RepositoriesOf returns a project's registered repositories, for adapters.
func (s *Service) RepositoriesOf(ctx context.Context, projectID string) ([]domain.Repository, error) {
	return s.Store.Repositories(ctx, projectID)
}

func (s *Service) project(ctx context.Context, actor auth.Principal, projectID string) (ProjectRef, error) {
	if err := ownerOnly(actor); err != nil {
		return ProjectRef{}, err
	}
	return s.Projects.Lookup(ctx, projectID)
}

func (s *Service) projectIntegrations(ctx context.Context, p ProjectRef) (ProjectIntegrations, error) {
	sn, err := s.Store.ProjectYouTrack(ctx, p.ID)
	if err != nil {
		return ProjectIntegrations{}, fmt.Errorf("loading youtrack binding: %w", err)
	}
	rs, err := s.Store.Repositories(ctx, p.ID)
	if err != nil {
		return ProjectIntegrations{}, fmt.Errorf("loading repositories: %w", err)
	}
	for i := range rs {
		rs[i].GitHub.Sync = rs[i].GitHub.Sync.At(s.Now())
	}
	return ProjectIntegrations{Project: p, YouTrackProject: sn, Repositories: rs}, nil
}

// ProjectIntegrations returns a project's integration settings (Owner only).
func (s *Service) ProjectIntegrations(ctx context.Context, actor auth.Principal, projectID string) (ProjectIntegrations, error) {
	p, err := s.project(ctx, actor, projectID)
	if err != nil {
		return ProjectIntegrations{}, err
	}
	return s.projectIntegrations(ctx, p)
}

// BindYouTrackProject links a project to a YouTrack project by short name; "" unbinds (Owner only). A YouTrack
// project is bound to at most one project.
func (s *Service) BindYouTrackProject(ctx context.Context, actor auth.Principal, projectID, shortName string) (ProjectIntegrations, error) {
	p, err := s.project(ctx, actor, projectID)
	if err != nil {
		return ProjectIntegrations{}, err
	}
	sn := ""
	if shortName != "" {
		if sn, err = domain.NewShortName(shortName); err != nil {
			return ProjectIntegrations{}, err
		}
	}
	var out ProjectIntegrations
	err = s.inTx(ctx, func(ctx context.Context) error {
		old, err := s.Store.ProjectYouTrack(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("loading youtrack binding: %w", err)
		}
		if old != sn {
			if err := s.Store.SetProjectYouTrack(ctx, p.ID, sn, s.Now()); err != nil {
				return err
			}
			kind, details := KindYouTrackProjectBound, map[string]string{"shortName": sn}
			if sn == "" {
				kind, details = KindYouTrackProjectUnbound, map[string]string{"shortName": old}
			}
			if err := s.record(ctx, auditapp.UserActor(actor), kind, p.ID, "project:"+p.ID, details); err != nil {
				return err
			}
		}
		out, err = s.projectIntegrations(ctx, p)
		return err
	})
	return out, err
}

func repoDetails(r domain.Repository) map[string]string {
	return map[string]string{"repository": r.FullName(), "integrationBranch": r.IntegrationBranch, "testCommand": r.TestCommand}
}

func repoSubject(id string) string { return "repository:" + id }

// AddRepository registers a GitHub repository for a project (Owner only). owner/name is unique per installation.
func (s *Service) AddRepository(ctx context.Context, actor auth.Principal, projectID string, in RepositoryInput) (domain.Repository, error) {
	p, err := s.project(ctx, actor, projectID)
	if err != nil {
		return domain.Repository{}, err
	}
	r, err := domain.NewRepository(s.NewID(), p.ID, in.Owner, in.Name, in.IntegrationBranch, s.Now())
	if err != nil {
		return domain.Repository{}, err
	}
	err = s.inTx(ctx, func(ctx context.Context) error {
		if err := s.Store.CreateRepository(ctx, r); err != nil {
			return err
		}
		if s.RepositoryAdded != nil {
			if err := s.RepositoryAdded(ctx, r); err != nil {
				return err
			}
			if r, err = s.Store.Repository(ctx, p.ID, r.ID); err != nil {
				return fmt.Errorf("reloading repository: %w", err)
			}
		}
		return s.record(ctx, auditapp.UserActor(actor), KindRepositoryAdded, p.ID, repoSubject(r.ID), repoDetails(r))
	})
	if err != nil {
		return domain.Repository{}, err
	}
	return r, nil
}

// UpdateRepository changes a repository's integration branch and, when testCommand is not nil, its test command
// (KIN-35) (Owner only).
func (s *Service) UpdateRepository(ctx context.Context, actor auth.Principal, projectID, id, branch string, testCommand *string) (domain.Repository, error) {
	p, err := s.project(ctx, actor, projectID)
	if err != nil {
		return domain.Repository{}, err
	}
	var r domain.Repository
	err = s.inTx(ctx, func(ctx context.Context) error {
		if r, err = s.Store.Repository(ctx, p.ID, id); err != nil {
			return err
		}
		if err := r.SetIntegrationBranch(branch, s.Now()); err != nil {
			return err
		}
		if testCommand != nil {
			if err := r.SetTestCommand(*testCommand, s.Now()); err != nil {
				return err
			}
		}
		if err := s.Store.SaveRepository(ctx, r); err != nil {
			return fmt.Errorf("saving repository: %w", err)
		}
		return s.record(ctx, auditapp.UserActor(actor), KindRepositoryUpdated, p.ID, repoSubject(r.ID), repoDetails(r))
	})
	if err != nil {
		return domain.Repository{}, err
	}
	return r, nil
}

// RemoveRepository unregisters a repository (Owner only).
func (s *Service) RemoveRepository(ctx context.Context, actor auth.Principal, projectID, id string) error {
	p, err := s.project(ctx, actor, projectID)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		r, err := s.Store.Repository(ctx, p.ID, id)
		if err != nil {
			return err
		}
		if err := s.Store.DeleteRepository(ctx, p.ID, id); err != nil {
			return err
		}
		return s.record(ctx, auditapp.UserActor(actor), KindRepositoryRemoved, p.ID, repoSubject(r.ID), repoDetails(r))
	})
}
