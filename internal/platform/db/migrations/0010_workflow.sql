-- workflow context (KIN-21): tasks with their FSM, immutable plan revisions, executions of a revision with their
-- DAG steps and step attempts. Executions and tasks carry a version for optimistic concurrency; a step's fence is
-- the last fencing token issued for it and never decreases. Runner IDs are opaque references (runners are revoked,
-- never deleted).
CREATE TABLE tasks (
  id             TEXT PRIMARY KEY,
  project_id     TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  title          TEXT NOT NULL,
  youtrack_issue TEXT,                                                      -- e.g. KIN-7; NULL for manual tasks
  state          TEXT NOT NULL CHECK (state IN ('CREATED', 'ANALYZING', 'READY', 'AWAITING_APPROVAL', 'RUNNING',
                                                'VERIFYING', 'COMPLETED', 'BLOCKED', 'FAILED', 'CANCELLED')),
  block_reason   TEXT CHECK (block_reason IN ('network', 'runner', 'approval', 'cli_limit')),
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
CREATE INDEX tasks_project ON tasks(project_id, created_at);
CREATE UNIQUE INDEX tasks_youtrack_issue ON tasks(project_id, youtrack_issue) WHERE youtrack_issue IS NOT NULL;

CREATE TABLE task_plan_revisions (
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  revision   INTEGER NOT NULL CHECK (revision >= 1),
  plan_json  TEXT NOT NULL,                                                -- canonical JSON of the plan
  plan_hash  TEXT NOT NULL CHECK (length(plan_hash) = 64),                 -- hex SHA-256 of plan_json
  risk       TEXT NOT NULL CHECK (risk IN ('L0', 'L1', 'L2', 'L3')),      -- effective risk after policy
  created_by TEXT NOT NULL DEFAULT '',                                     -- user id, or the planner/chat agent
  created_at TEXT NOT NULL,
  PRIMARY KEY (task_id, revision)
) STRICT;
CREATE TRIGGER task_plan_revisions_immutable BEFORE UPDATE ON task_plan_revisions
BEGIN
  SELECT RAISE(ABORT, 'task plan revisions are immutable');
END;

CREATE TABLE executions (
  id            TEXT PRIMARY KEY,
  task_id       TEXT NOT NULL,
  plan_revision INTEGER NOT NULL,
  plan_hash     TEXT NOT NULL,
  state         TEXT NOT NULL CHECK (state IN ('RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
  max_parallel  INTEGER NOT NULL CHECK (max_parallel >= 1),
  reason        TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  finished_at   TEXT,
  version       INTEGER NOT NULL CHECK (version >= 1),
  FOREIGN KEY (task_id, plan_revision) REFERENCES task_plan_revisions(task_id, revision) ON DELETE CASCADE,
  CHECK ((state = 'RUNNING') = (finished_at IS NULL))
) STRICT;
CREATE UNIQUE INDEX executions_one_running ON executions(task_id) WHERE state = 'RUNNING';

CREATE TABLE workflow_steps (
  id               TEXT PRIMARY KEY,
  execution_id     TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
  position         INTEGER NOT NULL,                                       -- plan order
  step_key         TEXT NOT NULL,
  title            TEXT NOT NULL,
  repository       TEXT NOT NULL,
  depends_on       TEXT NOT NULL DEFAULT '[]',                             -- JSON array of step keys
  state            TEXT NOT NULL CHECK (state IN ('PENDING', 'READY', 'CLAIMED', 'RUNNING', 'SUCCEEDED', 'WAITING',
                                                  'FAILED', 'CANCELLED')),
  wait_reason      TEXT CHECK (wait_reason IN ('network', 'runner', 'approval', 'cli_limit')),
  reason           TEXT NOT NULL DEFAULT '',
  fence            INTEGER NOT NULL DEFAULT 0 CHECK (fence >= 0),
  runner_id        TEXT,
  lease_expires_at TEXT,
  UNIQUE (execution_id, step_key),
  UNIQUE (execution_id, position),
  CHECK ((state = 'WAITING') = (wait_reason IS NOT NULL)),
  CHECK (state NOT IN ('CLAIMED', 'RUNNING') OR (runner_id IS NOT NULL AND lease_expires_at IS NOT NULL))
) STRICT;
CREATE INDEX workflow_steps_lease ON workflow_steps(lease_expires_at) WHERE lease_expires_at IS NOT NULL;
-- Fencing tokens are monotonic: a stale writer can never wind a step's token back.
CREATE TRIGGER workflow_steps_fence_monotonic BEFORE UPDATE OF fence ON workflow_steps
WHEN NEW.fence < OLD.fence
BEGIN
  SELECT RAISE(ABORT, 'fencing token must not decrease');
END;

CREATE TABLE step_attempts (
  step_id     TEXT NOT NULL REFERENCES workflow_steps(id) ON DELETE CASCADE,
  number      INTEGER NOT NULL CHECK (number >= 1),
  runner_id   TEXT NOT NULL,
  fence       INTEGER NOT NULL CHECK (fence >= 1),
  state       TEXT NOT NULL CHECK (state IN ('RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'UNKNOWN')),
  error       TEXT NOT NULL DEFAULT '',
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  PRIMARY KEY (step_id, number),
  UNIQUE (step_id, fence)
) STRICT;
