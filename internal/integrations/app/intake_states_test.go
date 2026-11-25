package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

type stateStore struct {
	mu     sync.Mutex
	bound  map[string]bool
	states map[string]domain.StateMap
}

func (s *stateStore) StateMap(_ context.Context, projectID string) (domain.StateMap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound[projectID] {
		return nil, domain.ErrNotFound
	}
	return s.states[projectID], nil
}

func (s *stateStore) SetStateMap(_ context.Context, projectID string, m domain.StateMap, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.bound[projectID] {
		return domain.ErrNotFound
	}
	s.states[projectID] = m
	return nil
}

// KIN-38 decision: the task state → YouTrack state map is a project setting (empty by default: comments only),
// changed by the Owner or the project's admin, validated and audited.
func TestStateMapIsAProjectSettingOfItsAdmins(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	st := &stateStore{bound: map[string]bool{"p1": true}, states: map[string]domain.StateMap{}}
	e.intake.States = st
	ctx := t.Context()
	if m, err := e.intake.StateMap(ctx, "p1"); err != nil || len(m) != 0 {
		t.Fatalf("default = %v, %v", m, err)
	}
	want := map[string]string{"RUNNING": "In Progress", "COMPLETED": "Fixed"}
	if _, err := e.intake.SetStateMap(ctx, alicePrincipal, "p1", want); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	if _, err := e.intake.SetStateMap(ctx, auth.Principal{UserID: "bob1"}, "p1", want); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := e.intake.SetStateMap(ctx, adminPrincipal, "p1", map[string]string{"RUNNING": "a}b"}); !errors.As(err, &ve) {
		t.Fatalf("invalid: %v", err)
	}
	m, err := e.intake.SetStateMap(ctx, adminPrincipal, "p1", want)
	if err != nil || m["COMPLETED"] != "Fixed" || len(m) != 2 {
		t.Fatalf("admin: %v, %v", m, err)
	}
	if got, _ := e.intake.StateMap(ctx, "p1"); got.Command("RUNNING") != "State {In Progress}" {
		t.Fatalf("saved = %v", got)
	}
	if _, err := e.intake.SetStateMap(ctx, owner, "p2", want); !errors.Is(err, domain.ErrNotBound) {
		t.Fatalf("unbound: %v", err)
	}
	if n := countKind(e, app.KindYouTrackStatesChanged); n != 1 {
		t.Fatalf("audited %d changes", n)
	}
}
