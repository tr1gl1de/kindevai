package app

import "context"

// CheckFence reports whether runnerID with fence holds the step's current claim (domain.ErrStaleFence otherwise,
// domain.ErrNotFound for an unknown step). It changes nothing; the runner channel checks every event with it.
func (s *Service) CheckFence(ctx context.Context, stepID, runnerID string, fence int64) error {
	e, err := s.Repo.ExecutionByStep(ctx, stepID)
	if err != nil {
		return err
	}
	return e.CheckHolder(stepID, runnerID, fence)
}
