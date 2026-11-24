-- projects context: projects and per-project memberships. The platform Owner is not stored as a member.
CREATE TABLE projects (
  id         TEXT PRIMARY KEY,
  slug       TEXT NOT NULL UNIQUE,
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL
) STRICT;

CREATE TABLE project_memberships (
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       TEXT NOT NULL CHECK (role IN ('project_admin', 'member')),
  created_at TEXT NOT NULL,
  PRIMARY KEY (project_id, user_id)
) STRICT;
CREATE INDEX project_memberships_user ON project_memberships(user_id);
