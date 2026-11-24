package messaging

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// table names one durable queue: its table, the column naming the external system and the dedup key column.
type table struct{ name, endpoint, key string }

var (
	outboxTable = table{name: "outbox_events", endpoint: "adapter", key: "idempotency_key"}
	inboxTable  = table{name: "inbox_events", endpoint: "source", key: "delivery_id"}
)

type queries struct{ insert, candidates, claim, done, reschedule, dead, listDead, retry, purge, stats string }

// sqlFor fills the statement templates; only the compile-time table definitions above are substituted.
func sqlFor(t table) queries {
	r := strings.NewReplacer("{t}", t.name, "{e}", t.endpoint, "{k}", t.key)
	return queries{
		insert: r.Replace(`INSERT INTO {t}({e}, topic, {k}, payload, next_attempt_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT({e}, {k}) DO NOTHING`),
		candidates: r.Replace(`SELECT id, {e}, topic, {k}, payload, attempts FROM {t}
			WHERE status = 'pending' AND next_attempt_at <= ? AND {e} NOT IN (SELECT value FROM json_each(?))
			ORDER BY next_attempt_at, id LIMIT ?`),
		claim:      r.Replace(`UPDATE {t} SET attempts = attempts + 1, next_attempt_at = ?, updated_at = ? WHERE id = ?`),
		done:       r.Replace(`UPDATE {t} SET status = 'done', last_error = NULL, updated_at = ? WHERE id = ? AND status = 'pending'`),
		reschedule: r.Replace(`UPDATE {t} SET next_attempt_at = ?, last_error = ?, updated_at = ? WHERE id = ? AND status = 'pending'`),
		dead:       r.Replace(`UPDATE {t} SET status = 'dead', last_error = ?, updated_at = ? WHERE id = ? AND status = 'pending'`),
		listDead: r.Replace(`SELECT id, {e}, topic, {k}, attempts, coalesce(last_error, ''), created_at, updated_at
			FROM {t} WHERE status = 'dead' ORDER BY updated_at DESC, id DESC LIMIT ?`),
		retry: r.Replace(`UPDATE {t} SET status = 'pending', attempts = 0, next_attempt_at = ?, updated_at = ?
			WHERE id = ? AND status = 'dead'`),
		purge: r.Replace(`DELETE FROM {t} WHERE (status = 'done' AND updated_at <= ?) OR (status = 'dead' AND updated_at <= ?)`),
		stats: r.Replace(`SELECT {e}, sum(status = 'pending'), sum(status = 'dead') FROM {t} GROUP BY {e}`),
	}
}

// record is a claimed message.
type record struct {
	id       int64
	endpoint string
	topic    string
	key      string
	payload  []byte
	attempts int // including the current one
}

// DeadMessage is a message that exhausted its attempts or failed permanently. Payloads are not exposed.
type DeadMessage struct {
	ID        int64
	Endpoint  string // adapter (outbox) or source (inbox)
	Topic     string
	Key       string // idempotency key (outbox) or delivery id (inbox)
	Attempts  int
	LastError string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Counts are the unfinished messages of one adapter or source.
type Counts struct {
	Pending int
	Dead    int
}

// queue is the storage and scheduling shared by the outbox and the inbox.
type queue struct {
	d    *sql.DB
	cfg  Config
	sql  queries
	wake chan struct{}
}

func newQueue(d *sql.DB, cfg Config, t table) *queue {
	return &queue{d: d, cfg: cfg.WithDefaults(), sql: sqlFor(t), wake: make(chan struct{}, 1)}
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// notify wakes the dispatcher without blocking; a pending wake-up already covers this one.
func (q *queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// insert stores a message through the caller's transaction, if any, and reports whether it was new.
func (q *queue) insert(ctx context.Context, endpoint, topic, key string, payload []byte) (bool, error) {
	now := ms(q.cfg.Now())
	res, err := db.Conn(ctx, q.d).ExecContext(ctx, q.sql.insert, endpoint, topic, key, payload, now, now, now)
	if err != nil {
		return false, fmt.Errorf("storing message: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("storing message: %w", err)
	}
	if n > 0 {
		q.notify()
	}
	return n > 0, nil
}

// claim leases up to limit due messages. Messages of blocked endpoints are skipped in SQL; admit is asked per
// remaining candidate (it may start a circuit probe) and release undoes an admit if the claim is not committed.
func (q *queue) claim(ctx context.Context, limit int, blocked []string, admit func(string) bool, release func(string)) ([]record, error) {
	if blocked == nil {
		blocked = []string{}
	}
	skip, err := json.Marshal(blocked)
	if err != nil {
		return nil, fmt.Errorf("claiming messages: %w", err)
	}
	var out []record
	err = db.Transactor{DB: q.d}.InTx(ctx, func(ctx context.Context) error {
		out = out[:0]
		now := q.cfg.Now()
		conn := db.Conn(ctx, q.d)
		rows, err := conn.QueryContext(ctx, q.sql.candidates, ms(now), string(skip), limit*4)
		if err != nil {
			return err
		}
		var cands []record
		for rows.Next() {
			var r record
			if err := rows.Scan(&r.id, &r.endpoint, &r.topic, &r.key, &r.payload, &r.attempts); err != nil {
				rows.Close()
				return err
			}
			cands = append(cands, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range cands {
			if len(out) == limit {
				break
			}
			if admit != nil && !admit(r.endpoint) {
				continue
			}
			out = append(out, r)
		}
		for i := range out {
			out[i].attempts++
			if _, err := conn.ExecContext(ctx, q.sql.claim, ms(now.Add(q.cfg.Lease)), ms(now), out[i].id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if release != nil {
			for _, r := range out {
				release(r.endpoint)
			}
		}
		return nil, fmt.Errorf("claiming messages: %w", err)
	}
	return out, nil
}

// finish records the outcome of an attempt: done, a retry after the backoff, or dead. It runs even if the
// dispatcher is stopping, so a completed delivery is not repeated after a graceful shutdown.
func (q *queue) finish(ctx context.Context, r record, cause error) (dead bool, err error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := q.cfg.Now()
	conn := db.Conn(ctx, q.d)
	switch {
	case cause == nil:
		_, err = conn.ExecContext(ctx, q.sql.done, ms(now), r.id)
	case isPermanent(cause) || r.attempts >= q.cfg.MaxAttempts:
		dead = true
		_, err = conn.ExecContext(ctx, q.sql.dead, truncate(cause.Error()), ms(now), r.id)
	default:
		_, err = conn.ExecContext(ctx, q.sql.reschedule, ms(now.Add(q.cfg.Backoff(r.attempts))), truncate(cause.Error()), ms(now), r.id)
	}
	if err != nil {
		return dead, fmt.Errorf("recording attempt of message %d: %w", r.id, err)
	}
	return dead, nil
}

func (q *queue) listDead(ctx context.Context, limit int) ([]DeadMessage, error) {
	rows, err := db.Conn(ctx, q.d).QueryContext(ctx, q.sql.listDead, limit)
	if err != nil {
		return nil, fmt.Errorf("listing dead messages: %w", err)
	}
	defer rows.Close()
	out := []DeadMessage{}
	for rows.Next() {
		var m DeadMessage
		var created, updated int64
		if err := rows.Scan(&m.ID, &m.Endpoint, &m.Topic, &m.Key, &m.Attempts, &m.LastError, &created, &updated); err != nil {
			return nil, fmt.Errorf("listing dead messages: %w", err)
		}
		m.CreatedAt, m.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing dead messages: %w", err)
	}
	return out, nil
}

func (q *queue) retry(ctx context.Context, id int64) error {
	now := ms(q.cfg.Now())
	res, err := db.Conn(ctx, q.d).ExecContext(ctx, q.sql.retry, now, now, id)
	if err != nil {
		return fmt.Errorf("retrying message %d: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("retrying message %d: %w", id, err)
	} else if n == 0 {
		return ErrNotFound
	}
	q.notify()
	return nil
}

func (q *queue) purge(ctx context.Context) (int64, error) {
	now := q.cfg.Now()
	res, err := db.Conn(ctx, q.d).ExecContext(ctx, q.sql.purge, ms(now.Add(-q.cfg.Retention)), ms(now.Add(-q.cfg.DeadRetention)))
	if err != nil {
		return 0, fmt.Errorf("purging messages: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purging messages: %w", err)
	}
	return n, nil
}

func (q *queue) stats(ctx context.Context) (map[string]Counts, error) {
	rows, err := db.Conn(ctx, q.d).QueryContext(ctx, q.sql.stats)
	if err != nil {
		return nil, fmt.Errorf("counting messages: %w", err)
	}
	defer rows.Close()
	out := map[string]Counts{}
	for rows.Next() {
		var e string
		var c Counts
		if err := rows.Scan(&e, &c.Pending, &c.Dead); err != nil {
			return nil, fmt.Errorf("counting messages: %w", err)
		}
		out[e] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting messages: %w", err)
	}
	return out, nil
}

// loop calls step until ctx ends: immediately, then on every wake-up, poll tick or extra signal, and purges
// old messages hourly. step reports whether it may have left due work behind (so it runs again at once).
func (q *queue) loop(ctx context.Context, extra <-chan struct{}, step func(context.Context) bool) {
	poll := time.NewTicker(q.cfg.PollInterval)
	defer poll.Stop()
	purge := time.NewTicker(time.Hour)
	defer purge.Stop()
	q.purgeLogged(ctx)
	for {
		more := step(ctx)
		if more {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-poll.C:
		case <-extra:
		case <-purge.C:
			q.purgeLogged(ctx)
		}
	}
}

func (q *queue) purgeLogged(ctx context.Context) {
	if n, err := q.purge(ctx); err != nil && !errors.Is(err, context.Canceled) {
		q.cfg.Logger.Error("purging finished messages", "err", err)
	} else if n > 0 {
		q.cfg.Logger.Info("purged finished messages", "count", n)
	}
}
