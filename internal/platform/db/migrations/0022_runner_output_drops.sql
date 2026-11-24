-- runners context (KIN-59): the Storage Overview lists recent runner output dropped because the Control Plane's
-- output storage was full. The partial index holds only those events, so the lookup stays cheap however many events
-- are recorded.
CREATE INDEX runner_attempt_events_output_dropped ON runner_attempt_events(received_at)
  WHERE reason = 'output_storage_limit';
