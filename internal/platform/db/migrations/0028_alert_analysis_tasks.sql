-- alerts context (KIN-61): the YouTrack analysis task suggested from an alert after explicit confirmation. A
-- separate table because closed alerts are immutable and the issue may be created after the alert closed. The
-- primary key keeps it to one per alert; it goes with its alert (retention).
CREATE TABLE alert_analysis_tasks (
  alert_id         TEXT PRIMARY KEY REFERENCES alerts(id) ON DELETE CASCADE,
  project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  youtrack_project TEXT NOT NULL,
  requested_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  requested_at     TEXT NOT NULL,
  issue_id         TEXT,
  issue_url        TEXT,
  created_at       TEXT,
  CHECK ((issue_id IS NULL) = (created_at IS NULL))
) STRICT;
