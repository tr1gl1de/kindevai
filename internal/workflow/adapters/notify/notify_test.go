package notify

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type fakeOutbox struct{ msgs []messaging.Message }

func (f *fakeOutbox) Enqueue(_ context.Context, m messaging.Message) error {
	f.msgs = append(f.msgs, m)
	return nil
}

func TestTaskFinishedEnqueuesOneEventPerStateChange(t *testing.T) {
	t.Parallel()
	ob := &fakeOutbox{}
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	task, err := domain.NewTask("t1", "p1", "Fix login", "KIN-7", "u1", now)
	if err != nil {
		t.Fatal(err)
	}
	task.State = domain.TaskFailed
	if err := (Notifier{Outbox: ob}).TaskFinished(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if len(ob.msgs) != 1 {
		t.Fatalf("messages = %d", len(ob.msgs))
	}
	m := ob.msgs[0]
	if m.Adapter != Adapter || m.Topic != TopicTaskFinished || m.Key != "task/t1/FAILED" {
		t.Fatalf("message = %s %s %s", m.Adapter, m.Topic, m.Key)
	}
	var p Payload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		t.Fatal(err)
	}
	want := Payload{TaskID: "t1", ProjectID: "p1", State: "FAILED", Title: "Fix login", CreatedBy: "u1"}
	if p != want {
		t.Fatalf("payload = %+v, want %+v", p, want)
	}
}
