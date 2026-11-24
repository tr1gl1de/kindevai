package app

import (
	"context"
	"errors"
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

// ListTasks returns the tasks p may see, newest first; projectID narrows it to one project.
func (s *Service) ListTasks(ctx context.Context, p auth.Principal, projectID string) ([]domain.Task, error) {
	v, err := s.visible(ctx, p)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(v))
	for id := range v {
		if projectID == "" || id == projectID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return s.Repo.ListTasks(ctx, ids, MaxTaskList)
}

// TaskDetail is a task with its current plan revision and latest execution (nil when there is none).
type TaskDetail struct {
	Task      domain.Task
	Revision  *domain.PlanRevision
	Execution *domain.Execution
	CanManage bool
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
