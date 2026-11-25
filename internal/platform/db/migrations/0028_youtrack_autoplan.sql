-- YouTrack intake (KIN-85): a bound project may hand the tasks its intake creates to the AI planner at once.
-- Off by default; the Owner or the project's admin turns it on.
ALTER TABLE project_youtrack ADD COLUMN auto_plan INTEGER NOT NULL DEFAULT 0 CHECK (auto_plan IN (0, 1));
