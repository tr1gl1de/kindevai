-- runners context (KIN-55): the last space-consumer measurement of a machine (KB KIN-A-26), one row per category;
-- replaced as a whole by every new measurement. Runners measure only when their operator allowed it.
CREATE TABLE storage_consumers (
  machine_id  TEXT NOT NULL,
  category    TEXT NOT NULL,       -- repos, workspaces, sessions, spool, container_cache
  bytes       INTEGER NOT NULL CHECK (bytes >= 0),
  entries     INTEGER NOT NULL CHECK (entries >= 0),
  partial     INTEGER NOT NULL CHECK (partial IN (0, 1)), -- stopped at the entry or time limit: bytes is a lower bound
  measured_at TEXT NOT NULL,
  PRIMARY KEY (machine_id, category)
) STRICT;
