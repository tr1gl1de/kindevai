package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// ErrForbidden: the caller may not see or change this project's tasks.
var ErrForbidden = errors.New("forbidden")

// Project roles as the projects context reports them.
const (
	RoleOwner        = "owner"
	RoleProjectAdmin = "project_admin"
)

// Access is the projects context's API as the task API needs it: the projects p may see, with p's role there
// (RoleOwner for the platform Owner).
type Access interface {
	Visible(ctx context.Context, p auth.Principal) (map[string]string, error)
}

// MaxTaskList bounds one task list response.
const MaxTaskList = 200

func (s *Service) visible(ctx context.Context, p auth.Principal) (map[string]string, error) {
	if s.Access == nil {
		return nil, errors.New("workflow: access not configured")
	}
	return s.Access.Visible(ctx, p)
}

// mayManage: the Owner or the project's admin.
func (s *Service) mayManage(ctx context.Context, p auth.Principal, projectID string) error {
	v, err := s.visible(ctx, p)
	if err != nil {
		return err
	}
	switch role, ok := v[projectID]; {
	case !ok:
		return domain.ErrNotFound
	case p.Owner || role == RoleOwner || role == RoleProjectAdmin:
		return nil
	}
	return ErrForbidden
}

// TaskQuery filters the task list: one project (empty: every visible one) and states (empty: all).
type TaskQuery struct {
	ProjectID string
	States    []domain.TaskState
}

// TaskListItem is a task in the list with its time.
type TaskListItem struct {
	Task domain.Task
	Time TaskTime
}

// TaskList is the task list: at most MaxTaskList tasks, newest first, and the agent time of every task the query
// selects (also beyond the limit).
type TaskList struct {
	Tasks []TaskListItem
	Total domain.AgentTime
}

// ListTasks returns the tasks p may see, newest first, with their agent time and the total for the query.
func (s *Service) ListTasks(ctx context.Context, p auth.Principal, q TaskQuery) (TaskList, error) {
	for _, st := range q.States {
		if !st.Valid() {
			return TaskList{}, &domain.ValidationError{Field: "state", Code: "state_unknown"}
		}
	}
	v, err := s.visible(ctx, p)
	if err != nil {
		return TaskList{}, err
	}
	f := TaskFilter{States: q.States}
	for id := range v {
		if q.ProjectID == "" || id == q.ProjectID {
			f.ProjectIDs = append(f.ProjectIDs, id)
		}
	}
	out := TaskList{Tasks: []TaskListItem{}, Total: domain.AgentTime{Status: domain.WorkNone}}
	if len(f.ProjectIDs) == 0 {
		return out, nil
	}
	slices.Sort(f.ProjectIDs)
	ts, err := s.Repo.ListTasks(ctx, f, MaxTaskList)
	if err != nil {
		return TaskList{}, err
	}
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		ids = append(ids, t.ID)
	}
	times, total, err := s.taskTimes(ctx, f, ids)
	if err != nil {
		return TaskList{}, err
	}
	for _, t := range ts {
		out.Tasks = append(out.Tasks, TaskListItem{Task: t, Time: times[t.ID]})
	}
	out.Total = total
	return out, nil
}

// TaskDetail is a task with its current plan revision and latest execution (nil when there is none).
type TaskDetail struct {
	Task      domain.Task
	Revision  *domain.PlanRevision
	Execution *domain.Execution
	Planning  *domain.PlanningRun // the latest planner run (KIN-34); nil when the task was never planned by AI
	CanManage bool
	Time      TaskTime // over all the task's executions
	// ChangeSet is the task's published branches and their pull requests (KIN-36), oldest first.
	ChangeSet []domain.GitArtifact
}

// TaskDetail returns a task p may see (domain.ErrNotFound otherwise).
func (s *Service) TaskDetail(ctx context.Context, p auth.Principal, id string) (TaskDetail, error) {
	t, err := s.Repo.Task(ctx, id)
	if err != nil {
		return TaskDetail{}, err
	}
	v, err := s.visible(ctx, p)
	if err != nil {
		return TaskDetail{}, err
	}
	role, ok := v[t.ProjectID]
	if !ok {
		return TaskDetail{}, domain.ErrNotFound
	}
	d := TaskDetail{Task: t, CanManage: p.Owner || role == RoleOwner || role == RoleProjectAdmin}
	if t.PlanRevision > 0 {
		rev, err := s.Repo.Revision(ctx, t.ID, t.PlanRevision)
		if err != nil {
			return TaskDetail{}, err
		}
		d.Revision = &rev
	}
	e, err := s.Repo.LatestExecution(ctx, t.ID)
	switch {
	case err == nil:
		d.Execution = &e
	case !errors.Is(err, domain.ErrNotFound):
		return TaskDetail{}, err
	}
	if s.Planning != nil {
		run, err := s.Planning.LatestPlanRun(ctx, t.ID)
		switch {
		case err == nil:
			d.Planning = &run
		case !errors.Is(err, domain.ErrNotFound):
			return TaskDetail{}, err
		}
	}
	times, _, err := s.taskTimes(ctx, TaskFilter{TaskIDs: []string{t.ID}}, []string{t.ID})
	if err != nil {
		return TaskDetail{}, err
	}
	d.Time = times[t.ID]
	if d.ChangeSet, err = s.TaskChangeSet(ctx, t.ID); err != nil {
		return TaskDetail{}, err
	}
	return d, nil
}

// CreateTaskAs creates a manual task in a project p manages.
func (s *Service) CreateTaskAs(ctx context.Context, p auth.Principal, projectID, title string) (domain.Task, error) {
	if err := s.mayManage(ctx, p, projectID); err != nil {
		return domain.Task{}, err
	}
	return s.CreateTask(ctx, projectID, title, "", p.UserID)
}

// CancelTaskAs cancels a task of a project p manages (its running attempts get execution.cancel).
func (s *Service) CancelTaskAs(ctx context.Context, p auth.Principal, taskID, reason string) (domain.Task, error) {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if err := s.mayManage(ctx, p, t.ProjectID); err != nil {
		return domain.Task{}, err
	}
	return s.CancelTask(ctx, taskID, reason)
}

// RetryTask plans a FAILED task again (Owner or project admin): its current plan becomes the next revision, which
// the Policy Evaluator checks again (L2/L3 need a new approval), and the scheduler runs it like any new plan.
func (s *Service) RetryTask(ctx context.Context, p auth.Principal, taskID string) (domain.Task, error) {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if err := s.mayManage(ctx, p, t.ProjectID); err != nil {
		return domain.Task{}, err
	}
	if t.State != domain.TaskFailed || t.PlanRevision == 0 {
		return domain.Task{}, fmt.Errorf("task %s is %s: retry: %w", t.ID, t.State, domain.ErrInvalidTransition)
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		rev, err := s.Repo.Revision(ctx, t.ID, t.PlanRevision)
		if err != nil {
			return err
		}
		if _, err := s.StartAnalysis(ctx, t.ID); err != nil {
			return err
		}
		_, err = s.SubmitPlan(ctx, t.ID, rev.Plan(), p.UserID)
		return err
	})
	if err != nil {
		return domain.Task{}, err
	}
	return s.Repo.Task(ctx, taskID)
}
