-- runners context (KIN-22): the runner command/event channel. Commands for a runner are numbered in its log
-- (seq monotonic within command_epoch) and re-sent until acknowledged; acknowledged ones are purged after 7 days.
-- event_epoch/event_last are the cursor in the runner's spool stream (the last seq applied). Every runner event is
-- recorded once per (runner, step, fence, attempt_seq); output counts towards a per-attempt cap. Times are Unix ms.
CREATE TABLE runner_channels (
  runner_id     TEXT PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
  command_epoch TEXT NOT NULL CHECK (command_epoch <> ''),
  command_seq   INTEGER NOT NULL DEFAULT 0 CHECK (command_seq >= 0),
  command_acked INTEGER NOT NULL DEFAULT 0 CHECK (command_acked >= 0 AND command_acked <= command_seq),
  event_epoch   TEXT NOT NULL DEFAULT '',
  event_last    INTEGER NOT NULL DEFAULT 0 CHECK (event_last >= 0)
) STRICT;

CREATE TABLE runner_commands (
  runner_id  TEXT NOT NULL REFERENCES runner_channels(runner_id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL CHECK (seq >= 1),
  command_id TEXT NOT NULL CHECK (command_id <> ''),
  kind       TEXT NOT NULL CHECK (kind <> ''),
  body       BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  acked_at   INTEGER,
  PRIMARY KEY (runner_id, seq),
  UNIQUE (runner_id, command_id)
) STRICT;
CREATE INDEX runner_commands_acked ON runner_commands(acked_at) WHERE acked_at IS NOT NULL;

CREATE TABLE runner_attempt_events (
  step_id     TEXT NOT NULL,
  fence       INTEGER NOT NULL CHECK (fence >= 1),
  attempt_seq INTEGER NOT NULL CHECK (attempt_seq >= 1),
  runner_id   TEXT NOT NULL,
  kind        TEXT NOT NULL,
  body        BLOB,                                          -- NULL when over the attempt's output cap
  outcome     TEXT NOT NULL CHECK (outcome IN ('applied', 'rejected')),
  reason      TEXT NOT NULL DEFAULT '',
  runner_at   INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (runner_id, step_id, fence, attempt_seq)                -- a runner dedups only its own events
) STRICT;

CREATE TABLE runner_attempt_output (
  step_id TEXT NOT NULL,
  fence   INTEGER NOT NULL,
  bytes   INTEGER NOT NULL CHECK (bytes >= 0),
  PRIMARY KEY (step_id, fence)
) STRICT;
