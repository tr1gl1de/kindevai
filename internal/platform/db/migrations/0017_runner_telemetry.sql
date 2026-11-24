-- runners context (KIN-20): runner liveness and capabilities, and the storage metrics of every machine (runners
-- and the Control Plane itself, machine_id 'control-plane') with an hourly history for the fill-up forecast.
CREATE TABLE runner_telemetry (
  runner_id         TEXT PRIMARY KEY REFERENCES runners(id) ON DELETE CASCADE,
  last_heartbeat_at TEXT,
  unreachable_since TEXT,          -- set by the sweep after 3 missed heartbeats, cleared by the next heartbeat
  capabilities      TEXT,          -- JSON of the last runner.capabilities (self-reported, informational)
  capabilities_at   TEXT
) STRICT;

-- Current volumes of a machine: one row per role (workspaces, windows_host, data); replaced on every report.
CREATE TABLE storage_volumes (
  machine_id      TEXT NOT NULL,
  role            TEXT NOT NULL,
  volume_id       TEXT NOT NULL,   -- mount point (mount_or_volume_id)
  path            TEXT NOT NULL,
  total_bytes     INTEGER NOT NULL CHECK (total_bytes > 0),
  available_bytes INTEGER NOT NULL CHECK (available_bytes >= 0),
  free_bytes      INTEGER NOT NULL CHECK (free_bytes >= 0),
  reported_at     TEXT NOT NULL,
  PRIMARY KEY (machine_id, role)
) STRICT;

-- Hourly aggregates (the last report of the hour); 30-day TTL and a row limit, purged hourly.
CREATE TABLE storage_history (
  machine_id  TEXT NOT NULL,
  role        TEXT NOT NULL,
  hour        TEXT NOT NULL,       -- RFC 3339 UTC, truncated to the hour
  total_bytes INTEGER NOT NULL,
  used_bytes  INTEGER NOT NULL,
  PRIMARY KEY (machine_id, role, hour)
) STRICT;
CREATE INDEX storage_history_hour ON storage_history(hour);
