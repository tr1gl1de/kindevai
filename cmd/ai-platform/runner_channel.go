package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// newRunnerChannel wires the runner command/event channel (KIN-22): commands go into per-runner transactional
// command logs; events are checked against the workflow's fencing tokens.
func newRunnerChannel(inst *bootstrap.Installation, wf *workflowapp.Service, logger *slog.Logger) *runnersapp.Channel {
	return &runnersapp.Channel{
		Repo:       runnersdb.New(inst.DB),
		Executions: runnerExecutions{wf},
		Tx:         db.Transactor{DB: inst.DB},
		Now:        time.Now,
		NewID:      rand.Text,
		Logger:     logger,
	}
}

// attachRunnerChannel runs the channel on every authorized runner session.
func attachRunnerChannel(h *runnersgw.Handler, ch *runnersapp.Channel) {
	if h.Messages == nil {
		h.Messages = map[string]runnersgw.MessageFunc{}
	}
	for _, typ := range ch.Types() {
		h.Messages[typ] = func(ctx context.Context, s runnersapp.Session, env protocol.Envelope) error {
			return ch.Handle(ctx, s, env)
		}
	}
	h.Sessions = append(h.Sessions, ch.Serve)
}

// runnerExecutions gives the runners context the workflow's attempt reports, translating its errors: a superseded
// token, an expired lease or an unknown step make the attempt stale (the runner is told to stop it); a report
// that does not fit the step's state is rejected.
type runnerExecutions struct{ wf *workflowapp.Service }

func (r runnerExecutions) CheckAttempt(ctx context.Context, runnerID, step string, fence int64) error {
	return attemptError(r.wf.CheckFence(ctx, step, runnerID, fence))
}

func (r runnerExecutions) StartAttempt(ctx context.Context, runnerID, step string, fence int64) error {
	return attemptError(r.wf.StartStep(ctx, step, runnerID, fence))
}

func (r runnerExecutions) CompleteAttempt(ctx context.Context, runnerID, step string, fence int64) error {
	return attemptError(r.wf.CompleteStep(ctx, step, runnerID, fence))
}

func (r runnerExecutions) FailAttempt(ctx context.Context, runnerID, step string, fence int64, reason string) error {
	return attemptError(r.wf.FailStep(ctx, step, runnerID, fence, reason))
}

func attemptError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workflow.ErrStaleFence), errors.Is(err, workflow.ErrLeaseExpired), errors.Is(err, workflow.ErrNotFound):
		return errors.Join(runners.ErrStaleAttempt, err)
	case errors.Is(err, workflow.ErrInvalidTransition), errors.Is(err, workflow.ErrAttemptUnknown):
		return errors.Join(runners.ErrEventRejected, err)
	}
	return err
}
