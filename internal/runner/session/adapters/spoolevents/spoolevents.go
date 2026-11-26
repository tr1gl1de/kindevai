// Package spoolevents gives the session context's executor attempt event streams on the connection context's
// channel (the durable event spool, KIN-22).
package spoolevents

import (
	"io"

	connapp "github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
)

// Events implements app.Events on a connection channel.
type Events struct{ Channel *connapp.Channel }

var _ app.Events = Events{}

type stream struct{ a *connapp.Attempt }

func (s stream) Emit(kind string, body any) error { return s.a.Emit(kind, body) }
func (s stream) Output(name string) io.Writer     { return s.a.Output(name) }

// Attempt opens the attempt's stream; with reserve it survives a runner restart (connection ResumeAttempt).
func (e Events) Attempt(step string, fence, after int64, reserve func(upTo int64) error) app.Stream {
	if reserve == nil {
		return stream{e.Channel.Attempt(step, fence)}
	}
	return stream{e.Channel.ResumeAttempt(step, fence, after, reserve)}
}
