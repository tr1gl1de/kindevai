-- workflow context (KIN-105): a repository without CI never merges automatically. An artifact remembers since when
-- its head has had no check run at all ('' while it has some or no result yet); past the window the task is BLOCKED.
ALTER TABLE change_set_artifacts ADD COLUMN ci_no_checks_since TEXT NOT NULL DEFAULT '';
