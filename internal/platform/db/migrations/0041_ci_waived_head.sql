-- workflow context (KIN-113): the Owner's explicit "merge without CI" of a pull request without any check run. The
-- artifact remembers the head it was given for ('' none); it counts only while that head still has no check run.
ALTER TABLE change_set_artifacts ADD COLUMN ci_waived_head TEXT NOT NULL DEFAULT '';
