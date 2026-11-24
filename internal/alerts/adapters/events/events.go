// Package events turns the monitors' and approvals' outbox events (KIN-20, KIN-24) into Alert Center use cases.
// The producers enqueue them with adapter "alerts" / "telegram"; the Control Plane registers these handlers for
// their topics.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// Handlers are the outbox handlers of the Alert Center.
type Handlers struct{ Svc *app.Service }

// decode parses a payload; a malformed one can never succeed, so it is permanent.
func decode(d messaging.Delivery, v any) error {
	if err := json.Unmarshal(d.Payload, v); err != nil {
		return messaging.Permanent(fmt.Errorf("%s payload: %w", d.Topic, err))
	}
	return nil
}

// permanent marks validation failures (a bad event stays bad) as permanent.
func permanent(err error) error {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		return messaging.Permanent(err)
	}
	return err
}

// RunnerDedupKey groups a runner's unreachable alerts until it recovers.
func RunnerDedupKey(runnerID string) string { return "runner/" + runnerID + "/unreachable" }

// RunnerUnreachable handles alerts.runner_unreachable {runnerId, since, missedHeartbeats}.
func (h Handlers) RunnerUnreachable(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		RunnerID string `json:"runnerId"`
		Since    string `json:"since"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.RunnerID == "" {
		return messaging.Permanent(errors.New("runner_unreachable: no runnerId"))
	}
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{Kind: domain.KindRunnerUnreachable,
		Severity: domain.SeverityWarning, DedupKey: RunnerDedupKey(p.RunnerID), Subject: p.RunnerID,
		Details: map[string]string{"runnerId": p.RunnerID, "since": p.Since}}))
}

// RunnerRecovered handles alerts.runner_recovered {runnerId}: the runner's unreachable alert resolves itself.
func (h Handlers) RunnerRecovered(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		RunnerID string `json:"runnerId"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.RunnerID == "" {
		return messaging.Permanent(errors.New("runner_recovered: no runnerId"))
	}
	return h.Svc.ResolveKey(ctx, RunnerDedupKey(p.RunnerID))
}

// storageSeverity maps the KIN-20 volume levels (≥75% warning, ≥85% high = cleanup recommended, ≥95% or < 2 GiB
// critical) onto alert severities, so each rise escalates the grouped alert.
var storageSeverity = map[string]domain.Severity{
	"warning":  domain.SeverityInfo,
	"high":     domain.SeverityWarning,
	"critical": domain.SeverityCritical,
}

// StorageLevel handles alerts.storage_level {machineId, role, level, usedPercent, availableBytes, totalBytes}.
func (h Handlers) StorageLevel(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		MachineID      string  `json:"machineId"`
		Role           string  `json:"role"`
		Level          string  `json:"level"`
		UsedPercent    float64 `json:"usedPercent"`
		AvailableBytes int64   `json:"availableBytes"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	sev, ok := storageSeverity[p.Level]
	if !ok || p.MachineID == "" || p.Role == "" {
		return messaging.Permanent(fmt.Errorf("storage_level: bad level %q or volume", p.Level))
	}
	subject := p.MachineID + "/" + p.Role
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{Kind: domain.KindStorageLevel, Severity: sev,
		DedupKey: "storage/" + subject, Subject: subject, Details: map[string]string{
			"machineId": p.MachineID, "role": p.Role, "level": p.Level,
			"usedPercent":    strconv.FormatFloat(p.UsedPercent, 'f', 0, 64),
			"availableBytes": strconv.FormatInt(p.AvailableBytes, 10)}}))
}

// runnerOutputKey groups the Control Plane's "runner output storage full" alerts until output is stored again.
const runnerOutputKey = "runner-output/full"

// RunnerOutputFull handles alerts.runner_output_full {limitBytes, runnerId, stepId} (KIN-59): output of running
// executions is dropped because the Control Plane's runner output storage is full.
func (h Handlers) RunnerOutputFull(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		LimitBytes int64  `json:"limitBytes"`
		RunnerID   string `json:"runnerId"`
		StepID     string `json:"stepId"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.LimitBytes <= 0 {
		return messaging.Permanent(errors.New("runner_output_full: no limitBytes"))
	}
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{Kind: domain.KindRunnerOutputFull,
		Severity: domain.SeverityWarning, DedupKey: runnerOutputKey, Subject: "control-plane", Details: map[string]string{
			"limitBytes": strconv.FormatInt(p.LimitBytes, 10), "limitMiB": strconv.FormatInt(p.LimitBytes>>20, 10),
			"runnerId": clipID(p.RunnerID), "stepId": clipID(p.StepID)}}))
}

// clipID keeps at most 128 bytes of the printable runes of an id the runner reported, so a malformed one never
// costs the alert.
func clipID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > 128 {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// RunnerOutputRecovered handles alerts.runner_output_recovered {}: output is stored again, the alert resolves.
func (h Handlers) RunnerOutputRecovered(ctx context.Context, _ messaging.Delivery) error {
	return h.Svc.ResolveKey(ctx, runnerOutputKey)
}

// ApprovalRequested handles telegram.approval_requested {approvalId, projectId, taskId, planRevision, scope,
// reason, risk}: the Owners are told in Telegram.
func (h Handlers) ApprovalRequested(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		ApprovalID string `json:"approvalId"`
		ProjectID  string `json:"projectId"`
		TaskID     string `json:"taskId"`
		Reason     string `json:"reason"`
		Risk       string `json:"risk"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	return permanent(h.Svc.NotifyApproval(ctx, app.ApprovalRequest{ApprovalID: p.ApprovalID, ProjectID: p.ProjectID,
		TaskID: p.TaskID, Risk: p.Risk, Reason: p.Reason}))
}

// TaskFinished handles telegram.task_finished {taskId, projectId, state, title, createdBy} (KIN-62): the
// task's audience is told in Telegram.
func (h Handlers) TaskFinished(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		TaskID    string `json:"taskId"`
		ProjectID string `json:"projectId"`
		State     string `json:"state"`
		Title     string `json:"title"`
		CreatedBy string `json:"createdBy"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	return permanent(h.Svc.NotifyTask(ctx, app.TaskFinished{TaskID: p.TaskID, ProjectID: p.ProjectID, State: p.State,
		Title: p.Title, CreatedBy: p.CreatedBy}))
}
