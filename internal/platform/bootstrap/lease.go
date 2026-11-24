package bootstrap

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/datadir"
)

// ErrInstanceActive means another Control Plane instance holds the database's instance record (KIN-41). It wraps
// datadir.ErrLocked: both say "this data is in use".
var ErrInstanceActive = fmt.Errorf("another Control Plane instance is active on this database: %w", datadir.ErrLocked)

// DefaultLeaseTTL is how long an instance record stays valid without a heartbeat; the record is renewed every
// third of it. After a crash a new instance waits at most this long.
const DefaultLeaseTTL = 30 * time.Second

const metaInstanceLock = "instance_lock"

// lease is the instance record in meta: the second half of the single-instance lock. The lock file (flock) stops a
// second process on the same host; the record also stops one that reaches the same database through a file system
// where flock is not reliable (network shares, some container volume drivers).
type lease struct {
	Holder      string    `json:"holder"` // random per process
	Host        string    `json:"host"`
	PID         int       `json:"pid"`
	StartedAt   time.Time `json:"startedAt"`
	HeartbeatAt time.Time `json:"heartbeatAt"`
}

type leaseKeeper struct {
	db     *sql.DB
	holder string
	ttl    time.Duration
	now    func() time.Time
	stop   context.CancelFunc
	wg     sync.WaitGroup
	lost   chan struct{} // closed when the record was taken over or could not be renewed for ttl
}

// acquireLease takes the instance record unless another instance renewed it within ttl, and keeps renewing it
// until release.
func acquireLease(ctx context.Context, d *sql.DB, ttl time.Duration, now func() time.Time) (*leaseKeeper, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("generating instance id: %w", err)
	}
	host, _ := os.Hostname()
	t := now().UTC()
	mine := lease{Holder: hex.EncodeToString(b[:]), Host: host, PID: os.Getpid(), StartedAt: t, HeartbeatAt: t}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("taking the instance record: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var v string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", metaInstanceLock).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("reading the instance record: %w", err)
	default:
		var other lease
		// The same host's previous process: it held the lock file we hold now, so it is gone (crash, kill -9) and
		// a restart must not wait for the record to expire.
		if json.Unmarshal([]byte(v), &other) == nil && t.Sub(other.HeartbeatAt) < ttl && other.Host != host {
			return nil, fmt.Errorf("%w (host %q, pid %d, last heartbeat %s); stop it first, or wait %s if it crashed",
				ErrInstanceActive, other.Host, other.PID, other.HeartbeatAt.Format(time.RFC3339), ttl)
		}
	}
	if err := putLease(ctx, tx, mine); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("taking the instance record: %w", err)
	}
	hctx, cancel := context.WithCancel(context.Background())
	k := &leaseKeeper{db: d, holder: mine.Holder, ttl: ttl, now: now, stop: cancel, lost: make(chan struct{})}
	k.wg.Go(func() { k.heartbeat(hctx, mine) })
	return k, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func putLease(ctx context.Context, x execer, l lease) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	if _, err := x.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, metaInstanceLock, string(b)); err != nil {
		return fmt.Errorf("writing the instance record: %w", err)
	}
	return nil
}

func (k *leaseKeeper) heartbeat(ctx context.Context, l lease) {
	last := l.HeartbeatAt
	t := time.NewTicker(k.ttl / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		at := k.now().UTC()
		l.HeartbeatAt = at
		// Only our own record is renewed; a failed write is retried on the next tick until ttl has passed.
		b, err := json.Marshal(l)
		if err != nil {
			continue
		}
		res, err := k.db.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = ? AND json_extract(value, '$.holder') = ?`,
			string(b), metaInstanceLock, k.holder)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if n, rerr := res.RowsAffected(); rerr == nil && n == 0 {
				close(k.lost) // another instance took the record over: we must stop writing
				return
			}
			last = at
			continue
		}
		if at.Sub(last) >= k.ttl {
			close(k.lost) // others may consider the record stale and start
			return
		}
	}
}

// release stops the heartbeat and deletes the record if it is still ours.
func (k *leaseKeeper) release() error {
	k.stop()
	k.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := k.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ? AND json_extract(value, '$.holder') = ?`,
		metaInstanceLock, k.holder); err != nil {
		return fmt.Errorf("releasing the instance record: %w", err)
	}
	return nil
}
