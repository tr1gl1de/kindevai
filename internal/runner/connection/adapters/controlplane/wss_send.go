package controlplane

import (
	"context"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
)

var _ app.Sender = (*link)(nil)

// Send writes one envelope on the authorized connection (KIN-20 telemetry). The WebSocket library serializes
// concurrent writes, so it is safe alongside Wait and keepalive pings.
func (l *link) Send(ctx context.Context, typ string, payload any) error {
	if err := writeEnvelope(ctx, l.conn, typ, payload); err != nil {
		return closeError(err)
	}
	return nil
}
