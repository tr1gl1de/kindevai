package app

import (
	"context"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// TaskFilter selects tasks in the repository: by project, state and id (an empty list does not filter, but a
// query needs ProjectIDs or TaskIDs).
type TaskFilter struct {
	ProjectIDs []string
	States     []domain.TaskState
	TaskIDs    []string
}

// ExecutionSpan is when a task's first execution was created and its last one finished (nil while one runs).
type ExecutionSpan struct {
	Start time.Time
	End   *time.Time
}

// TaskTime is a task's agent time (the sum of its attempts' work periods, KIN-25) and, separately, its calendar
// duration (from its first execution's start to its last one's end, or now while one runs): parallel agents make
// the agent time larger than the calendar time.
type TaskTime struct {
	Agent    domain.AgentTime
	Calendar time.Duration
	Running  bool // an execution runs: the calendar duration counts up to now
	Started  bool // the task has an execution
}

// taskTimes returns the time of the tasks ids and the agent time of every task f selects.
func (s *Service) taskTimes(ctx context.Context, f TaskFilter, ids []string) (map[string]TaskTime, domain.AgentTime, error) {
	now := s.now()
	out := make(map[string]TaskTime, len(ids))
	for _, id := range ids {
		out[id] = TaskTime{Agent: domain.AgentTime{Status: domain.WorkNone}}
	}
	total := domain.AgentTime{Status: domain.WorkNone}
	err := s.Repo.EachAttempt(ctx, f, func(taskID string, a domain.Attempt) error {
		st, d := a.Work(now)
		total.Add(st, d)
		if t, ok := out[taskID]; ok {
			t.Agent.Add(st, d)
			out[taskID] = t
		}
		return nil
	})
	if err != nil {
		return nil, total, err
	}
	if s.Discussion != nil {
		// The CLI's time on plan discussion answers (KIN-43) is agent time of the task too.
		work, err := s.Discussion.DiscussionWork(ctx, f)
		if err != nil {
			return nil, total, err
		}
		for id, d := range work {
			total.Add(domain.WorkComplete, d)
			if t, ok := out[id]; ok {
				t.Agent.Add(domain.WorkComplete, d)
				out[id] = t
			}
		}
	}
	if len(ids) == 0 {
		return out, total, nil
	}
	spans, err := s.Repo.ExecutionSpans(ctx, ids)
	if err != nil {
		return nil, total, err
	}
	for id, sp := range spans {
		t, ok := out[id]
		if !ok {
			continue
		}
		end := now
		if sp.End != nil {
			end = *sp.End
		}
		t.Started, t.Running, t.Calendar = true, sp.End == nil, max(0, end.Sub(sp.Start))
		out[id] = t
	}
	return out, total, nil
}
