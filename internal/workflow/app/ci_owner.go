package app

import (
	"context"
	"fmt"
	"strconv"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The Owner's actions on a task BLOCKED on CI (KIN-105): "Resume" returns it to VERIFYING and checks CI at once (a
// still-red CI blocks it again, a refused merge whose CI is green is merged, a repository without CI gets a new
// NoCIBlockAfter window); "Retry merge" clears a refused merge and checks CI again, so a green head goes through
// the merge gate again. Neither skips the gate:
// integrate_dev and a green CI on the reviewed head are still required.

// ResumeCIAs resumes a task BLOCKED on CI of a project p manages (Owner or project admin).
func (s *Service) ResumeCIAs(ctx context.Context, p auth.Principal, taskID string) (domain.Task, error) {
	return s.ownerCIAction(ctx, p, taskID, KindCIResumed, s.mayManage, func(ctx context.Context, t domain.Task, arts []domain.GitArtifact, now time.Time) (map[string]string, error) {
		for _, a := range arts {
			if a.RestartNoChecksWindow() {
				if err := s.ChangeSets.SaveArtifact(ctx, a); err != nil {
					return nil, err
				}
			}
		}
		return nil, s.checkCINow(ctx, t, arts, now)
	})
}

// RetryMergeAs clears the refused merge of a task BLOCKED on CI of a project p manages (Owner or project admin); CI
// is checked again at once and a green head goes through the merge gate again.
func (s *Service) RetryMergeAs(ctx context.Context, p auth.Principal, taskID string) (domain.Task, error) {
	return s.ownerCIAction(ctx, p, taskID, KindMergeRetried, s.mayManage, func(ctx context.Context, t domain.Task, arts []domain.GitArtifact, now time.Time) (map[string]string, error) {
		retried := false
		for _, a := range arts {
			if a.RetryMerge(now) {
				if err := s.ChangeSets.SaveArtifact(ctx, a); err != nil {
					return nil, err
				}
				retried = true
			}
		}
		if !retried {
			return nil, fmt.Errorf("task %s has no refused merge: retry merge: %w", t.ID, domain.ErrInvalidTransition)
		}
		return nil, s.checkCINow(ctx, t, arts, now)
	})
}

// MergeWithoutCIAs is the Owner's explicit "merge without CI" of a task BLOCKED on CI whose pull requests have no
// check run at all (KIN-113, owner decision 2026-11-26). Every open, unmerged pull request of the current run must
// have either green CI or no check run on its head (at least one without): red or running CI, or a refused merge,
// are refused. The waiver is bound to each head and holds only while it has no check run; the merge gate still
// requires integrate_dev and the reviewed, published head, and main/master stay refused. Approving a merge is the
// Owner's (KB KIN-A-21), so a project admin may not.
func (s *Service) MergeWithoutCIAs(ctx context.Context, p auth.Principal, taskID string) (domain.Task, error) {
	return s.ownerCIAction(ctx, p, taskID, KindMergedWithoutCI, s.mayApproveMerge, func(ctx context.Context, t domain.Task, arts []domain.GitArtifact, now time.Time) (map[string]string, error) {
		details := map[string]string{}
		executions := map[string]bool{}
		for _, a := range arts {
			switch {
			case a.Merge == domain.MergeFailed:
				return nil, fmt.Errorf("the merge of %s (%s) was refused: retry merge instead: %w", a.Repository,
					a.Branch, domain.ErrInvalidTransition)
			case a.CIPassedOnHead():
			default:
				if err := a.WaiveCI(now); err != nil {
					return nil, err
				}
				if err := s.ChangeSets.SaveArtifact(ctx, a); err != nil {
					return nil, err
				}
				details[a.Repository+" "+a.Branch] = a.HeadSHA
			}
			executions[a.ExecutionID] = true
		}
		if len(details) == 0 {
			return nil, fmt.Errorf("task %s has no pull request without CI checks: %w", t.ID, domain.ErrInvalidTransition)
		}
		for id := range executions {
			key := hashKey("merge/", "without-ci", t.ID, strconv.FormatInt(now.UnixNano(), 10), id)
			if err := s.enqueue(ctx, TopicMerge, key, mergePayload{id}); err != nil {
				return nil, err
			}
		}
		return details, nil
	})
}

// mayApproveMerge lets only the platform Owner approve a merge (KB KIN-A-21: approvals are the Owner's); others who
// see the project get ErrForbidden, others ErrNotFound.
func (s *Service) mayApproveMerge(ctx context.Context, p auth.Principal, projectID string) error {
	if err := s.mayManage(ctx, p, projectID); err != nil {
		return err
	}
	if !p.Owner {
		return ErrForbidden
	}
	return nil
}

// checkCINow queues a CI check of each artifact (HandleCICheck: red blocks again, green goes to the merge gate).
func (s *Service) checkCINow(ctx context.Context, t domain.Task, arts []domain.GitArtifact, now time.Time) error {
	for _, a := range arts {
		key := hashKey("ci-check/", "owner", t.ID, strconv.FormatInt(now.UnixNano(), 10), a.ExecutionID, a.Repository, a.Branch)
		if err := s.enqueue(ctx, TopicCICheck, key, artifactRef{a.ExecutionID, a.Repository, a.Branch}); err != nil {
			return err
		}
	}
	return nil
}

// ownerCIAction resumes a task BLOCKED on CI (to VERIFYING), runs act on the open, unmerged artifacts of its current
// run in the same transaction and audits kind as the principal's action.
func (s *Service) ownerCIAction(ctx context.Context, p auth.Principal, taskID, kind string,
	may func(context.Context, auth.Principal, string) error,
	act func(context.Context, domain.Task, []domain.GitArtifact, time.Time) (map[string]string, error)) (domain.Task, error) {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if err := may(ctx, p, t.ProjectID); err != nil {
		return domain.Task{}, err
	}
	if s.ChangeSets == nil || s.Outbox == nil {
		return domain.Task{}, fmt.Errorf("the CI loop is not configured: %w", domain.ErrInvalidTransition)
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		t, err = s.Repo.Task(ctx, taskID)
		if err != nil {
			return err
		}
		if t.State != domain.TaskBlocked || t.BlockReason != domain.WaitCI {
			return fmt.Errorf("task %s is %s (%s): %w", t.ID, t.State, t.BlockReason, domain.ErrInvalidTransition)
		}
		arts, err := s.currentArtifacts(ctx, t)
		if err != nil {
			return err
		}
		if len(arts) == 0 {
			// Nothing would move a VERIFYING task on (a pull request never opened, everything merged): the Owner
			// cancels or retries the task instead.
			return fmt.Errorf("task %s has no open pull request to verify: %w", t.ID, domain.ErrInvalidTransition)
		}
		now := s.now()
		if err := t.Resume(now); err != nil {
			return err
		}
		details, err := act(ctx, t, arts, now)
		if err != nil {
			return err
		}
		if err := s.saveTask(ctx, &t); err != nil {
			return err
		}
		return s.record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: kind, ProjectID: t.ProjectID,
			Subject: auditSubjectTaskPrefix + t.ID, Details: details})
	})
	if err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

// currentArtifacts are the open, unmerged artifacts of the task's current run (its plan revision's execution).
func (s *Service) currentArtifacts(ctx context.Context, t domain.Task) ([]domain.GitArtifact, error) {
	all, err := s.ChangeSets.TaskArtifacts(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	current := map[string]bool{}
	var out []domain.GitArtifact
	for _, a := range all {
		if a.State != domain.ArtifactOpen || a.Merge == domain.MergeMerged {
			continue
		}
		ok, seen := current[a.ExecutionID]
		if !seen {
			e, err := s.Repo.Execution(ctx, a.ExecutionID)
			if err != nil {
				return nil, err
			}
			ok = e.PlanRevision == t.PlanRevision
			current[a.ExecutionID] = ok
		}
		if ok {
			out = append(out, a)
		}
	}
	return out, nil
}
