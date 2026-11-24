-- integrations context (KIN-26): the credentials vault, the installation's YouTrack connection, project bindings to
-- YouTrack projects and the project repository registry. Credentials are stored only as AES-256-GCM ciphertext
-- under the installation key (KIN-2), one row per credential, the name bound as associated data.
CREATE TABLE integration_secrets (
  name       TEXT PRIMARY KEY,
  ciphertext BLOB NOT NULL CHECK (length(ciphertext) > 0),
  updated_at TEXT NOT NULL,
  updated_by TEXT REFERENCES users(id) ON DELETE SET NULL
) STRICT;

-- One connection per installation in the MVP (KIN-26 decision); its token is the secret "youtrack.token".
CREATE TABLE youtrack_connection (
  id                 INTEGER PRIMARY KEY CHECK (id = 1),
  base_url           TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  check_status       TEXT NOT NULL DEFAULT 'none' CHECK (check_status IN ('none', 'pending', 'ok', 'failed')),
  check_id           TEXT NOT NULL DEFAULT '',
  check_code         TEXT NOT NULL DEFAULT '',
  check_detail       TEXT NOT NULL DEFAULT '',
  check_requested_at TEXT,
  checked_at         TEXT
) STRICT;

-- A YouTrack project feeds at most one project.
CREATE TABLE project_youtrack (
  project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  short_name TEXT NOT NULL UNIQUE,
  updated_at TEXT NOT NULL
) STRICT;

-- A GitHub repository belongs to one project; owner/name compare case-insensitively like on GitHub. main/master are
-- protected by the domain and never stored as an integration branch.
CREATE TABLE project_repositories (
  id                 TEXT PRIMARY KEY,
  project_id         TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  owner              TEXT NOT NULL COLLATE NOCASE,
  name               TEXT NOT NULL COLLATE NOCASE,
  integration_branch TEXT NOT NULL,
  created_at         TEXT NOT NULL,
  updated_at         TEXT NOT NULL,
  UNIQUE (owner, name)
) STRICT;
CREATE INDEX project_repositories_project ON project_repositories(project_id);
