-- workflow context (KIN-43): the plan discussion. A chat turn is a planning run of kind 'discussion' answering
-- message_id (its attempts use the planner's step id and runner channel); usage_json is what the run's CLI reported.
ALTER TABLE planning_runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'plan' CHECK (kind IN ('plan', 'discussion'));
ALTER TABLE planning_runs ADD COLUMN message_id TEXT NOT NULL DEFAULT '';
ALTER TABLE planning_runs ADD COLUMN usage_json TEXT NOT NULL DEFAULT '{}';

-- Messages of a task's plan discussion: user messages (status PENDING -> DELIVERED -> ANSWERED | FAILED, run_id the
-- current turn), the planner's answers (run_id the turn that answered: one answer per turn) and system notices.
-- text is untrusted; sent_at is when a user message was last sent (the hourly limit). work_ms is the CLI's time on an answer (agent time).
CREATE TABLE plan_discussion_messages (
  id            TEXT PRIMARY KEY,
  task_id       TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  role          TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
  author        TEXT NOT NULL,
  plan_revision INTEGER NOT NULL CHECK (plan_revision >= 0),
  text          TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('PENDING', 'DELIVERED', 'ANSWERED', 'FAILED')),
  reason        TEXT NOT NULL DEFAULT '',
  notice        TEXT NOT NULL DEFAULT '',
  revision      INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0),
  reply_to      TEXT NOT NULL DEFAULT '',
  run_id        TEXT NOT NULL DEFAULT '',
  agent         TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  usage_json    TEXT NOT NULL DEFAULT '{}',
  work_ms       INTEGER NOT NULL DEFAULT 0 CHECK (work_ms >= 0),
  delivered_at  TEXT,
  sent_at       TEXT,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  version       INTEGER NOT NULL CHECK (version >= 1)
) STRICT;
CREATE INDEX plan_discussion_messages_task ON plan_discussion_messages(task_id, created_at);
CREATE INDEX plan_discussion_messages_sent ON plan_discussion_messages(author, sent_at) WHERE role = 'user';
CREATE UNIQUE INDEX plan_discussion_one_answer ON plan_discussion_messages(run_id) WHERE role = 'assistant';

-- The discussion limits the Owner set (one row; absent: the defaults).
CREATE TABLE plan_discussion_settings (
  id                     INTEGER PRIMARY KEY CHECK (id = 1),
  max_message_chars      INTEGER NOT NULL,
  max_messages_per_plan  INTEGER NOT NULL,
  max_messages_per_hour  INTEGER NOT NULL,
  answer_timeout_seconds INTEGER NOT NULL,
  updated_by             TEXT NOT NULL DEFAULT '',
  updated_at             TEXT NOT NULL
) STRICT;
