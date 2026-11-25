-- workflow context (KIN-79): what an attempt's AI CLI reported — its native session id (to resume), model, CLI
-- version and usage (tokens, cost in USD, turns) — and when a step WAITING for its CLI's limit (cli_limit) is
-- retried (RFC 3339 UTC; NULL otherwise).
ALTER TABLE step_attempts ADD COLUMN agent_session TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN agent_model TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN agent_version TEXT NOT NULL DEFAULT '';
ALTER TABLE step_attempts ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0);
ALTER TABLE step_attempts ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0);
ALTER TABLE step_attempts ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0);
ALTER TABLE step_attempts ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0);
ALTER TABLE step_attempts ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0 CHECK (cost_usd >= 0);
ALTER TABLE step_attempts ADD COLUMN turns INTEGER NOT NULL DEFAULT 0 CHECK (turns >= 0);
ALTER TABLE workflow_steps ADD COLUMN wait_until TEXT;
