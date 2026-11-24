-- alerts context (KIN-40): the Alert Center and the users' notification preferences.

-- One row per alert. While an alert is active (OPEN/ACKNOWLEDGED) repeats with the same dedup_key are grouped
-- into it (occurrences, last_seen_at); a closed alert never changes again. project_id NULL = platform alert
-- (runners, Control Plane storage), visible to the Owner only.
CREATE TABLE alerts (
  id           TEXT PRIMARY KEY,
  project_id   TEXT REFERENCES projects(id) ON DELETE CASCADE,
  kind         TEXT NOT NULL,
  severity     TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
  dedup_key    TEXT NOT NULL,
  subject      TEXT NOT NULL DEFAULT '',
  details_json TEXT NOT NULL DEFAULT '{}',
  state        TEXT NOT NULL CHECK (state IN ('OPEN', 'ACKNOWLEDGED', 'DISMISSED', 'RESOLVED')),
  occurrences  INTEGER NOT NULL CHECK (occurrences >= 1),
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  acked_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  acked_at     TEXT,
  closed_by    TEXT REFERENCES users(id) ON DELETE SET NULL,           -- NULL when the system resolved it
  closed_at    TEXT,
  last_event   TEXT NOT NULL DEFAULT '',
  CHECK ((state IN ('DISMISSED', 'RESOLVED')) = (closed_at IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX alerts_one_active ON alerts(dedup_key) WHERE state IN ('OPEN', 'ACKNOWLEDGED');
CREATE INDEX alerts_list ON alerts(last_seen_at);
CREATE INDEX alerts_project ON alerts(project_id, last_seen_at);
CREATE TRIGGER alerts_closed_immutable BEFORE UPDATE ON alerts
WHEN OLD.state IN ('DISMISSED', 'RESOLVED')
BEGIN
  SELECT RAISE(ABORT, 'closed alerts are immutable');
END;

-- Notification settings; a user without a row gets the defaults (English, every type on).
CREATE TABLE notification_preferences (
  user_id       TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  language      TEXT NOT NULL CHECK (language IN ('en', 'ru')),
  disabled_json TEXT NOT NULL DEFAULT '[]',
  updated_at    TEXT NOT NULL
) STRICT;
