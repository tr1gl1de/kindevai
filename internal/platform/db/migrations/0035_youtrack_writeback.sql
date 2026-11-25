-- YouTrack write-back (KIN-38).
-- A bound project may map task states to YouTrack states (JSON object; empty = comments only, the default).
ALTER TABLE project_youtrack ADD COLUMN state_map TEXT NOT NULL DEFAULT '{}';

-- Every write-back message of a task (its outbox key is the correlation id): the task page shows the pending and
-- failed ones ("awaiting publication").
CREATE TABLE youtrack_writes (
  key        TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL,
  issue_id   TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('comment', 'state', 'article')),
  target     TEXT,
  status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'published', 'failed')),
  attempts   INTEGER NOT NULL DEFAULT 0,
  error      TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX youtrack_writes_task ON youtrack_writes(task_id, status);

-- The documentation decision of a task's completed run: updated articles, or DOCS_NOT_APPLICABLE with its reason.
CREATE TABLE task_docs (
  task_id      TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  execution_id TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('updated', 'not_applicable', 'unstated')),
  reason       TEXT CHECK (status <> 'not_applicable' OR (reason IS NOT NULL AND trim(reason) <> '')),
  articles     TEXT NOT NULL DEFAULT '[]',
  rejected     TEXT NOT NULL DEFAULT '[]',
  created_at   TEXT NOT NULL,
  PRIMARY KEY (task_id, execution_id)
);
