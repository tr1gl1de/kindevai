package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

type storedLog []domain.AttemptEvent

func (l storedLog) AttemptEvents(context.Context, string, int64, int64, int) ([]domain.AttemptEvent, error) {
	return l, nil
}

func (l storedLog) LatestAttemptEvent(context.Context, string, int64, string) (domain.AttemptEvent, bool, error) {
	return domain.AttemptEvent{}, false, nil
}

// KIN-77: the task UI's attempt log masks secrets again when it reads them (defence in depth: events stored before
// ingest redaction, or by a future path that skips it), keeping the lines.
func TestAttemptLogRedactsStoredOutput(t *testing.T) {
	t.Parallel()
	out, _ := json.Marshal(protocol.Output{Stream: "stdout", Data: "a\ntoken=ghp_Ab1Cd2Ef3Gh4Ij5Kl6Mn7\nb\n"})
	fail, _ := json.Marshal(protocol.Failure{Reason: "Bearer ghs_Ab1Cd2Ef3Gh4Ij5Kl6"})
	l := AttemptLog{Repo: storedLog{{Kind: protocol.EventOutput, Body: out}, {Kind: protocol.EventFailed, Body: fail}}}
	evs, err := l.Events(t.Context(), "s1", 1, 0, 10)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events %v, %v", evs, err)
	}
	var o protocol.Output
	_ = json.Unmarshal(evs[0].Body, &o)
	if o.Data != "a\ntoken=[redacted]\nb\n" {
		t.Errorf("output %q", o.Data)
	}
	if b := string(evs[1].Body); strings.Contains(b, "ghs_") {
		t.Errorf("failure %s", b)
	}
}
