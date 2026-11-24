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
		DedupKey: StorageDedupKey(p.MachineID, p.Role), Subject: subject, Details: map[string]string{
			"machineId": p.MachineID, "role": p.Role, "level": p.Level,
			"usedPercent":    strconv.FormatFloat(p.UsedPercent, 'f', 0, 64),
			"availableBytes": strconv.FormatInt(p.AvailableBytes, 10)}}))
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

// StorageDedupKey groups a volume's level alerts until the volume recovers.
func StorageDedupKey(machineID, role string) string { return "storage/" + machineID + "/" + role }

// StorageRecovered handles alerts.storage_recovered {machineId, role}: the volume fell back below the recovery
// threshold (KIN-60 hysteresis), so its level alert resolves itself.
func (h Handlers) StorageRecovered(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		MachineID string `json:"machineId"`
		Role      string `json:"role"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.MachineID == "" || p.Role == "" {
		return messaging.Permanent(errors.New("storage_recovered: no volume"))
	}
	return h.Svc.ResolveKey(ctx, StorageDedupKey(p.MachineID, p.Role))
}

// EgressDedupKey groups a runner's failed egress checks until a check passes.
func EgressDedupKey(runnerID string) string { return "runner/" + runnerID + "/egress" }

// EgressCheckFailed handles alerts.egress_check_failed {runnerId, testId, code}: a platform alert.
func (h Handlers) EgressCheckFailed(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		RunnerID string `json:"runnerId"`
		TestID   string `json:"testId"`
		Code     string `json:"code"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.RunnerID == "" {
		return messaging.Permanent(errors.New("egress_check_failed: no runnerId"))
	}
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{Kind: domain.KindEgressCheckFailed,
		Severity: domain.SeverityWarning, DedupKey: EgressDedupKey(p.RunnerID), Subject: p.RunnerID,
		Details: map[string]string{"runnerId": p.RunnerID, "testId": p.TestID, "code": clip(p.Code)}}))
}

// EgressCheckPassed handles alerts.egress_check_passed {runnerId}.
func (h Handlers) EgressCheckPassed(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		RunnerID string `json:"runnerId"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.RunnerID == "" {
		return messaging.Permanent(errors.New("egress_check_passed: no runnerId"))
	}
	return h.Svc.ResolveKey(ctx, EgressDedupKey(p.RunnerID))
}

// ciEvent is the payload of alerts.ci_failed / alerts.ci_passed.
type ciEvent struct {
	ProjectID   string `json:"projectId"`
	Repository  string `json:"repository"` // owner/name
	Branch      string `json:"branch"`
	PullRequest int    `json:"pullRequest"` // 0: a run on the branch itself
	HeadSHA     string `json:"headSha"`
	Conclusion  string `json:"conclusion"`
}

func (e ciEvent) valid() bool {
	return e.ProjectID != "" && e.Repository != "" && (e.PullRequest > 0 || e.Branch != "")
}

// key groups CI failures per repository and PR, or per repository and branch for a run outside a PR.
func (e ciEvent) key() string {
	if e.PullRequest > 0 {
		return "ci/" + e.Repository + "/pr/" + strconv.Itoa(e.PullRequest)
	}
	return "ci/" + e.Repository + "/branch/" + e.Branch
}

func (e ciEvent) subject() string {
	if e.PullRequest > 0 {
		return e.Repository + "#" + strconv.Itoa(e.PullRequest)
	}
	return e.Repository + "@" + e.Branch
}

// CIFailed handles alerts.ci_failed: a project alert (Alert First: nothing else happens, no task is created).
func (h Handlers) CIFailed(ctx context.Context, d messaging.Delivery) error {
	var p ciEvent
	if err := decode(d, &p); err != nil {
		return err
	}
	if !p.valid() {
		return messaging.Permanent(errors.New("ci_failed: no project, repository or ref"))
	}
	det := map[string]string{"repository": clip(p.Repository), "branch": clip(p.Branch), "headSha": clip(p.HeadSHA),
		"conclusion": clip(p.Conclusion)}
	if p.PullRequest > 0 {
		det["pullRequest"] = strconv.Itoa(p.PullRequest)
	}
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{ProjectID: p.ProjectID, Kind: domain.KindCIFailed,
		Severity: domain.SeverityWarning, DedupKey: clip(p.key()), Subject: clip(p.subject()), Details: det}))
}

// CIPassed handles alerts.ci_passed: CI is green again on that branch or PR.
func (h Handlers) CIPassed(ctx context.Context, d messaging.Delivery) error {
	var p ciEvent
	if err := decode(d, &p); err != nil {
		return err
	}
	if !p.valid() {
		return messaging.Permanent(errors.New("ci_passed: no project, repository or ref"))
	}
	return h.Svc.ResolveKey(ctx, clip(p.key()))
}

// CircuitDedupKey groups an integration adapter's circuit alerts until it works again.
func CircuitDedupKey(adapter string) string { return "integration/" + adapter + "/circuit" }

// CircuitOpen handles alerts.integration_circuit_open {adapter, failures}: a platform alert.
func (h Handlers) CircuitOpen(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		Adapter  string `json:"adapter"`
		Failures int    `json:"failures"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.Adapter == "" {
		return messaging.Permanent(errors.New("integration_circuit_open: no adapter"))
	}
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{Kind: domain.KindIntegrationCircuitOpen,
		Severity: domain.SeverityWarning, DedupKey: CircuitDedupKey(p.Adapter), Subject: p.Adapter,
		Details: map[string]string{"adapter": p.Adapter, "failures": strconv.Itoa(p.Failures)}}))
}

// CircuitClosed handles alerts.integration_recovered {adapter}.
func (h Handlers) CircuitClosed(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		Adapter string `json:"adapter"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.Adapter == "" {
		return messaging.Permanent(errors.New("integration_recovered: no adapter"))
	}
	return h.Svc.ResolveKey(ctx, CircuitDedupKey(p.Adapter))
}

// TaskDedupKey groups a task's BLOCKED alerts until it leaves BLOCKED.
func TaskDedupKey(taskID string) string { return "task/" + taskID + "/blocked" }

// TaskBlocked handles alerts.task_blocked {projectId, taskId, reason}: a project alert.
func (h Handlers) TaskBlocked(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		ProjectID string `json:"projectId"`
		TaskID    string `json:"taskId"`
		Reason    string `json:"reason"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.ProjectID == "" || p.TaskID == "" {
		return messaging.Permanent(errors.New("task_blocked: no project or task"))
	}
	return permanent(h.Svc.Raise(ctx, d.Key, domain.Occurrence{ProjectID: p.ProjectID, Kind: domain.KindTaskBlocked,
		Severity: domain.SeverityWarning, DedupKey: TaskDedupKey(p.TaskID), Subject: p.TaskID,
		Details: map[string]string{"taskId": p.TaskID, "reason": clip(p.Reason)}}))
}

// TaskUnblocked handles alerts.task_unblocked {taskId}: the task left BLOCKED (resumed, failed or cancelled).
func (h Handlers) TaskUnblocked(ctx context.Context, d messaging.Delivery) error {
	var p struct {
		TaskID string `json:"taskId"`
	}
	if err := decode(d, &p); err != nil {
		return err
	}
	if p.TaskID == "" {
		return messaging.Permanent(errors.New("task_unblocked: no taskId"))
	}
	return h.Svc.ResolveKey(ctx, TaskDedupKey(p.TaskID))
}

// clip bounds a free-form value from a monitor to what an alert stores (domain.Occurrence rejects longer values).
func clip(s string) string {
	const maxBytes = 200
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	for len(s) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}
