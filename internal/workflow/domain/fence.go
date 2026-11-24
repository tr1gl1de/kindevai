package domain

// CheckHolder reports whether runnerID with fence holds stepID's current claim; ErrStaleFence when the token was
// superseded or the execution finished. It changes nothing: the runner channel checks every event with it (KIN-22).
func (e *Execution) CheckHolder(stepID, runnerID string, fence int64) error {
	_, err := e.holder(stepID, runnerID, fence, "report")
	return err
}
