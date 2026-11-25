package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Alert hooks (KIN-60): a task entering or leaving BLOCKED, for the Alert Center; a runner whose strict egress
// keeps failing (KIN-81: network holds in a row, raised as a failed egress check of that runner).
const (
	AdapterAlerts          = "alerts"
	TopicTaskBlocked       = "alerts.task_blocked"
	TopicTaskUnblocked     = "alerts.task_unblocked"
	TopicEgressCheckFailed = "alerts.egress_check_failed"
)

// CodeNetworkHoldRepeated is the egress alert's code for a step held for the network domain.NetworkAlertAfter
// times or more in a row on one runner.
const CodeNetworkHoldRepeated = "network_hold_repeated"

// alertNetworkHolds raises the runner's egress alert once a step's network holds in a row on it reached
// domain.NetworkAlertAfter (each further hold raises it again: the Alert Center groups them per runner).
func (s *Service) alertNetworkHolds(ctx context.Context, st *domain.Step, runnerID string) error {
	if s.Outbox == nil || st == nil || st.NetworkHolds < domain.NetworkAlertAfter {
		return nil
	}
	b, err := json.Marshal(map[string]string{"runnerId": runnerID, "code": CodeNetworkHoldRepeated})
	if err != nil {
		return err
	}
	key := "step/" + st.ID + "/network_hold/" + strconv.FormatInt(st.Fence, 10)
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: TopicEgressCheckFailed, Key: key,
		Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", TopicEgressCheckFailed, err)
	}
	return nil
}

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
	// The aggregate version (incremented by every save) makes each transition's event unique.
	key := "task/" + t.ID + "/" + topic + "/v" + strconv.FormatInt(t.Version, 10)
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}
