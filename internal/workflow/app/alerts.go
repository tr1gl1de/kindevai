package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Alert hooks (KIN-60): a task entering or leaving BLOCKED, for the Alert Center.
const (
	AdapterAlerts      = "alerts"
	TopicTaskBlocked   = "alerts.task_blocked"
	TopicTaskUnblocked = "alerts.task_unblocked"
)

// Outbox stores outgoing messages in the caller's transaction.
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// saveTask saves t and, with an Outbox, tells the Alert Center when it entered or left BLOCKED since it was stored.
func (s *Service) saveTask(ctx context.Context, t *domain.Task) error {
	if s.Outbox == nil {
		return s.Repo.SaveTask(ctx, t)
	}
	stored, err := s.Repo.Task(ctx, t.ID)
	if err != nil {
		return err
	}
	if err := s.Repo.SaveTask(ctx, t); err != nil {
		return err
	}
	was, is := stored.State == domain.TaskBlocked, t.State == domain.TaskBlocked
	if was == is {
		return nil
	}
	topic, body := TopicTaskUnblocked, map[string]string{"projectId": t.ProjectID, "taskId": t.ID, "state": string(t.State)}
	if is {
		topic, body["reason"] = TopicTaskBlocked, string(t.BlockReason)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	key := "task/" + t.ID + "/" + topic + "/" + strconv.FormatInt(t.UpdatedAt.UnixNano(), 10)
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}
