-- identity context: Telegram Mini App sign-in and account linking (KIN-39).

-- How a session was opened; unlinking Telegram revokes only the Telegram ones.
ALTER TABLE user_sessions ADD COLUMN via TEXT NOT NULL DEFAULT 'password' CHECK (via IN ('password', 'telegram'));

-- One Telegram account per user and one user per Telegram account. username is display-only.
CREATE TABLE telegram_identities (
  user_id     TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  telegram_id INTEGER NOT NULL UNIQUE CHECK (telegram_id > 0),
  username    TEXT NOT NULL DEFAULT '',
  linked_at   TEXT NOT NULL
) STRICT;

-- Single-use Link Telegram requests, bound to the creating web session. Only hashes of the startapp token, the
-- web session token and the confirmation code are stored; the state is derived from the columns.
CREATE TABLE telegram_link_requests (
  id                 TEXT PRIMARY KEY,
  token_hash         BLOB NOT NULL UNIQUE CHECK (length(token_hash) = 32),
  user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_hash       BLOB NOT NULL,
  created_at         TEXT NOT NULL,
  expires_at         TEXT NOT NULL,
  telegram_id        INTEGER,
  telegram_username  TEXT NOT NULL DEFAULT '',
  telegram_name      TEXT NOT NULL DEFAULT '',
  code_hash          BLOB,
  attempts           INTEGER NOT NULL DEFAULT 0,
  consumed_at        TEXT,
  cancelled_at       TEXT,
  CHECK (consumed_at IS NULL OR cancelled_at IS NULL)
) STRICT;
CREATE INDEX telegram_link_requests_user ON telegram_link_requests(user_id, created_at);

-- Mini App settings (the bot token itself is in the encrypted integrations vault).
CREATE TABLE telegram_bot_settings (
  id             INTEGER PRIMARY KEY CHECK (id = 1),
  bot_username   TEXT NOT NULL,
  app_short_name TEXT NOT NULL,
  updated_at     TEXT NOT NULL
) STRICT;
