-- identity context: login sessions. Only the SHA-256 of the bearer token is stored.
CREATE TABLE user_sessions (
  token_hash   BLOB PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  expires_at   TEXT NOT NULL
) STRICT;
CREATE INDEX user_sessions_expires ON user_sessions(expires_at);
CREATE INDEX user_sessions_user ON user_sessions(user_id);
