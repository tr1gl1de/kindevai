-- identity context: one-time invitations. Only the SHA-256 of the 256-bit token is stored; the state is derived
-- from the timestamps (pending / used / revoked / expired). project_id and role belong to the projects context.
CREATE TABLE invitations (
  id          TEXT PRIMARY KEY,
  token_hash  BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
  project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  role        TEXT NOT NULL,
  created_by  TEXT NOT NULL REFERENCES users(id),
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  consumed_at TEXT,
  consumed_by TEXT REFERENCES users(id) ON DELETE SET NULL,
  revoked_at  TEXT,
  CHECK (consumed_at IS NULL OR revoked_at IS NULL)
) STRICT;
CREATE INDEX invitations_created ON invitations(created_at);
