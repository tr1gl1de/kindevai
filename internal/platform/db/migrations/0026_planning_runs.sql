-- workflow context (KIN-34): planner runs. A run analyses a task with the AI CLI bound to the planner role on a
-- runner; its attempts use the runner channel's step id 'planner_' || id with fence (the last token issued, never
-- decreasing). context_json is the bounded context package (untrusted issue text, linked articles); result is the
-- last attempt's final answer (untrusted, bounded). The native session (session, session_runner, session_agent) is
-- resumed by a repeat or replan on the same runner and CLI and by the plan chat (KIN-43). One active run per task.
CREATE TABLE planning_runs (
  id             TEXT PRIMARY KEY,
  task_id        TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  state          TEXT NOT NULL CHECK (state IN ('PREPARING', 'QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED')),
  fence          INTEGER NOT NULL DEFAULT 0 CHECK (fence >= 0),
  used           INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
  runner_id      TEXT NOT NULL DEFAULT '',
  agent          TEXT NOT NULL DEFAULT '',
  model          TEXT NOT NULL DEFAULT '',
  session        TEXT NOT NULL DEFAULT '',
  session_runner TEXT NOT NULL DEFAULT '',
  session_agent  TEXT NOT NULL DEFAULT '',
  feedback       TEXT NOT NULL DEFAULT '',
  result         TEXT NOT NULL DEFAULT '',
  context_json   TEXT NOT NULL DEFAULT '{}',
  wait_reason    TEXT NOT NULL DEFAULT '',
  wait_detail    TEXT NOT NULL DEFAULT '',
  not_before     TEXT,
  lease_expires  TEXT,
  reason         TEXT NOT NULL DEFAULT '',
  revision       INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
  created_by     TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  version        INTEGER NOT NULL CHECK (version >= 1),
  CHECK ((state = 'RUNNING') = (lease_expires IS NOT NULL))
) STRICT;
CREATE INDEX planning_runs_task ON planning_runs(task_id, created_at);
CREATE UNIQUE INDEX planning_runs_one_active ON planning_runs(task_id) WHERE state IN ('PREPARING', 'QUEUED', 'RUNNING');
CREATE INDEX planning_runs_active ON planning_runs(state) WHERE state IN ('PREPARING', 'QUEUED', 'RUNNING');
