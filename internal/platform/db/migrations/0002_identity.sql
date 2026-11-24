-- identity context: accounts. Setup state and the setup token hash live in meta.
CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  login         TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  platform_role TEXT NOT NULL CHECK (platform_role IN ('owner', 'user')),
  created_at    TEXT NOT NULL,
  disabled_at   TEXT
) STRICT;

-- MVP has exactly one Owner; enforce it in the schema, not only in the setup flow.
CREATE UNIQUE INDEX users_single_owner ON users(platform_role) WHERE platform_role = 'owner';
