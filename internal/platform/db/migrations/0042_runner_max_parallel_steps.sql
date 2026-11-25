-- runners context (KIN-116): the Owner's parallel-step limit of a runner, set in the runners UI (0 = use the runner's
-- own --max-parallel-steps). The scheduler applies the lower of the two.
ALTER TABLE runners ADD COLUMN max_parallel_steps INTEGER NOT NULL DEFAULT 0;
