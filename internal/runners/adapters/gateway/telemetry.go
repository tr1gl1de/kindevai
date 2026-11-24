package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// TelemetrySink receives a runner's telemetry (app.Telemetry).
type TelemetrySink interface {
	Heartbeat(ctx context.Context, runnerID, session string) error
	Capabilities(ctx context.Context, runnerID, session string, c domain.Capabilities) error
	Storage(ctx context.Context, machineID, session string, vs []domain.Volume) error
}

// TelemetryMessages handles runner.heartbeat, runner.capabilities and runner.storage (KIN-20). Payloads are
// decoded leniently (unknown fields are ignored, so newer runners stay compatible) but validated strictly: an
// invalid one closes the connection with CloseProtocolError. Times are the Control Plane's clock, never the
// runner's. A failure to store valid telemetry is logged and does not close the connection.
func TelemetryMessages(sink TelemetrySink, now func() time.Time, logger *slog.Logger) map[string]MessageFunc {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	stored := func(s app.Session, typ string, err error) error {
		var ve *domain.ValidationError
		if errors.As(err, &ve) {
			return err
		}
		if err != nil {
			logger.Error("storing runner telemetry", "runner", s.RunnerID, "type", typ, "err", err)
		}
		return nil
	}
	return map[string]MessageFunc{
		protocol.TypeHeartbeat: func(ctx context.Context, s app.Session, env protocol.Envelope) error {
			var hb protocol.Heartbeat
			if err := decode(env.Payload, &hb); err != nil {
				return err
			}
			return stored(s, env.Type, sink.Heartbeat(ctx, s.RunnerID, s.ID))
		},
		protocol.TypeCapabilities: func(ctx context.Context, s app.Session, env protocol.Envelope) error {
			var p protocol.Capabilities
			if err := decode(env.Payload, &p); err != nil {
				return err
			}
			if len(p.Tools) > domain.MaxTools {
				return fmt.Errorf("%s: too many tools", env.Type)
			}
			tools := make([]domain.Tool, 0, len(p.Tools))
			for _, t := range p.Tools {
				tools = append(tools, domain.Tool(t))
			}
			c, err := domain.NewCapabilities(p.OS, p.Arch, p.WSL, p.StrictEgress, tools, now())
			if err != nil {
				return err
			}
			return stored(s, env.Type, sink.Capabilities(ctx, s.RunnerID, s.ID, c))
		},
		protocol.TypeStorage: func(ctx context.Context, s app.Session, env protocol.Envelope) error {
			var p protocol.Storage
			if err := decode(env.Payload, &p); err != nil {
				return err
			}
			if len(p.Volumes) > domain.MaxVolumes {
				return fmt.Errorf("%s: too many volumes", env.Type)
			}
			at := now()
			vs := make([]domain.Volume, 0, len(p.Volumes))
			for _, v := range p.Volumes {
				if v.Role != protocol.VolumeWorkspaces && v.Role != protocol.VolumeWindowsHost { // "data" is the CP's own
					return fmt.Errorf("%s: unknown volume role", env.Type)
				}
				dv, err := domain.NewVolume(v.ID, v.Role, v.Path, v.TotalBytes, v.AvailableBytes, v.FreeBytes, at)
				if err != nil {
					return err
				}
				vs = append(vs, dv)
			}
			return stored(s, env.Type, sink.Storage(ctx, s.RunnerID, s.ID, vs))
		},
	}
}

// decode accepts a JSON object (or no payload) and ignores unknown fields.
func decode(raw json.RawMessage, v any) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] != '{' {
		return fmt.Errorf("payload is not an object")
	}
	return json.Unmarshal(raw, v)
}
