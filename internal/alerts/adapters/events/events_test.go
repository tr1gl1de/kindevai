package events_test

import (
	"testing"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/events"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// KIN-62: a task_finished event that can never succeed (malformed or for a state that is not announced) is a
// permanent failure, not a retry loop.
func TestTaskFinishedRejectsBadEventsPermanently(t *testing.T) {
	t.Parallel()
	h := events.Handlers{Svc: &app.Service{}}
	for _, payload := range []string{`{`, `{"taskId":"t1","projectId":"p1","state":"CANCELLED"}`, `{"projectId":"p1","state":"FAILED"}`} {
		err := h.TaskFinished(t.Context(), messaging.Delivery{Topic: "telegram.task_finished", Payload: []byte(payload)})
		if !messaging.IsPermanent(err) {
			t.Fatalf("%s: err = %v, want permanent", payload, err)
		}
	}
}
