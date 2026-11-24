// Package notify implements the workflow's TaskNotifier with the outbox (KIN-62): a task that enters COMPLETED or
// FAILED enqueues one telegram.task_finished event in the task's own transaction. The Alert Center handles it and
// fans it out to the task's audience; the event's key makes it idempotent per task and state.
package notify

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Outbox event of a finished task.
const (
	Adapter           = "telegram"
	TopicTaskFinished = "telegram.task_finished"
)

// Payload is the event: ids, the final state and the title (the handler clips it for the chat).
type Payload struct {
	TaskID    string `json:"taskId"`
	ProjectID string `json:"projectId"`
	State     string `json:"state"`
	Title     string `json:"title"`
	CreatedBy string `json:"createdBy"`
}

// Outbox stores outgoing messages in the caller's transaction.
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// Notifier is the workflow app's TaskNotifier.
type Notifier struct{ Outbox Outbox }

// TaskFinished enqueues the event; a task reaches a final state once, so the key never repeats for new news.
func (n Notifier) TaskFinished(ctx context.Context, t domain.Task) error {
	b, err := json.Marshal(Payload{TaskID: t.ID, ProjectID: t.ProjectID, State: string(t.State), Title: t.Title,
		CreatedBy: t.CreatedBy})
	if err != nil {
		return err
	}
	if err := n.Outbox.Enqueue(ctx, messaging.Message{Adapter: Adapter, Topic: TopicTaskFinished,
		Key: "task/" + t.ID + "/" + string(t.State), Payload: b}); err != nil {
		return fmt.Errorf("enqueueing task notification: %w", err)
	}
	return nil
}
