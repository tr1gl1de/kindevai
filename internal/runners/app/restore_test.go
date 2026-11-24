package app

import (
	"fmt"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// KIN-41 (closes the KIN-22 limit): the Control Plane database is restored from a backup (or imported) that is
// behind what the runner already applied. The restarted Control Plane is a new instance, so its command epoch on the
// wire differs: the runner resets its cursor (jumping to Base) and applies the new commands instead of skipping them
// as duplicates of seq numbers it saw before the restore, and its ACK is accepted.
func TestRestoredDatabaseDoesNotReuseAppliedSeqNumbers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	runner := protocol.Cursor{} // the runner's persisted command cursor survives the Control Plane restore

	before := newChannelEnv()
	before.ch.Instance = "boot1"
	for i := range 5 {
		if err := before.ch.Send(ctx, "r1", fmt.Sprintf("c%d", i), protocol.CommandCancel, protocol.Cancel{}); err != nil {
			t.Fatal(err)
		}
	}
	sess, f := newSession("r1")
	stop := runServe(t, before.ch, sess)
	for _, c := range commandsSent(t, f, 5) {
		v, next := runner.Receive(c.Epoch, c.Seq, c.Base)
		if v != protocol.Apply {
			t.Fatalf("before restore %+v: %s", c, v)
		}
		runner = next
	}
	stop()

	// The backup was taken after two commands were acknowledged; the same command log epoch is restored.
	restored := newChannelEnv()
	for i := range 2 {
		if err := restored.ch.Send(ctx, "r1", fmt.Sprintf("c%d", i), protocol.CommandCancel, protocol.Cancel{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.repo.AckCommands(ctx, "r1", 2, restored.ch.Now()); err != nil {
		t.Fatal(err)
	}
	restored.ch.Instance = "boot2"
	if err := restored.ch.Send(ctx, "r1", "new", protocol.CommandCancel, protocol.Cancel{}); err != nil {
		t.Fatal(err)
	}
	sess2, f2 := newSession("r1")
	runServe(t, restored.ch, sess2)
	c := commandsSent(t, f2, 1)[0]
	if c.ID != "new" || c.Seq != 3 {
		t.Fatalf("after restore %+v", c)
	}
	v, next := runner.Receive(c.Epoch, c.Seq, c.Base)
	if v != protocol.Apply {
		t.Fatalf("runner verdict on the restored log: %s (command %+v, cursor %+v)", v, c, runner)
	}
	if err := handle(t, restored.ch, sess2, protocol.TypeCommandAck, protocol.Ack{Epoch: next.Epoch, Seq: next.Last}); err != nil {
		t.Fatalf("ack after restore: %v", err)
	}
	if st, _, _ := restored.repo.Channel(ctx, "r1"); st.CommandAcked != 3 {
		t.Fatalf("acked %d, want 3", st.CommandAcked)
	}
	// An ACK from the runner's pre-restore epoch is ignored, not a protocol error.
	if err := handle(t, restored.ch, sess2, protocol.TypeCommandAck, protocol.Ack{Epoch: runner.Epoch, Seq: 5}); err != nil {
		t.Fatalf("stale epoch ack: %v", err)
	}
}
