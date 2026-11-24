-- Installation-level metadata (installation id, key check, setup state).
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;

-- Platform configuration editable from the Web UI. Secret values are AES-GCM ciphertext (encrypted = 1).
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      BLOB NOT NULL,
  encrypted  INTEGER NOT NULL DEFAULT 0 CHECK (encrypted IN (0, 1)),
  updated_at TEXT NOT NULL
) STRICT;
