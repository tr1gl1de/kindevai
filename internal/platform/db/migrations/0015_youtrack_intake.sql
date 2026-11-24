-- YouTrack intake (KIN-27): issues of a bound YouTrack project become workflow tasks. Polling is the primary
-- channel; each binding keeps its own cursor (the newest issue `updated`, Unix ms) and trigger tag. YouTrack
-- logins are linked to platform accounts by the Owner; the last intake decision per issue is kept with its reason.
ALTER TABLE project_youtrack ADD COLUMN trigger_tag TEXT NOT NULL DEFAULT 'kindevai';
ALTER TABLE project_youtrack ADD COLUMN poll_cursor INTEGER NOT NULL DEFAULT 0 CHECK (poll_cursor >= 0);
ALTER TABLE project_youtrack ADD COLUMN last_polled_at TEXT;

CREATE TABLE youtrack_intake_settings (
  id              INTEGER PRIMARY KEY CHECK (id = 1),
  poll_interval_s INTEGER NOT NULL CHECK (poll_interval_s BETWEEN 15 AND 3600),
  updated_at      TEXT NOT NULL
) STRICT;

CREATE TABLE youtrack_user_links (
  youtrack_login TEXT PRIMARY KEY COLLATE NOCASE,
  user_id        TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
  updated_at     TEXT NOT NULL
) STRICT;

CREATE TABLE youtrack_intake (
  project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_id      TEXT NOT NULL,
  issue_updated INTEGER NOT NULL DEFAULT 0,
  outcome       TEXT NOT NULL CHECK (outcome IN ('queued', 'created', 'updated', 'unchanged', 'rejected')),
  reason        TEXT NOT NULL DEFAULT '',
  initiator     TEXT NOT NULL DEFAULT '',  -- platform user id; opaque so a deleted account keeps the history
  reporter      TEXT NOT NULL DEFAULT '',  -- YouTrack login
  task_id       TEXT NOT NULL DEFAULT '',
  channel       TEXT NOT NULL CHECK (channel IN ('poll', 'webhook', 'manual')),
  processed_at  TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_id)
) STRICT;
CREATE INDEX youtrack_intake_recent ON youtrack_intake(project_id, processed_at);
