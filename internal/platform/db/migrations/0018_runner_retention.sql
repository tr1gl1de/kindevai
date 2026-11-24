-- runners context (KIN-52): retention of recorded runner events. Output is removed per attempt some days after its
-- execution finished (last_at: the attempt's last counted output, Unix ms, finds candidates); other events later.
-- runner_output_usage is the total of counted output bytes (the sum of runner_attempt_output.bytes), checked
-- against the Control Plane-wide output limit on every output event.
ALTER TABLE runner_attempt_output ADD COLUMN last_at INTEGER NOT NULL DEFAULT 0;
CREATE INDEX runner_attempt_output_last ON runner_attempt_output(last_at, step_id, fence);
CREATE INDEX runner_attempt_events_attempt ON runner_attempt_events(step_id, fence);
CREATE INDEX runner_attempt_events_received ON runner_attempt_events(received_at);

CREATE TABLE runner_output_usage (
  id    INTEGER PRIMARY KEY CHECK (id = 1),
  bytes INTEGER NOT NULL CHECK (bytes >= 0)
) STRICT;
INSERT INTO runner_output_usage(id, bytes) SELECT 1, coalesce(sum(bytes), 0) FROM runner_attempt_output;
