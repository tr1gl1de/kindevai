-- workflow context (KIN-36): publishing a step's reviewed work. A step's phase may now be 'publish' (the runner
-- that holds the work branch pushes the reviewed head); SQLite cannot change a column's CHECK, so the column is
-- replaced (its own CHECK goes with it). A publish attempt records the branch it pushed.
ALTER TABLE workflow_steps ADD COLUMN step_phase TEXT NOT NULL DEFAULT '' CHECK (step_phase IN ('', 'review', 'publish'));
UPDATE workflow_steps SET step_phase = phase;
ALTER TABLE workflow_steps DROP COLUMN phase;
ALTER TABLE workflow_steps RENAME COLUMN step_phase TO phase;
ALTER TABLE step_attempts ADD COLUMN pushed_branch TEXT NOT NULL DEFAULT '';
-- The execution's change set: one git artifact per published branch of a repository (multi-repo: one pull request
-- per repository and branch). The pull request into the repository's integration branch is opened (or an open one
-- of the branch reused) by the outbox: state opening → open, or failed with the reason.
CREATE TABLE change_set_artifacts (
  execution_id TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
  repository   TEXT NOT NULL,
  branch       TEXT NOT NULL CHECK (branch LIKE 'feature/%'),
  task_id      TEXT NOT NULL,
  step_id      TEXT NOT NULL,
  base_sha     TEXT NOT NULL DEFAULT '',
  head_sha     TEXT NOT NULL,
  state        TEXT NOT NULL CHECK (state IN ('opening', 'open', 'failed')),
  pr_number    INTEGER NOT NULL DEFAULT 0 CHECK (pr_number >= 0),
  pr_url       TEXT NOT NULL DEFAULT '',
  pr_base      TEXT NOT NULL DEFAULT '',
  error        TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (execution_id, repository, branch)
) STRICT;
CREATE INDEX change_set_artifacts_task ON change_set_artifacts(task_id);
