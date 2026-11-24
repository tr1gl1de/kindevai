package app

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type alertOutbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (o *alertOutbox) Enqueue(_ context.Context, m messaging.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if slices.ContainsFunc(o.msgs, func(x messaging.Message) bool { return x.Key == m.Key }) {
		return nil
	}
	o.msgs = append(o.msgs, m)
	return nil
}

// KIN-60 AC: a task entering BLOCKED raises a project alert event; leaving it (resumed or closed) resolves it.
func TestBlockedTaskRaisesAndResolvesAlert(t *testing.T) {
	t.Parallel()
	e := newEnv()
	out := &alertOutbox{}
	e.svc.Outbox = out
	ctx := t.Context()
	a, _ := e.svc.CreateTask(ctx, "p1", "Login", "", "u1")
	b, _ := e.svc.CreateTask(ctx, "p1", "Logout", "", "u1")
	if _, err := e.svc.StartAnalysis(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	steps := []func() (domain.Task, error){
		func() (domain.Task, error) { return e.svc.BlockTask(ctx, a.ID, domain.WaitNetwork, "proxy down") },
		func() (domain.Task, error) { return e.svc.ResumeTask(ctx, a.ID) },
		func() (domain.Task, error) { return e.svc.BlockTask(ctx, a.ID, domain.WaitRunner, "") },
		func() (domain.Task, error) { return e.svc.CancelTask(ctx, a.ID, "no longer needed") },
		func() (domain.Task, error) { return e.svc.CancelTask(ctx, b.ID, "never blocked") },
	}
	for i, step := range steps {
		e.clock.Add(time.Second)
		if _, err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	var topics []string
	for _, m := range out.msgs {
		topics = append(topics, m.Topic)
	}
	want := []string{TopicTaskBlocked, TopicTaskUnblocked, TopicTaskBlocked, TopicTaskUnblocked}
	if !slices.Equal(topics, want) {
		t.Fatalf("topics = %v, want %v", topics, want)
	}
	var p struct{ ProjectID, TaskID, Reason string }
	if err := json.Unmarshal(out.msgs[0].Payload, &p); err != nil || p.ProjectID != "p1" || p.TaskID != a.ID ||
		p.Reason != string(domain.WaitNetwork) || out.msgs[0].Adapter != "alerts" {
		t.Fatalf("blocked event %s", out.msgs[0].Payload)
	}
}
