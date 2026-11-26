-- GitHub App (KIN-28): one App per installation; its private key and webhook secret live in integration_secrets
-- ("github.app_private_key", "github.webhook_secret"). Installation tokens are never stored: they are minted on
-- demand and cached in memory only.
CREATE TABLE github_app (
  id                 INTEGER PRIMARY KEY CHECK (id = 1),
  app_id             INTEGER NOT NULL CHECK (app_id > 0),
  updated_at         TEXT NOT NULL,
  check_status       TEXT NOT NULL DEFAULT 'none' CHECK (check_status IN ('none', 'pending', 'ok', 'failed')),
  check_id           TEXT NOT NULL DEFAULT '',
  check_code         TEXT NOT NULL DEFAULT '',
  check_detail       TEXT NOT NULL DEFAULT '',
  check_requested_at TEXT,
  checked_at         TEXT,
  last_delivery_at   TEXT,
  last_event         TEXT NOT NULL DEFAULT ''
) STRICT;

-- What the App reads from GitHub per registered repository: installation, default branch and whether main/master
-- are protected by a ruleset or branch protection (unprotected = warning in the UI).
ALTER TABLE project_repositories ADD COLUMN gh_installation_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_repositories ADD COLUMN gh_default_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repositories ADD COLUMN gh_protection TEXT NOT NULL DEFAULT 'unknown'
  CHECK (gh_protection IN ('unknown', 'protected', 'unprotected'));
ALTER TABLE project_repositories ADD COLUMN gh_unprotected TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repositories ADD COLUMN gh_sync_status TEXT NOT NULL DEFAULT 'none'
  CHECK (gh_sync_status IN ('none', 'pending', 'ok', 'failed'));
ALTER TABLE project_repositories ADD COLUMN gh_sync_id TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repositories ADD COLUMN gh_sync_code TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repositories ADD COLUMN gh_sync_detail TEXT NOT NULL DEFAULT '';
ALTER TABLE project_repositories ADD COLUMN gh_sync_requested_at TEXT;
ALTER TABLE project_repositories ADD COLUMN gh_synced_at TEXT;
