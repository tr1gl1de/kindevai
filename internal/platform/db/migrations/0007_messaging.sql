-- shared kernel (KIN-15): durable outbox (OutboxEvent) and inbox (InboxEvent). Times are Unix milliseconds so
-- the dispatcher's due-time comparisons are numeric. Payloads are JSON without secrets and are deleted with the
-- row after the retention period (done 7 days, dead 30 days).
CREATE TABLE outbox_events (
  id              INTEGER PRIMARY KEY,
  adapter         TEXT NOT NULL CHECK (adapter <> ''),     -- external system: youtrack, github, telegram, runner
  topic           TEXT NOT NULL CHECK (topic <> ''),       -- selects the delivery handler
  idempotency_key TEXT NOT NULL CHECK (idempotency_key <> ''),
  payload         BLOB NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'dead')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,                       -- also the lease expiry while a delivery is in flight
  last_error      TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (adapter, idempotency_key)
) STRICT;
CREATE INDEX outbox_events_due ON outbox_events(status, next_attempt_at);
CREATE INDEX outbox_events_finished ON outbox_events(status, updated_at);

CREATE TABLE inbox_events (
  id              INTEGER PRIMARY KEY,
  source          TEXT NOT NULL CHECK (source <> ''),      -- sender: github, youtrack, telegram, runner
  delivery_id     TEXT NOT NULL CHECK (delivery_id <> ''), -- the sender's delivery/event id, for deduplication
  topic           TEXT NOT NULL CHECK (topic <> ''),
  payload         BLOB NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'dead')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  last_error      TEXT,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (source, delivery_id)
) STRICT;
CREATE INDEX inbox_events_due ON inbox_events(status, next_attempt_at);
CREATE INDEX inbox_events_finished ON inbox_events(status, updated_at);
