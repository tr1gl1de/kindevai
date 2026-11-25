-- Resolve tombstones (KIN-99): the latest observation time of an "anomaly ended" event per dedup key (e.g. a CLI's
-- cli_ready), so an occurrence observed at or before it that is delivered late opens no new alert. One row per key.
CREATE TABLE alert_resolutions (
  dedup_key   TEXT PRIMARY KEY,
  observed_at TEXT NOT NULL
) STRICT;
