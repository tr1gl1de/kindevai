package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

// StateStore keeps the bindings' state maps (KIN-38); it runs on the caller's transaction.
type StateStore interface {
	StateMap(ctx context.Context, projectID string) (domain.StateMap, error) // domain.ErrNotFound when unbound
	SetStateMap(ctx context.Context, projectID string, m domain.StateMap, at time.Time) error
}

// StateMap returns the project's task state → YouTrack state map (empty: comments only); domain.ErrNotBound when
// the project has no YouTrack binding. Callers check visibility first.
func (in *Intake) StateMap(ctx context.Context, projectID string) (domain.StateMap, error) {
	m, err := in.States.StateMap(ctx, projectID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, domain.ErrNotBound
	}
	return m, err
}

// SetStateMap replaces the project's state map: the Owner or the project's admin, like the other intake settings.
func (in *Intake) SetStateMap(ctx context.Context, actor auth.Principal, projectID string, states map[string]string) (domain.StateMap, error) {
	if err := in.mayManage(ctx, actor, projectID); err != nil {
		return nil, err
	}
	m, err := domain.NewStateMap(states)
	if err != nil {
		return nil, err
	}
	err = in.Integrations.inTx(ctx, func(ctx context.Context) error {
		if err := in.States.SetStateMap(ctx, projectID, m, in.now()); errors.Is(err, domain.ErrNotFound) {
			return domain.ErrNotBound
		} else if err != nil {
			return err
		}
		pairs := make([]string, 0, len(m))
		for _, k := range slices.Sorted(maps.Keys(m)) {
			pairs = append(pairs, k+"="+m[k])
		}
		return in.Integrations.record(ctx, auditapp.UserActor(actor), KindYouTrackStatesChanged, projectID,
			"project:"+projectID, map[string]string{"states": strings.Join(pairs, "; ")})
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// mayManage lets the Owner and the project's admin change its intake settings; other members are refused and
// everybody else learns nothing about the project.
func (in *Intake) mayManage(ctx context.Context, actor auth.Principal, projectID string) error {
	if actor.Owner {
		return nil
	}
	ok, err := in.Access.CanManageAI(ctx, projectID, actor.UserID)
	if err != nil || ok {
		return err
	}
	if err := in.mayRun(ctx, actor, projectID); err != nil {
		return err
	}
	return domain.ErrForbidden
}
