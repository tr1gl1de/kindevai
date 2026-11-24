-- approvals context (KIN-24): project risk policies for the deterministic Policy Evaluator, and approvals bound to
-- one task plan revision and its planHash. A decided or superseded approval never changes again.
CREATE TABLE risk_policies (
  project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  rules_json TEXT NOT NULL,                                                -- [{category, risk, patterns[]}]
  updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
  updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE approvals (
  id            TEXT PRIMARY KEY,
  project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  task_id       TEXT NOT NULL,
  task_title    TEXT NOT NULL,                                             -- snapshot for the list
  plan_revision INTEGER NOT NULL CHECK (plan_revision >= 1),
  plan_hash     TEXT NOT NULL CHECK (length(plan_hash) = 64),
  scope         TEXT NOT NULL CHECK (scope IN ('execute', 'integrate_dev')),
  reason        TEXT NOT NULL CHECK (reason IN ('plan', 'scope_exceeded')),
  step_key      TEXT NOT NULL DEFAULT '',
  step_id       TEXT NOT NULL DEFAULT '',
  paths_json    TEXT NOT NULL DEFAULT '[]',                                -- files beyond the scope
  risk          TEXT NOT NULL CHECK (risk IN ('L0', 'L1', 'L2', 'L3')),    -- effective risk of the revision
  proposed_risk TEXT NOT NULL DEFAULT '' CHECK (proposed_risk IN ('', 'L0', 'L1', 'L2', 'L3')),
  findings_json TEXT NOT NULL DEFAULT '[]',                                -- policy rules that matched
  state         TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'superseded')),
  approver_id   TEXT REFERENCES users(id) ON DELETE SET NULL,              -- NULL when granted by the policy
  approver      TEXT NOT NULL DEFAULT '',                                  -- login at decision time, or 'policy'
  comment       TEXT NOT NULL DEFAULT '',
  requested_at  TEXT NOT NULL,
  decided_at    TEXT,
  FOREIGN KEY (task_id, plan_revision) REFERENCES task_plan_revisions(task_id, revision) ON DELETE CASCADE,
  CHECK ((state = 'pending') = (decided_at IS NULL))
) STRICT;
CREATE INDEX approvals_task ON approvals(task_id, requested_at);
CREATE INDEX approvals_list ON approvals(state, requested_at);
CREATE UNIQUE INDEX approvals_one_pending ON approvals(task_id, scope, step_key) WHERE state = 'pending';
CREATE TRIGGER approvals_decided_immutable BEFORE UPDATE ON approvals
WHEN OLD.state <> 'pending'
BEGIN
  SELECT RAISE(ABORT, 'decided approvals are immutable');
END;
