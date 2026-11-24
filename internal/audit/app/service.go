// Package app holds the audit use cases: recording events (called by every other bounded context inside its own
// transaction) and the Owner's audit view. There is deliberately no use case that changes or deletes events.
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/audit/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/redact"
)

// Repository appends and reads events. Append must run on the caller's transaction (db.Conn), so an event is
// stored exactly when the action it records commits.
type Repository interface {
	Append(ctx context.Context, e domain.Event) (int64, error)
	// Query returns at most f.Limit events matching f, newest first.
	Query(ctx context.Context, f domain.Filter) ([]domain.Event, error)
	// Kinds returns every kind recorded so far, sorted.
	Kinds(ctx context.Context) ([]string, error)
}

// Actor is who performed an action (see domain.Actor).
type Actor = domain.Actor

// UserActor is the signed-in user p.
func UserActor(p auth.Principal) Actor { return User(p.UserID, p.Login) }

// User is an account acting by id, labelled with its login.
func User(id, login string) Actor { return Actor{Type: domain.ActorUser, ID: id, Label: login} }

// Anonymous is an unauthenticated caller, such as a failed sign-in attempt.
func Anonymous() Actor { return Actor{Type: domain.ActorSystem, ID: "anonymous", Label: "anonymous"} }

// System is the Control Plane acting on its own.
func System() Actor { return Actor{Type: domain.ActorSystem, Label: "system"} }

// Entry is what a bounded context records. Kind is "<namespace>.<action>"; Subject is "<type>:<id>" of what was
// acted on; Details is a small projection (ids, names, roles). Secrets must not be passed; values that look like
// credentials are redacted anyway.
type Entry struct {
	Actor     Actor
	Kind      string
	ProjectID string
	Subject   string
	Details   map[string]string
}

// Page is one page of events, newest first. Next is the cursor for the following page (0 when there is none).
type Page struct {
	Events []domain.Event
	Next   int64
}

// Service implements the audit log.
type Service struct {
	Repo Repository
	Now  func() time.Time
}

// Record appends an event. Call it with the ctx of the transaction that performs the action (db.Transactor.InTx),
// so the action and its audit event commit or roll back together; an error must fail the action.
func (s *Service) Record(ctx context.Context, e Entry) error {
	subject := e.Subject
	if redact.SecretValue(subject) {
		subject = redact.Placeholder
	} else {
		subject = redact.Text(subject)
	}
	ev, err := domain.NewEvent(s.Now(), e.Actor, e.Kind, e.ProjectID, subject, redact.Fields(e.Details))
	if err != nil {
		return err
	}
	if _, err := s.Repo.Append(ctx, ev); err != nil {
		return fmt.Errorf("recording audit event %s: %w", ev.Kind, err)
	}
	return nil
}

// Query returns a page of events matching f (Owner only: the log spans every project).
func (s *Service) Query(ctx context.Context, actor auth.Principal, f domain.Filter) (Page, error) {
	if !actor.Owner {
		return Page{}, domain.ErrForbidden
	}
	f, err := domain.NewFilter(f)
	if err != nil {
		return Page{}, err
	}
	limit := f.Limit
	f.Limit++ // one extra row tells whether another page exists
	events, err := s.Repo.Query(ctx, f)
	if err != nil {
		return Page{}, fmt.Errorf("querying audit events: %w", err)
	}
	p := Page{Events: events}
	if len(events) > limit {
		p.Events = events[:limit]
		p.Next = p.Events[limit-1].ID
	}
	return p, nil
}

// Kinds lists the recorded event kinds, for the Owner's filter (Owner only).
func (s *Service) Kinds(ctx context.Context, actor auth.Principal) ([]string, error) {
	if !actor.Owner {
		return nil, domain.ErrForbidden
	}
	k, err := s.Repo.Kinds(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing audit kinds: %w", err)
	}
	return k, nil
}
