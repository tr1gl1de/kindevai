package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCursorReceive(t *testing.T) {
	t.Parallel()
	at := func(epoch string, last int64) Cursor { return Cursor{Epoch: epoch, Last: last} }
	for _, tc := range []struct {
		name      string
		cur       Cursor
		epoch     string
		seq, base int64
		want      Verdict
		wantNext  Cursor
	}{
		{"first message of a stream", Cursor{}, "e1", 1, 0, Apply, at("e1", 1)},
		{"next in order", at("e1", 4), "e1", 5, 4, Apply, at("e1", 5)},
		{"duplicate", at("e1", 5), "e1", 5, 4, Duplicate, at("e1", 5)},
		{"old duplicate", at("e1", 5), "e1", 2, 1, Duplicate, at("e1", 5)},
		{"gap (a message was lost or reordered)", at("e1", 5), "e1", 7, 5, Gap, at("e1", 5)},
		{"new epoch restarts the stream", at("e1", 9), "e2", 1, 0, Apply, at("e2", 1)},
		{"new epoch not starting at 1 is a gap", at("e1", 9), "e2", 3, 0, Gap, at("e2", 0)},
		{"receiver lost its state: base moves it forward", at("e1", 2), "e1", 8, 7, Apply, at("e1", 8)},
		{"base forward then still a gap", at("e1", 2), "e1", 9, 7, Gap, at("e1", 7)},
		{"base behind the receiver is ignored", at("e1", 6), "e1", 7, 3, Apply, at("e1", 7)},
		{"zero seq", at("e1", 1), "e1", 0, 0, Invalid, at("e1", 1)},
		{"base not below seq", at("e1", 1), "e1", 2, 2, Invalid, at("e1", 1)},
		{"empty epoch", at("e1", 1), "", 2, 1, Invalid, at("e1", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, next := tc.cur.Receive(tc.epoch, tc.seq, tc.base)
			if v != tc.want || next != tc.wantNext {
				t.Fatalf("Receive = %v %+v, want %v %+v", v, next, tc.want, tc.wantNext)
			}
		})
	}
}

// A command or event at the size limits still fits one message: the envelope is bounded by MaxMessageBytes.
func TestChannelMessagesFitTheFrame(t *testing.T) {
	t.Parallel()
	worst := strings.Repeat("\x01", MaxOutputChunk) // control bytes escape to \u0001: the worst JSON expansion
	body, err := json.Marshal(Output{Stream: "stderr", Offset: 1 << 40, Data: worst})
	if err != nil {
		t.Fatal(err)
	}
	ev := Event{Epoch: strings.Repeat("e", MaxIDLen), Seq: 1 << 40, Base: 1<<40 - 1, Step: strings.Repeat("s", MaxIDLen),
		Fence: 1 << 40, AttemptSeq: 1 << 40, Kind: EventOutput, At: 1 << 50, Body: body}
	raw, _ := json.Marshal(ev)
	env, _ := json.Marshal(Envelope{Type: TypeEvent, Payload: raw})
	if len(env) > MaxMessageBytes {
		t.Fatalf("worst-case output event is %d bytes, limit %d", len(env), MaxMessageBytes)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestEventValidate(t *testing.T) {
	t.Parallel()
	ok := Event{Epoch: "e", Seq: 2, Base: 1, Step: "s", Fence: 1, AttemptSeq: 1, Kind: EventStarted, At: 1}
	for name, mut := range map[string]func(*Event){
		"no step":         func(e *Event) { e.Step = "" },
		"zero fence":      func(e *Event) { e.Fence = 0 },
		"zero attempt":    func(e *Event) { e.AttemptSeq = 0 },
		"unknown kind":    func(e *Event) { e.Kind = "execution.exploded" },
		"long step":       func(e *Event) { e.Step = strings.Repeat("s", MaxIDLen+1) },
		"control in step": func(e *Event) { e.Step = "s\n" },
		"big body":        func(e *Event) { e.Body = json.RawMessage(`"` + strings.Repeat("a", MaxBodyBytes) + `"`) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := ok
			mut(&e)
			if e.Validate() == nil {
				t.Fatal("Validate accepted an invalid event")
			}
		})
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandValidate(t *testing.T) {
	t.Parallel()
	ok := Command{Epoch: "e", Seq: 1, ID: "c1", Kind: CommandCancel, Body: json.RawMessage(`{}`)}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Command){
		"no id":      func(c *Command) { c.ID = "" },
		"no kind":    func(c *Command) { c.Kind = "" },
		"bad kind":   func(c *Command) { c.Kind = "Exec Assign" },
		"zero seq":   func(c *Command) { c.Seq = 0 },
		"base ≥ seq": func(c *Command) { c.Base = 1 },
		"big body":   func(c *Command) { c.Body = json.RawMessage(`"` + strings.Repeat("a", MaxBodyBytes) + `"`) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := ok
			mut(&c)
			if c.Validate() == nil {
				t.Fatal("Validate accepted an invalid command")
			}
		})
	}
}
