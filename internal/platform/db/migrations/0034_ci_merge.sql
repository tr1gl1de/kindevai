-- migrate: foreign-keys-off
-- workflow context (KIN-37): the CI fix loop and the policy-gated merge into the integration branch.
-- A change set artifact follows CI on its head and its merge; a step counts its CI fix rounds and keeps the CI
-- failure told to the fix round's implementer.
ALTER TABLE change_set_artifacts ADD COLUMN ci_state TEXT NOT NULL DEFAULT ''
  CHECK (ci_state IN ('', 'pending', 'passed', 'failed'));
ALTER TABLE change_set_artifacts ADD COLUMN ci_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE change_set_artifacts ADD COLUMN ci_detail TEXT NOT NULL DEFAULT '';
ALTER TABLE change_set_artifacts ADD COLUMN merge_state TEXT NOT NULL DEFAULT ''
  CHECK (merge_state IN ('', 'waiting', 'merged', 'failed'));
ALTER TABLE change_set_artifacts ADD COLUMN merge_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE change_set_artifacts ADD COLUMN merge_error TEXT NOT NULL DEFAULT '';
CREATE INDEX change_set_artifacts_branch ON change_set_artifacts(repository, branch);
ALTER TABLE workflow_steps ADD COLUMN ci_round INTEGER NOT NULL DEFAULT 0 CHECK (ci_round >= 0);
ALTER TABLE workflow_steps ADD COLUMN ci_feedback TEXT NOT NULL DEFAULT '';
-- A task may be BLOCKED with reason 'ci' (CI fix budget exhausted, merge refused). SQLite cannot change a CHECK in
-- place, so tasks is rebuilt (12-step procedure, foreign keys off for this migration; indexes recreated).
CREATE TABLE tasks_new (
  id             TEXT PRIMARY KEY,
  project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  title          TEXT NOT NULL,
  youtrack_issue TEXT,
  state          TEXT NOT NULL CHECK (state IN ('CREATED', 'ANALYZING', 'READY', 'AWAITING_APPROVAL', 'RUNNING',
                                                'VERIFYING', 'COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED')),
  block_reason   TEXT CHECK (block_reason IN ('network', 'runner', 'approval', 'cli_limit', 'agent', 'ci')),
  blocked_from   TEXT,
  reason         TEXT NOT NULL DEFAULT '',
  plan_revision  INTEGER NOT NULL DEFAULT 0 CHECK (plan_revision >= 0),
  created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  version        INTEGER NOT NULL CHECK (version >= 1),
  CHECK ((state = 'BLOCKED') = (block_reason IS NOT NULL)),
  CHECK ((state = 'BLOCKED') = (blocked_from IS NOT NULL))
) STRICT;
INSERT INTO tasks_new SELECT id, project_id, title, youtrack_issue, state, block_reason, blocked_from, reason,
  plan_revision, created_by, created_at, updated_at, version FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;
CREATE INDEX tasks_project ON tasks(project_id, created_at);
CREATE UNIQUE INDEX tasks_youtrack_issue ON tasks(project_id, youtrack_issue) WHERE youtrack_issue IS NOT NULL;
CREATE INDEX tasks_project_state ON tasks(project_id, state, created_at);
