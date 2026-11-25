-- migrate: foreign-keys-off
-- routing context (KIN-33): AI CLI switches (project_id '' = the global System switch; a project id = its
-- allowlist entry, checked by the use case, no FK across contexts), role bindings (global defaults and project
-- overrides) and CLI cool-downs per runner after a usage limit (cli_limit). Times are RFC 3339 UTC text.
CREATE TABLE agent_switches (
  project_id TEXT NOT NULL,
  agent      TEXT NOT NULL CHECK (agent IN ('claude', 'codex')),
  enabled    INTEGER NOT NULL CHECK (enabled IN (0, 1)),
  updated_by TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (project_id, agent)
) STRICT;

CREATE TABLE agent_role_bindings (
  project_id    TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('planner', 'implementer', 'reviewer')),
  primary_agent TEXT NOT NULL CHECK (primary_agent IN ('claude', 'codex')),
  primary_model TEXT NOT NULL DEFAULT '',
  fallback_json TEXT NOT NULL DEFAULT '[]',                -- [{"agent","model"}] in order
  strategy      TEXT NOT NULL CHECK (strategy IN ('ordered', 'different-from-author')),
  updated_by    TEXT NOT NULL DEFAULT '',
  updated_at    TEXT NOT NULL,
  PRIMARY KEY (project_id, role)
) STRICT;

CREATE TABLE agent_cooldowns (
  runner_id TEXT NOT NULL,
  agent     TEXT NOT NULL,
  until_at  TEXT NOT NULL,
  reason    TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (runner_id, agent)
) STRICT;

-- workflow: the AI CLI and model of a step's latest claim, and the wait reason 'agent' (no permitted CLI for the
-- step's role). SQLite cannot change a CHECK in place, so tasks and workflow_steps are rebuilt (12-step procedure,
-- foreign keys off for this migration; their indexes and triggers are recreated).
CREATE TABLE tasks_new (
  id             TEXT PRIMARY KEY,
  project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  title          TEXT NOT NULL,
  youtrack_issue TEXT,
  state          TEXT NOT NULL CHECK (state IN ('CREATED', 'ANALYZING', 'READY', 'AWAITING_APPROVAL', 'RUNNING',
                                                'VERIFYING', 'COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED')),
  block_reason   TEXT CHECK (block_reason IN ('network', 'runner', 'approval', 'cli_limit', 'agent')),
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

CREATE TABLE workflow_steps_new (
  id               TEXT PRIMARY KEY,
  execution_id     TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
  position         INTEGER NOT NULL,
  step_key         TEXT NOT NULL,
  title            TEXT NOT NULL,
  repository       TEXT NOT NULL,
  depends_on       TEXT NOT NULL DEFAULT '[]',
  state            TEXT NOT NULL CHECK (state IN ('PENDING', 'READY', 'CLAIMED', 'RUNNING', 'SUCCEEDED', 'WAITING',
                                                  'FAILED', 'CANCELLED')),
  wait_reason      TEXT CHECK (wait_reason IN ('network', 'runner', 'approval', 'cli_limit', 'agent')),
  reason           TEXT NOT NULL DEFAULT '',
  fence            INTEGER NOT NULL DEFAULT 0 CHECK (fence >= 0),
  runner_id        TEXT,
  lease_expires_at TEXT,
  agent            TEXT NOT NULL DEFAULT '',
  model            TEXT NOT NULL DEFAULT '',
  UNIQUE (execution_id, step_key),
  UNIQUE (execution_id, position),
  CHECK ((state = 'WAITING') = (wait_reason IS NOT NULL)),
  CHECK (state NOT IN ('CLAIMED', 'RUNNING') OR (runner_id IS NOT NULL AND lease_expires_at IS NOT NULL))
) STRICT;
INSERT INTO workflow_steps_new (id, execution_id, position, step_key, title, repository, depends_on, state,
  wait_reason, reason, fence, runner_id, lease_expires_at)
  SELECT id, execution_id, position, step_key, title, repository, depends_on, state, wait_reason, reason, fence,
  runner_id, lease_expires_at FROM workflow_steps;
DROP TABLE workflow_steps;
ALTER TABLE workflow_steps_new RENAME TO workflow_steps;
CREATE INDEX workflow_steps_lease ON workflow_steps(lease_expires_at) WHERE lease_expires_at IS NOT NULL;
CREATE TRIGGER workflow_steps_fence_monotonic BEFORE UPDATE OF fence ON workflow_steps
WHEN NEW.fence < OLD.fence
BEGIN
  SELECT RAISE(ABORT, 'fencing token must not decrease');
END;
