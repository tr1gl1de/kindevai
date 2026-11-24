package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
)

type purger struct {
	left   int
	before []time.Time
	limits []int
}

func (p *purger) DeleteClosed(_ context.Context, before time.Time, limit int) (int64, error) {
	p.before, p.limits = append(p.before, before), append(p.limits, limit)
	n := min(p.left, limit)
	p.left -= n
	return int64(n), nil
}

// KIN-60: closed alerts older than the retention period are purged in bounded batches until none is left.
func TestRetentionPurgesClosedAlertsInBatches(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	p := &purger{left: 2*app.RetentionBatch + 3}
	r := &app.Retention{Repo: p, Now: func() time.Time { return now }}
	n, err := r.Purge(t.Context())
	if err != nil || n != int64(2*app.RetentionBatch+3) {
		t.Fatalf("purged %d, %v", n, err)
	}
	if len(p.limits) != 3 {
		t.Fatalf("batches = %v", p.limits)
	}
	for i, b := range p.before {
		if !b.Equal(now.Add(-app.AlertRetention)) || p.limits[i] != app.RetentionBatch {
			t.Fatalf("batch %d: before %v limit %d", i, b, p.limits[i])
		}
	}
	if app.AlertRetention != 90*24*time.Hour {
		t.Fatalf("retention = %v", app.AlertRetention)
	}
}
