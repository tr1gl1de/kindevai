package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strconv"
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
func newRunnerChannel(inst *bootstrap.Installation, wf *workflowapp.Service, ret runners.RetentionPolicy, logger *slog.Logger) *runnersapp.Channel {
	return &runnersapp.Channel{
		Repo:            runnersdb.New(inst.DB),
		Executions:      runnerExecutions{wf},
		Tx:              db.Transactor{DB: inst.DB},
		Now:             time.Now,
		NewID:           rand.Text,
		Logger:          logger,
		MaxStoredOutput: ret.MaxOutputBytes,
	}
}

// reportOutputStorage shows the stored runner output against its limit in the Storage Overview and raises the
// Alert Center's "runner output storage full" alert through the outbox (KIN-59).
func reportOutputStorage(inst *bootstrap.Installation, msg *messagingModule, tel *runnersapp.Telemetry, ch *runnersapp.Channel,
	ret runners.RetentionPolicy) {
	tel.Output, tel.OutputPolicy = runnersdb.New(inst.DB), ret
	ch.Outbox = msg.Outbox
}

// retentionFlags are the retention settings of recorded runner events (KIN-52): flags defaulting to KINDEV_*
// environment variables, then to domain.DefaultRetention.
type retentionFlags struct{ output, events, limitMB *string }

func addRetentionFlags(fset *flag.FlagSet, getenv func(string) string) retentionFlags {
	def := runners.DefaultRetention()
	or := func(name string, v int64) string {
		if s := getenv(name); s != "" {
			return s
		}
		return strconv.FormatInt(v, 10)
	}
	return retentionFlags{
		output: fset.String("runner-output-retention-days", or("KINDEV_RUNNER_OUTPUT_RETENTION_DAYS", int64(def.Output/runners.Day)),
			"days runner output is kept after its execution finished"),
		events: fset.String("runner-event-retention-days", or("KINDEV_RUNNER_EVENT_RETENTION_DAYS", int64(def.Events/runners.Day)),
			"days other runner events are kept after their execution finished (at least the output retention)"),
		limitMB: fset.String("runner-output-limit-mb", or("KINDEV_RUNNER_OUTPUT_LIMIT_MB", def.MaxOutputBytes>>20),
			"MiB of runner output stored in total; beyond it the oldest finished output is evicted, then new output is dropped"),
	}
}

func (f retentionFlags) policy() (runners.RetentionPolicy, error) {
	num := func(flag, s string, max int64) (int64, error) {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 || n > max {
			return 0, fmt.Errorf("--%s: %q is not a whole number from 1 to %d", flag, s, max)
		}
		return n, nil
	}
	const maxDays, maxMB = 3650, 1 << 20 // ten years, 1 TiB
	out, err := num("runner-output-retention-days", *f.output, maxDays)
	if err != nil {
		return runners.RetentionPolicy{}, err
	}
	ev, err := num("runner-event-retention-days", *f.events, maxDays)
	if err != nil {
		return runners.RetentionPolicy{}, err
	}
	mb, err := num("runner-output-limit-mb", *f.limitMB, maxMB)
	if err != nil {
		return runners.RetentionPolicy{}, err
	}
	p := runners.RetentionPolicy{Output: time.Duration(out) * runners.Day, Events: time.Duration(ev) * runners.Day,
		MaxOutputBytes: mb << 20}
	return p, p.Validate()
}

// retentionPause lets other writers in between retention batches.
const retentionPause = 10 * time.Millisecond

// startRetention purges recorded runner events by the policy at start and hourly until ctx ends; the returned
// stop waits for the loop.
func startRetention(ctx context.Context, inst *bootstrap.Installation, wf *workflowapp.Service, p runners.RetentionPolicy,
	logger *slog.Logger) (stop func()) {
	r := &runnersapp.Retention{Repo: runnersdb.New(inst.DB), Steps: wf, Policy: p, Now: time.Now, Logger: logger,
		Pause: retentionPause}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()
	return func() { cancel(); <-done }
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

func (r runnerExecutions) StartAttempt(ctx context.Context, runnerID, step string, fence int64, at time.Time, agent string) error {
	return attemptError(r.wf.StartStepAt(ctx, step, runnerID, fence, at, agent))
}

func (r runnerExecutions) CompleteAttempt(ctx context.Context, runnerID, step string, fence int64, at time.Time) error {
	return attemptError(r.wf.CompleteStepAt(ctx, step, runnerID, fence, at))
}

func (r runnerExecutions) FailAttempt(ctx context.Context, runnerID, step string, fence int64, reason string, at time.Time) error {
	return attemptError(r.wf.FailStepAt(ctx, step, runnerID, fence, reason, at))
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
