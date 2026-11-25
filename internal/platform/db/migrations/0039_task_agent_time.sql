-- Per-task agent time (KIN-75): the task list's totals read this aggregate instead of every attempt of every task.
-- settled_ns sums the periods that no longer change with the clock (complete attempts and plan discussion answers,
-- in nanoseconds so the sum equals the per-attempt sum exactly); settled_status is their combined status (none,
-- complete, incomplete); running counts the attempts whose period still counts up to now, which the list reads at
-- request time. The workflow repository rewrites a task's row in the transaction that saves its attempts or an answer.
CREATE TABLE task_agent_time (
  task_id        TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  settled_ns     INTEGER NOT NULL CHECK (settled_ns >= 0),
  settled_status TEXT NOT NULL CHECK (settled_status IN ('none', 'complete', 'incomplete')),
  running        INTEGER NOT NULL CHECK (running >= 0)
) STRICT;
-- The refresh and LatestExecution read a task's executions; until now only its running one was indexed.
CREATE INDEX executions_task ON executions(task_id, created_at);

-- Backfill: the same rules as domain.Attempt.Work. Timestamps are fixed-width UTC with nanoseconds
-- (2006-01-02T15:04:05.000000000Z), so text order is time order and the nanoseconds are exact integers.
WITH attempt AS (
  SELECT e.task_id, a.state IN ('RUNNING', 'UNKNOWN') AS live, a.work_incomplete AS bad,
    a.work_started_at AS ws, a.work_ended_at AS we,
    (CAST(strftime('%s', substr(a.work_ended_at, 1, 19)) AS INTEGER) * 1000000000 + CAST(substr(a.work_ended_at, 21, 9) AS INTEGER))
    - (CAST(strftime('%s', substr(a.work_started_at, 1, 19)) AS INTEGER) * 1000000000 + CAST(substr(a.work_started_at, 21, 9) AS INTEGER)) AS ns
  FROM step_attempts a JOIN workflow_steps s ON s.id = a.step_id JOIN executions e ON e.id = s.execution_id
), work AS (
  SELECT task_id, CASE
      WHEN bad THEN 'incomplete'
      WHEN ws IS NULL AND we IS NULL AND NOT live THEN 'none'
      WHEN ws IS NULL THEN 'incomplete'
      WHEN we IS NULL AND live THEN 'running'
      WHEN we IS NULL OR we < ws OR ns IS NULL OR ns > 86400000000000 THEN 'incomplete' -- NULL ns: an unreadable time
      ELSE 'complete' END AS status, ns
  FROM attempt
  UNION ALL
  SELECT task_id, 'complete', work_ms * 1000000 FROM plan_discussion_messages WHERE role = 'assistant'
)
INSERT INTO task_agent_time(task_id, settled_ns, settled_status, running)
SELECT task_id, coalesce(sum(CASE WHEN status = 'complete' THEN ns END), 0),
  CASE WHEN sum(status = 'incomplete') > 0 THEN 'incomplete' WHEN sum(status = 'complete') > 0 THEN 'complete' ELSE 'none' END,
  sum(status = 'running')
FROM work GROUP BY task_id;
