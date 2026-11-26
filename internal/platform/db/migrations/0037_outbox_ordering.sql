-- Ordered delivery (KIN-107): outbox messages of one adapter that share an ordering key are delivered one at a time
-- in enqueue order (e.g. a task's YouTrack writes: the final comment never lands before the state change).
ALTER TABLE outbox_events ADD COLUMN ordering_key TEXT;
CREATE INDEX outbox_events_ordering ON outbox_events(adapter, ordering_key, id)
  WHERE ordering_key IS NOT NULL AND status = 'pending';
