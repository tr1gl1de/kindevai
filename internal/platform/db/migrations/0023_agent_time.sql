-- workflow context (KIN-25): agent time accounting. An attempt's work period is the time its CLI process ran, from
-- the runner's execution.started event to its execution.completed/failed event (runner clock, UTC), with the agent
-- (CLI adapter) that ran it. Each end is written once; work_incomplete marks a time that could not be recorded.
ALTER TABLE step_attempts ADD COLUMN agent TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN work_started_at TEXT;
ALTER TABLE step_attempts ADD COLUMN work_ended_at TEXT;
ALTER TABLE step_attempts ADD COLUMN work_incomplete INTEGER NOT NULL DEFAULT 0 CHECK (work_incomplete IN (0, 1));
CREATE INDEX tasks_project_state ON tasks(project_id, state, created_at);
