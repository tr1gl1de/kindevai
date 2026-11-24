-- audit context (KIN-16): append-only event log. No foreign keys: events outlive the users, projects and runners
-- they mention (actor_label keeps the login readable). Times are Unix milliseconds for numeric period filters.
-- Retention: none in the MVP (the volume is small). The triggers make the table append-only at the database
-- level; there is no API to change or delete events either.
CREATE TABLE audit_events (
  id          INTEGER PRIMARY KEY,
  occurred_at INTEGER NOT NULL,
  actor_type  TEXT NOT NULL CHECK (actor_type IN ('user', 'system', 'runner')),
  actor_id    TEXT NOT NULL,
  actor_label TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind <> ''),
  project_id  TEXT,                                   -- NULL for platform-wide events
  subject     TEXT NOT NULL DEFAULT '',
  details     TEXT NOT NULL DEFAULT '{}'              -- JSON object of strings, secrets redacted
) STRICT;
CREATE INDEX audit_events_project ON audit_events(project_id, id);
CREATE INDEX audit_events_actor ON audit_events(actor_id, id);
CREATE INDEX audit_events_kind ON audit_events(kind, id);
CREATE INDEX audit_events_time ON audit_events(occurred_at);

CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit_events is append-only');
END;
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit_events is append-only');
END;
