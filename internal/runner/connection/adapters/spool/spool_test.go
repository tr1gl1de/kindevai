package spool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
)

func ev(kind string, n int64, body string) app.SpooledEvent {
	var raw json.RawMessage
	if body != "" {
		raw, _ = json.Marshal(body)
	}
	return app.SpooledEvent{Step: "s1", Fence: 3, AttemptSeq: n, Kind: kind, At: 1000 + n, Body: raw}
}

func open(t *testing.T, dir string, cfg Config) *Spool {
	t.Helper()
	cfg.Dir = dir
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seqs(es []app.SpooledEvent) []int64 {
	out := make([]int64, 0, len(es))
	for _, e := range es {
		out = append(out, e.Seq)
	}
	return out
}

func TestAppendReadAckSurviveReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := open(t, dir, Config{SegmentBytes: 200})
	for i := int64(1); i <= 5; i++ {
		seq, err := s.Append(ev("execution.output", i, "line"))
		if err != nil || seq != i {
			t.Fatalf("Append %d = %d, %v", i, seq, err)
		}
	}
	got, err := s.Read(0, 10, 1<<20)
	if err != nil || !slices.Equal(seqs(got), []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("Read = %v, %v", seqs(got), err)
	}
	if got[2].AttemptSeq != 3 || got[2].Step != "s1" || string(got[2].Body) != `"line"` {
		t.Fatalf("record = %+v", got[2])
	}
	if got, _ := s.Read(3, 1, 1<<20); !slices.Equal(seqs(got), []int64{4}) {
		t.Fatalf("Read(3, 1) = %v", seqs(got))
	}
	if err := s.Ack(3); err != nil {
		t.Fatal(err)
	}
	epoch := s.State().Epoch
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := open(t, dir, Config{SegmentBytes: 200})
	st := s2.State()
	if st.Epoch != epoch || st.Acked != 3 || st.Last != 5 {
		t.Fatalf("after reopen state = %+v, want epoch %s acked 3 last 5", st, epoch)
	}
	if got, _ := s2.Read(st.Acked, 10, 1<<20); !slices.Equal(seqs(got), []int64{4, 5}) {
		t.Fatalf("unacked after reopen = %v", seqs(got))
	}
	if seq, _ := s2.Append(ev("execution.completed", 6, "")); seq != 6 {
		t.Fatalf("seq after reopen = %d", seq)
	}
}

func TestAckDeletesSegmentsAndFreesSpace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := open(t, dir, Config{SegmentBytes: 150})
	for i := int64(1); i <= 20; i++ {
		if _, err := s.Append(ev("execution.output", i, strings.Repeat("x", 40))); err != nil {
			t.Fatal(err)
		}
	}
	before := s.State().Bytes
	if n := segmentFiles(t, dir); n < 5 {
		t.Fatalf("segments = %d, want several", n)
	}
	if err := s.Ack(20); err != nil {
		t.Fatal(err)
	}
	if st := s.State(); st.Bytes >= before || st.Bytes > 150 {
		t.Fatalf("bytes after ack = %d (before %d)", st.Bytes, before)
	}
	if n := segmentFiles(t, dir); n > 1 {
		t.Fatalf("segments after full ack = %d", n)
	}
	if got, _ := s.Read(20, 10, 1<<20); len(got) != 0 {
		t.Fatalf("read after full ack = %v", seqs(got))
	}
}

func segmentFiles(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

// Overflow policy: output beyond the limit is refused (the caller counts it and reports execution.output.dropped);
// lifecycle events still fit in the reserve.
func TestFullSpoolRefusesOutputKeepsLifecycle(t *testing.T) {
	t.Parallel()
	s := open(t, t.TempDir(), Config{LimitBytes: 600, ReserveBytes: 300, SegmentBytes: 1 << 20})
	var full bool
	for i := int64(1); i < 100; i++ {
		_, err := s.Append(ev("execution.output", i, strings.Repeat("y", 50)))
		if errors.Is(err, app.ErrSpoolFull) {
			full = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !full {
		t.Fatal("output never hit the limit")
	}
	if st := s.State(); st.Bytes > 600 {
		t.Fatalf("output used %d bytes over the 600 limit", st.Bytes)
	}
	if _, err := s.Append(ev("execution.failed", 100, "boom")); err != nil {
		t.Fatalf("lifecycle event refused: %v", err)
	}
	for i := 0; ; i++ {
		_, err := s.Append(ev("execution.started", 101, ""))
		if errors.Is(err, app.ErrSpoolFull) {
			break
		}
		if i > 100 {
			t.Fatal("reserve is unbounded")
		}
	}
	if st := s.State(); st.Bytes > 900 {
		t.Fatalf("spool grew to %d, beyond limit + reserve", st.Bytes)
	}
}

// A crash in the middle of a write leaves a torn frame: it is cut off on open, the records before it survive.
func TestTornTailIsTruncated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := open(t, dir, Config{})
	for i := int64(1); i <= 3; i++ {
		if _, err := s.Append(ev("execution.output", i, "abc")); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	m, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	f, err := os.OpenFile(m[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte{0, 0, 0, 90, 1, 2, 3}) // a header promising 90 bytes, then the crash
	_ = f.Close()

	s2 := open(t, dir, Config{})
	if got, _ := s2.Read(0, 10, 1<<20); !slices.Equal(seqs(got), []int64{1, 2, 3}) {
		t.Fatalf("records after torn tail = %v", seqs(got))
	}
	if seq, err := s2.Append(ev("execution.completed", 4, "")); err != nil || seq != 4 {
		t.Fatalf("append after recovery = %d, %v", seq, err)
	}
	if got, _ := s2.Read(0, 10, 1<<20); !slices.Equal(seqs(got), []int64{1, 2, 3, 4}) {
		t.Fatalf("records after append = %v", seqs(got))
	}
}

func TestCorruptRecordIsDropped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := open(t, dir, Config{})
	for i := int64(1); i <= 3; i++ {
		_, _ = s.Append(ev("execution.output", i, "abc"))
	}
	_ = s.Close()
	m, _ := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	b, _ := os.ReadFile(m[0])
	b[len(b)-3] ^= 0xff // flip a byte of the last record
	if err := os.WriteFile(m[0], b, 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := open(t, dir, Config{})
	if got, _ := s2.Read(0, 10, 1<<20); !slices.Equal(seqs(got), []int64{1, 2}) {
		t.Fatalf("records = %v, want the intact prefix", seqs(got))
	}
}

func TestReadIsBoundedByBytes(t *testing.T) {
	t.Parallel()
	s := open(t, t.TempDir(), Config{})
	for i := int64(1); i <= 10; i++ {
		_, _ = s.Append(ev("execution.output", i, strings.Repeat("z", 100)))
	}
	got, err := s.Read(0, 100, 250)
	if err != nil || len(got) < 1 || len(got) > 3 {
		t.Fatalf("Read(maxBytes 250) = %d records, %v", len(got), err)
	}
	if got, _ := s.Read(0, 100, 1); len(got) != 1 {
		t.Fatalf("a tiny byte budget still returns one record, got %d", len(got))
	}
}

func TestPendingResults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := open(t, dir, Config{})
	_, _ = s.Append(ev("execution.started", 1, ""))
	_, _ = s.Append(app.SpooledEvent{Step: "s2", Fence: 7, AttemptSeq: 4, Kind: "execution.failed", At: 1})
	_, _ = s.Append(ev("execution.completed", 2, ""))
	_ = s.Ack(2)
	want := []app.PendingResult{{Step: "s1", Fence: 3, Kind: "execution.completed", Seq: 3}}
	if got := s.Pending(); !slices.Equal(got, want) {
		t.Fatalf("Pending = %+v, want %+v", got, want)
	}
	_ = s.Close()
	if got := open(t, dir, Config{}).Pending(); !slices.Equal(got, want) {
		t.Fatalf("Pending after reopen = %+v", got)
	}
}

func TestNewSpoolsGetDistinctEpochs(t *testing.T) {
	t.Parallel()
	a, b := open(t, t.TempDir(), Config{}), open(t, t.TempDir(), Config{})
	if a.State().Epoch == "" || a.State().Epoch == b.State().Epoch {
		t.Fatalf("epochs %q %q", a.State().Epoch, b.State().Epoch)
	}
	if a.State().LimitBytes != DefaultLimit {
		t.Fatalf("default limit = %d", a.State().LimitBytes)
	}
}
