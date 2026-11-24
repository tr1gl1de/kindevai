package app

import (
	"context"
	"errors"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/maintenance/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

type memStore struct{ m domain.Mode }

func (s *memStore) Load(context.Context) (domain.Mode, error) { return s.m, nil }
func (s *memStore) Save(_ context.Context, m domain.Mode) error {
	s.m = m
	return nil
}

type memAudit struct{ kinds []string }

func (a *memAudit) Record(_ context.Context, e auditapp.Entry) error {
	a.kinds = append(a.kinds, e.Kind)
	return nil
}

type passTx struct{}

func (passTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

type drain int

func (d drain) ActiveSteps(context.Context) (int, error) { return int(d), nil }

var (
	owner  = auth.Principal{UserID: "u1", Login: "owner", Owner: true}
	member = auth.Principal{UserID: "u2", Login: "dev"}
	t0     = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
)

func newService(store *memStore, a *memAudit) *Service {
	return &Service{Store: store, Audit: a, Tx: passTx{}, Drain: drain(2), Now: func() time.Time { return t0 }}
}

// Only the Owner switches maintenance mode; each switch is persisted, audited and pauses scheduling.
func TestOwnerSwitchesMaintenanceMode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, a := &memStore{}, &memAudit{}
	s := newService(store, a)
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, member, true, "x"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	st, err := s.Set(ctx, owner, true, "moving")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || st.By != "owner" || st.Reason != "moving" || st.ActiveAttempts != 2 || !store.m.Enabled {
		t.Fatalf("status %+v store %+v", st, store.m)
	}
	if p, _ := s.SchedulingPaused(ctx); !p {
		t.Fatal("scheduling not paused")
	}
	if _, err := s.Set(ctx, owner, true, "moving"); err != nil { // no change: no second audit event
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, owner, false, ""); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.SchedulingPaused(ctx); p || store.m.Enabled {
		t.Fatal("still paused")
	}
	if len(a.kinds) != 2 || a.kinds[0] != KindEnabled || a.kinds[1] != KindDisabled {
		t.Fatalf("audit %v", a.kinds)
	}
}

// The mode survives a restart: Load restores it from the store.
func TestMaintenanceModeSurvivesRestart(t *testing.T) {
	t.Parallel()
	store := &memStore{m: domain.Mode{Enabled: true, By: "owner", Since: t0}}
	s := newService(store, &memAudit{})
	if err := s.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.Mode().Enabled {
		t.Fatal("mode lost on restart")
	}
}
