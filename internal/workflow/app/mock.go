package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// MockScenarios are the mock agent's behaviours (KIN-23); the mock planner names its step after the scenario.
var MockScenarios = []string{"success", "fail", "flaky", "crash", "hang", "gate", "scope"}

// MockScope is the scope of the mock plan's step: the mock agent's own directory (the scope scenario also writes
// outside it, KIN-51).
const MockScope = "mock"

// MockPlanner is the author of mock plans.
const MockPlanner = "mock-planner"

// MockPlan is the fixed plan of a mock run: one step on the project's first repository (scope MockScope), run by the mock agent
// with the scenario (no AI). Its proposed risk is L1; the Policy Evaluator may still raise it.
func MockPlan(repos []string, scenario string) (domain.Plan, error) {
	if !slices.Contains(MockScenarios, scenario) {
		return domain.Plan{}, &domain.ValidationError{Field: "scenario", Code: "scenario_unknown"}
	}
	if len(repos) == 0 {
		return domain.Plan{}, &domain.ValidationError{Field: "projectId", Code: "repository_required"}
	}
	return domain.Plan{Risk: domain.RiskL1, MaxParallel: 1, Summary: "Mock execution, scenario " + scenario + " (no AI)",
		Steps: []domain.PlanStep{{Key: scenario, Title: "Mock agent: " + scenario, Repository: repos[0],
			Scope: []string{MockScope}}}}, nil
}

// RunMock plans the task with the mock planner: CREATED goes to ANALYZING, the mock plan becomes the next revision
// and the task is routed by its effective risk (READY starts with the next scheduler pass; L2/L3 wait in the
// Approval Center). Owner or the project's admin only.
func (s *Service) RunMock(ctx context.Context, p auth.Principal, taskID, scenario string) (domain.Task, error) {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if err := s.mayManage(ctx, p, t.ProjectID); err != nil {
		return domain.Task{}, err
	}
	repos, err := s.Projects.Repositories(ctx, t.ProjectID)
	if err != nil {
		return domain.Task{}, err
	}
	plan, err := MockPlan(repos, scenario)
	if err != nil {
		return domain.Task{}, err
	}
	if t.State == domain.TaskCreated {
		if _, err := s.StartAnalysis(ctx, taskID); err != nil {
			return domain.Task{}, err
		}
	}
	if _, err := s.SubmitPlan(ctx, taskID, plan, MockPlanner); err != nil {
		return domain.Task{}, fmt.Errorf("mock plan: %w", err)
	}
	return s.Repo.Task(ctx, taskID)
}
