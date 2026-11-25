-- workflow context (KIN-81): a step's network holds in a row on the same runner. Each hold doubles the retry delay
-- of WAITING(network) (up to 30 minutes) and from the third one the Owner gets an egress alert for the runner; an
-- attempt that ends otherwise resets it.
ALTER TABLE workflow_steps ADD COLUMN network_holds INTEGER NOT NULL DEFAULT 0 CHECK (network_holds >= 0);
