-- Scheduler failure penalty (KIN-76): the scheduler counts each runner's recently failed or lost attempts on every
-- pass; this partial index holds only those attempts, ordered by when they failed, so the count never scans the
-- whole attempt history.
CREATE INDEX step_attempts_failures ON step_attempts(coalesce(finished_at, started_at))
  WHERE state IN ('FAILED', 'UNKNOWN');
