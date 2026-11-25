-- workflow context (KIN-35): the implementation and review loop of a plan step. A step records whether it runs the
-- loop, its phase ('' implement, 'review'), its fix round, the runner that holds its work branch and the test
-- command of its current claim. An attempt records its role and round, the CLI's final answer (the handoff or the
-- verdict, at most 16 KiB), the test command it was sent and the runner's test evidence (JSON: command, exit code,
-- timed out, duration, output tail), and its final workspace report (base/head SHA, changed files as JSON).
ALTER TABLE workflow_steps ADD COLUMN review INTEGER NOT NULL DEFAULT 0 CHECK (review IN (0, 1));
ALTER TABLE workflow_steps ADD COLUMN phase TEXT NOT NULL DEFAULT '' CHECK (phase IN ('', 'review'));
ALTER TABLE workflow_steps ADD COLUMN review_round INTEGER NOT NULL DEFAULT 0 CHECK (review_round >= 0);
ALTER TABLE workflow_steps ADD COLUMN pinned_runner TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_steps ADD COLUMN test_command TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN role TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN review_round INTEGER NOT NULL DEFAULT 0 CHECK (review_round >= 0);
ALTER TABLE step_attempts ADD COLUMN agent_result TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN test_command TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN tests TEXT;
ALTER TABLE step_attempts ADD COLUMN base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN files TEXT NOT NULL DEFAULT '[]';
ALTER TABLE step_attempts ADD COLUMN files_total INTEGER NOT NULL DEFAULT 0 CHECK (files_total >= 0);
