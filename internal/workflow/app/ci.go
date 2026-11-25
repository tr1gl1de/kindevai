package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The CI fix loop and the policy-gated merge into the integration branch (KIN-37, domain/ci.go). CI results reach
// the execution in two ways, both through the outbox (handlers call GitHub, never the webhook transaction):
//
//   - CIEvent: a completed check suite on a branch of the change set (the GitHub webhook handler, in the inbox
//     transaction) queues a CI check of that artifact; such CI is the execution's, not the Alert Center's (KIN-60).
//   - ReconcileCI: every ReconcileCIEvery the open, unmerged artifacts of VERIFYING tasks (and of tasks BLOCKED on CI)
//     get a CI check too, so a lost webhook, an Owner's later integrate_dev approval or a green re-run is picked up.
//
// HandleCICheck reads the status of all checks on the artifact's head: failed starts a CI fix round (or blocks the
// task once the budget is spent), passed queues the merge gate (HandleMerge).

// Outbox topics of the CI loop and the merge.
const (
	TopicCICheck = "workflow.ci_check"
	TopicMerge   = "workflow.merge"
)

// Audit kinds of the CI loop and the merge.
const (
	KindCIFailed          = "workflow.ci_failed"
	KindCIFixStarted      = "workflow.ci_fix_started"
	KindCIBudgetExhausted = "workflow.ci_budget_exhausted"
	KindMergeHeld         = "workflow.merge_held"
	KindMerged            = "workflow.merged"
	KindMergeRefused      = "workflow.merge_refused"
	KindMergeFailed       = "workflow.merge_failed"
	KindCIMissing         = "workflow.ci_missing"    // KIN-105: no check run on the head within NoCIBlockAfter
	KindCIResumed         = "workflow.ci_resumed"    // KIN-105: the Owner resumed a task blocked on CI
	KindMergeRetried      = "workflow.merge_retried" // KIN-105: the Owner retried a refused merge
)

// ReconcileCIEvery is the period of the CI reconciliation; reconcileBatch bounds the artifacts checked per pass.
const (
	ReconcileCIEvery = time.Minute
	reconcileBatch   = 1000
)

// ErrHeadMoved: the pull request's head is no longer the commit the platform published and CI checked; it is not
// merged (someone pushed to the branch, or GitHub answered 409).
var ErrHeadMoved = errors.New("the pull request's head moved away from the checked commit")

// CIStatus is the combined status of all checks on a commit; Detail names the failed ones (untrusted text from
// GitHub, bounded by the adapter). NoChecks: pending because the commit has no check run at all (KIN-105).
type CIStatus struct {
	State    domain.CIState
	Detail   string
	NoChecks bool
}

// MergeRequest asks to merge pull request Number of the project's Repository into Base with a merge commit, only
// while its head is HeadSHA.
type MergeRequest struct {
	ProjectID  string
	Repository string
	Number     int
	HeadSHA    string
	Base       string
}

// CIChecks is the integrations context's GitHub App as the CI loop needs it. MergePullRequest returns the merge
// commit; ErrHeadMoved when the head is not HeadSHA; domain.ErrProtectedBranch for a main/master base; errors
// marked messaging.Permanent cannot be fixed by retrying (not mergeable, repository gone).
type CIChecks interface {
	CIStatus(ctx context.Context, projectID, repository, headSHA string) (CIStatus, error)
	MergePullRequest(ctx context.Context, r MergeRequest) (string, error)
}

// IntegrateGate says whether the task's current plan revision may be merged into dev: integrate_dev granted by the
// policy (L0/L1) or approved by the Owner (L2/L3) (approvals context).
type IntegrateGate interface {
	IntegrateDevApproved(ctx context.Context, taskID string) (bool, error)
}

type artifactRef struct {
	ExecutionID string `json:"executionId"`
	Repository  string `json:"repository"`
	Branch      string `json:"branch"`
}

type mergePayload struct {
	ExecutionID string `json:"executionId"`
}

func hashKey(prefix string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return prefix + hex.EncodeToString(h.Sum(nil))
}

func (s *Service) enqueue(ctx context.Context, topic, key string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterGitHub, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}

// CIEvent takes a completed CI run on branch of repository (a GitHub webhook, in the inbox transaction). It reports
// whether the branch is one the platform published (then its CI belongs to the execution, not the Alert Center) and
// queues a CI check of its open, unmerged artifacts.
func (s *Service) CIEvent(ctx context.Context, repository, branch, headSHA, delivery string) (bool, error) {
	if s.ChangeSets == nil || s.Outbox == nil || branch == "" {
		return false, nil
	}
	arts, err := s.ChangeSets.BranchArtifacts(ctx, repository, branch)
	if err != nil {
		return false, err
	}
	for _, a := range arts {
		if a.State != domain.ArtifactOpen || a.Merge == domain.MergeMerged {
			continue
		}
		key := hashKey("ci-check/", delivery, a.ExecutionID, a.Repository, a.Branch, headSHA)
		if err := s.enqueue(ctx, TopicCICheck, key, artifactRef{a.ExecutionID, a.Repository, a.Branch}); err != nil {
			return true, err
		}
	}
	return len(arts) > 0, nil
}

// ReconcileCI queues a CI check of every open, unmerged artifact of a VERIFYING task or of a task BLOCKED on CI
// (one per artifact and ReconcileCIEvery window).
func (s *Service) ReconcileCI(ctx context.Context) error {
	if s.ChangeSets == nil || s.Outbox == nil || s.CI == nil {
		return nil
	}
	window := strconv.FormatInt(s.now().Unix()/int64(ReconcileCIEvery/time.Second), 10)
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		arts, err := s.ChangeSets.VerifyingArtifacts(ctx, reconcileBatch)
		if err != nil {
			return err
		}
		for _, a := range arts {
			key := hashKey("ci-check/", "reconcile", window, a.ExecutionID, a.Repository, a.Branch, a.HeadSHA)
			if err := s.enqueue(ctx, TopicCICheck, key, artifactRef{a.ExecutionID, a.Repository, a.Branch}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ciRelevant reports a task whose CI the loop follows: VERIFYING, or BLOCKED on CI (a green re-run resumes it).
func ciRelevant(t domain.Task) bool {
	return t.State == domain.TaskVerifying || t.State == domain.TaskBlocked && t.BlockReason == domain.WaitCI
}

// HandleCICheck is the outbox handler of TopicCICheck: it reads the combined CI status of the artifact's head and
// acts on it (fix round, block, merge gate). Results of a head that is no longer the artifact's are dropped.
func (s *Service) HandleCICheck(ctx context.Context, d messaging.Delivery) error {
	var p artifactRef
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.ExecutionID == "" {
		return messaging.Permanent(errors.New("malformed CI check payload"))
	}
	if s.ChangeSets == nil || s.CI == nil {
		return errors.New("the CI loop is not configured")
	}
	var a domain.GitArtifact
	var task domain.Task
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if a, err = s.ChangeSets.Artifact(ctx, p.ExecutionID, p.Repository, p.Branch); err != nil {
			return err
		}
		task, err = s.Repo.Task(ctx, a.TaskID)
		return err
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil // gone (task deleted)
	}
	if err != nil {
		return err
	}
	if a.State != domain.ArtifactOpen || a.Merge == domain.MergeMerged || !ciRelevant(task) {
		return nil
	}
	st, err := s.CI.CIStatus(ctx, task.ProjectID, a.Repository, a.HeadSHA)
	if err != nil {
		return err
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		cur, err := s.ChangeSets.Artifact(ctx, p.ExecutionID, p.Repository, p.Branch)
		if err != nil {
			return err
		}
		t, err := s.Repo.Task(ctx, cur.TaskID)
		if err != nil {
			return err
		}
		if cur.HeadSHA != a.HeadSHA || !ciRelevant(t) {
			return nil // a fix round pushed a new head meanwhile, or the task moved on
		}
		now := s.now()
		var changed bool
		if st.NoChecks && st.State == domain.CIPending {
			changed = cur.RecordNoChecks(a.HeadSHA, now)
		} else {
			changed = cur.RecordCI(a.HeadSHA, st.State, st.Detail, now)
		}
		if st.State == domain.CIPassed && cur.Merge == domain.MergeFailed && t.State == domain.TaskVerifying {
			cur.Merge, cur.MergeError, changed = "", "", true // the Owner resumed the task: try the merge again
		}
		if changed {
			if err := s.ChangeSets.SaveArtifact(ctx, cur); err != nil {
				return err
			}
		}
		switch st.State {
		case domain.CIFailed:
			if t.State != domain.TaskVerifying {
				return nil // a fix round runs, or the Owner has it (BLOCKED)
			}
			// A new failure, or one still there after the Owner resumed the task: a fix round, or blocked again
			// once the rounds are used up (a fix round makes the task RUNNING, so this never repeats one).
			return s.ciFailed(ctx, &t, cur, now)
		case domain.CIPassed:
			if t.State == domain.TaskBlocked {
				if cur.Merge == domain.MergeFailed {
					return nil // a refused merge waits for the Owner
				}
				if err := t.Resume(now); err != nil {
					return err
				}
				if err := s.saveTask(ctx, &t); err != nil {
					return err
				}
			}
			return s.enqueue(ctx, TopicMerge, hashKey("merge/", d.Key), mergePayload{cur.ExecutionID})
		case domain.CIPending:
			if t.State != domain.TaskVerifying || cur.NoChecksFor(now) < domain.NoCIBlockAfter {
				return nil
			}
			// A repository without CI: nothing is merged without a green check, the Owner decides.
			why := fmt.Sprintf("no CI check runs on %s (%s) for %s: the repository seems to have no CI, and nothing is "+
				"merged without it; add CI and resume, or merge the pull request yourself", cur.Branch, cur.HeadSHA,
				domain.NoCIBlockAfter)
			if err := s.blockOnCI(ctx, &t, why); err != nil {
				return err
			}
			return s.record(ctx, mergeEntry(t, cur, KindCIMissing, why))
		}
		return nil
	})
}

// ciFailed starts a CI fix round of the artifact's step in its execution (task VERIFYING → RUNNING), or blocks the
// task once the CI fix rounds are used up.
func (s *Service) ciFailed(ctx context.Context, t *domain.Task, a domain.GitArtifact, now time.Time) error {
	entry := auditapp.Entry{Actor: auditapp.System(), ProjectID: t.ProjectID, Subject: auditSubjectTaskPrefix + t.ID,
		Kind: KindCIFailed, Details: map[string]string{"repository": a.Repository, "branch": a.Branch, "head": a.HeadSHA,
			"pullRequest": strconv.Itoa(a.PRNumber), "checks": a.CIDetail}}
	if err := s.record(ctx, entry); err != nil {
		return err
	}
	e, err := s.Repo.Execution(ctx, a.ExecutionID)
	if err != nil {
		return err
	}
	if e.PlanRevision != t.PlanRevision || e.State != domain.ExecutionSucceeded {
		return nil // not the task's current run
	}
	feedback := fmt.Sprintf("Pull request #%d (%s, commit %s): %s", a.PRNumber, a.Branch, a.HeadSHA, a.CIDetail)
	var exhausted bool
	err = s.applyExecution(ctx, e, func(_ context.Context, e *domain.Execution, now time.Time) error {
		err := e.ReopenForCI(a.StepID, feedback, now)
		if errors.Is(err, domain.ErrCIBudgetExhausted) {
			exhausted = true
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if exhausted {
		msg := fmt.Sprintf("CI still fails on %s after %d CI fix rounds: %s", a.Branch, domain.MaxCIFixRounds, a.CIDetail)
		if err := t.Transition(domain.TaskBlocked, domain.Reason{Code: domain.WaitCI, Detail: cutRunes(msg, domain.MaxReasonLen)}, now); err != nil {
			return err
		}
		if err := s.saveTask(ctx, t); err != nil {
			return err
		}
		entry.Kind = KindCIBudgetExhausted
		return s.record(ctx, entry)
	}
	if err := t.Transition(domain.TaskRunning, domain.Reason{}, now); err != nil {
		return err
	}
	if err := s.saveTask(ctx, t); err != nil {
		return err
	}
	entry.Kind = KindCIFixStarted
	entry.Details["round"] = strconv.Itoa(e.Step(a.StepID).CIRound + 1) // e is the execution before the reopen
	return s.record(ctx, entry)
}

// HandleMerge is the outbox handler of TopicMerge: the merge gate of an execution's change set. It merges the next
// artifact in order when its CI passed on its head, its reviewed head is the published one and integrate_dev is
// allowed; then queues the next one, and completes the task once all are merged. A refused merge (head moved, not
// mergeable, main/master) is recorded, audited and blocks the task (reason ci); nothing is merged blindly.
func (s *Service) HandleMerge(ctx context.Context, d messaging.Delivery) error {
	var p mergePayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.ExecutionID == "" {
		return messaging.Permanent(errors.New("malformed merge payload"))
	}
	if s.ChangeSets == nil || s.CI == nil || s.Integrate == nil {
		return errors.New("the merge gate is not configured")
	}
	var req MergeRequest
	var target domain.GitArtifact
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		e, t, arts, err := s.mergeScope(ctx, p.ExecutionID)
		if err != nil || t.State != domain.TaskVerifying || !currentRun(e, t) {
			return err
		}
		next, stop := domain.NextMerge(arts)
		if stop != "" {
			return s.blockOnCI(ctx, &t, stop)
		}
		if next == nil {
			return nil
		}
		now := s.now()
		why := ""
		if ok, err := s.Integrate.IntegrateDevApproved(ctx, t.ID); err != nil {
			return err
		} else if !ok {
			why = fmt.Sprintf("integrate_dev is not approved: the Owner approves the merge of this plan revision into %s", next.PRBase)
		}
		if pub, ok := e.Step(next.StepID).Published(); !ok || pub.HeadSHA != next.HeadSHA {
			why = "the pull request's head is not the reviewed and published commit"
		}
		if why != "" {
			if !next.MergeHeld(why, now) {
				return nil
			}
			if err := s.ChangeSets.SaveArtifact(ctx, *next); err != nil {
				return err
			}
			return s.record(ctx, mergeEntry(t, *next, KindMergeHeld, why))
		}
		target = *next
		req = MergeRequest{ProjectID: t.ProjectID, Repository: next.Repository, Number: next.PRNumber,
			HeadSHA: next.HeadSHA, Base: next.PRBase}
		return nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil || req.Number == 0 {
		return err
	}
	sha, err := s.CI.MergePullRequest(ctx, req)
	refused := errors.Is(err, domain.ErrProtectedBranch)
	if err != nil && !refused && !errors.Is(err, ErrHeadMoved) && !messaging.IsPermanent(err) {
		return err // transient: the outbox retries
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		e, t, arts, serr := s.mergeScope(ctx, p.ExecutionID)
		if serr != nil {
			return serr
		}
		i := indexArtifact(arts, target)
		if i < 0 || arts[i].Merge == domain.MergeMerged {
			return nil
		}
		if arts[i].HeadSHA != target.HeadSHA {
			if err != nil {
				return nil
			}
			// The change set moved on while GitHub merged the checked head: keep the record of what was merged.
			entry := mergeEntry(t, target, KindMerged, "merged while the change set moved to "+arts[i].HeadSHA)
			entry.Details["mergeCommit"] = sha
			return s.record(ctx, entry)
		}
		a, now := &arts[i], s.now()
		if err != nil {
			kind := KindMergeFailed
			if refused {
				kind = KindMergeRefused
			}
			a.MergeFailed(err.Error(), now)
			if serr := s.ChangeSets.SaveArtifact(ctx, *a); serr != nil {
				return serr
			}
			if serr := s.record(ctx, mergeEntry(t, *a, kind, a.MergeError)); serr != nil {
				return serr
			}
			if !currentRun(e, t) {
				return nil
			}
			return s.blockOnCI(ctx, &t, fmt.Sprintf("the merge of %s (%s) failed: %s", a.Repository, a.Branch, a.MergeError))
		}
		a.Merged(sha, now)
		if serr := s.ChangeSets.SaveArtifact(ctx, *a); serr != nil {
			return serr
		}
		entry := mergeEntry(t, *a, KindMerged, "")
		entry.Details["mergeCommit"] = a.MergeSHA
		if serr := s.record(ctx, entry); serr != nil {
			return serr
		}
		if !domain.AllMerged(arts) {
			return s.enqueue(ctx, TopicMerge, hashKey("merge/", "after", e.ID, a.Repository, a.Branch, a.HeadSHA),
				mergePayload{e.ID})
		}
		if t.State != domain.TaskVerifying || !currentRun(e, t) {
			return nil
		}
		from := t.State
		if serr := t.Transition(domain.TaskCompleted, domain.Reason{}, now); serr != nil {
			return serr
		}
		if serr := s.saveTask(ctx, &t); serr != nil {
			return serr
		}
		return s.finished(ctx, from, t)
	})
}

// mergeScope loads the execution, its task and its change set (in order) for the merge gate.
func (s *Service) mergeScope(ctx context.Context, executionID string) (domain.Execution, domain.Task, []domain.GitArtifact, error) {
	e, err := s.Repo.Execution(ctx, executionID)
	if err != nil {
		return e, domain.Task{}, nil, err
	}
	t, err := s.Repo.Task(ctx, e.TaskID)
	if err != nil {
		return e, t, nil, err
	}
	all, err := s.ChangeSets.TaskArtifacts(ctx, t.ID)
	if err != nil {
		return e, t, nil, err
	}
	var arts []domain.GitArtifact
	for _, a := range all {
		if a.ExecutionID == e.ID {
			arts = append(arts, a)
		}
	}
	return e, t, arts, nil
}

// currentRun reports the task's finished current run: only its change set may be merged.
func currentRun(e domain.Execution, t domain.Task) bool {
	return e.State == domain.ExecutionSucceeded && e.PlanRevision == t.PlanRevision
}

func indexArtifact(arts []domain.GitArtifact, a domain.GitArtifact) int {
	for i, x := range arts {
		if x.Repository == a.Repository && x.Branch == a.Branch {
			return i
		}
	}
	return -1
}

// blockOnCI blocks a VERIFYING task for the Owner (reason ci); an alert follows from saveTask.
func (s *Service) blockOnCI(ctx context.Context, t *domain.Task, why string) error {
	if t.State != domain.TaskVerifying {
		return nil
	}
	if err := t.Transition(domain.TaskBlocked, domain.Reason{Code: domain.WaitCI, Detail: cutRunes(why, domain.MaxReasonLen)}, s.now()); err != nil {
		return err
	}
	return s.saveTask(ctx, t)
}

func mergeEntry(t domain.Task, a domain.GitArtifact, kind, why string) auditapp.Entry {
	d := map[string]string{"repository": a.Repository, "branch": a.Branch, "head": a.HeadSHA,
		"pullRequest": strconv.Itoa(a.PRNumber), "base": a.PRBase}
	if why != "" {
		d["reason"] = why
	}
	return auditapp.Entry{Actor: auditapp.System(), Kind: kind, ProjectID: t.ProjectID,
		Subject: auditSubjectTaskPrefix + t.ID, Details: d}
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
