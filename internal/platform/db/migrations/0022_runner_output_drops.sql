-- runners context (KIN-59): the Storage Overview lists recent runner output dropped because the Control Plane's
-- output storage was full. The partial index holds only those events and covers the lookup (count and per-attempt
-- grouping), so it never reads the event rows however many events are recorded.
CREATE INDEX runner_attempt_events_output_dropped ON runner_attempt_events(received_at, runner_id, step_id, fence)
  WHERE reason = 'output_storage_limit';
