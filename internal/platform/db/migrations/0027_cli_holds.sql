-- routing context (KIN-83): a runner's CLI hold after cli_limit remembers its cause, how many holds of that cause
-- came in a row (backoff and alert escalation) and whether it waits for the Owner (parked: no timer). The row stays
-- after the hold expired so the next hold continues the streak; a successful attempt or the Owner deletes it.
ALTER TABLE agent_cooldowns ADD COLUMN cause TEXT NOT NULL DEFAULT 'rate_limit'
  CHECK (cause IN ('rate_limit', 'auth', 'billing'));
ALTER TABLE agent_cooldowns ADD COLUMN strikes INTEGER NOT NULL DEFAULT 1 CHECK (strikes >= 1);
ALTER TABLE agent_cooldowns ADD COLUMN parked INTEGER NOT NULL DEFAULT 0 CHECK (parked IN (0, 1));
