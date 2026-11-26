-- runners context (KIN-17): one-time enrollment tokens and enrolled runner machines. Only the SHA-256 of the
-- 256-bit token is stored; a token's state is derived from its timestamps. A runner is identified by its Ed25519
-- public key (the private key never leaves the machine); status gates every connection (KIN-19).
CREATE TABLE runners (
  id                  TEXT PRIMARY KEY,
  public_key          BLOB NOT NULL UNIQUE CHECK (length(public_key) = 32), -- a key enrolls once
  fingerprint         TEXT NOT NULL,                                         -- OpenSSH "SHA256:<base64>"
  hostname            TEXT NOT NULL,                                         -- informational, not identity
  os                  TEXT NOT NULL,
  arch                TEXT NOT NULL,
  version             TEXT NOT NULL DEFAULT '',
  status              TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'disabled', 'revoked')),
  created_by          TEXT REFERENCES users(id) ON DELETE SET NULL,          -- the Owner who issued the token
  enrollment_token_id TEXT NOT NULL,
  enrolled_at         TEXT NOT NULL,
  approved_by         TEXT REFERENCES users(id) ON DELETE SET NULL,
  approved_at         TEXT,
  disabled_at         TEXT,
  revoked_by          TEXT REFERENCES users(id) ON DELETE SET NULL,
  revoked_at          TEXT,
  last_seen_at        TEXT,                                                  -- written by the gateway (KIN-19/20)
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL)),
  CHECK (status = 'pending' OR status = 'revoked' OR approved_at IS NOT NULL)
) STRICT;

CREATE TABLE runner_enrollment_tokens (
  id          TEXT PRIMARY KEY,
  token_hash  BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
  created_by  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  consumed_at TEXT,
  runner_id   TEXT UNIQUE REFERENCES runners(id) ON DELETE SET NULL,
  revoked_at  TEXT,
  CHECK (consumed_at IS NULL OR revoked_at IS NULL)
) STRICT;
CREATE INDEX runner_enrollment_tokens_created ON runner_enrollment_tokens(created_at);

-- A row lets the runner execute tasks of that project; nothing else is granted in the MVP.
CREATE TABLE runner_project_permissions (
  runner_id  TEXT NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  PRIMARY KEY (runner_id, project_id)
) STRICT;
CREATE INDEX runner_project_permissions_project ON runner_project_permissions(project_id);
