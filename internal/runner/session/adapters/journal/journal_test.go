package journal

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

func TestJournalRoundTrip(t *testing.T) {
	t.Parallel()
	j := Dir{Path: filepath.Join(t.TempDir(), "attempts")}
	if recs, err := j.List(); err != nil || len(recs) != 0 {
		t.Fatalf("empty journal %v %v", recs, err)
	}
	dir, err := j.Prepare("STEP_1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("work dir %v %v", st, err)
	}
	r := domain.Record{Step: "STEP_1", Fence: 2, Execution: "E", Project: "P", Scenario: "gate", PID: 42, PGID: 42, Reserved: 256}
	if err := j.Save(r); err != nil {
		t.Fatal(err)
	}
	r.Reserved = 512
	if err := j.Save(r); err != nil {
		t.Fatal(err)
	}
	recs, err := j.List()
	if err != nil || len(recs) != 1 || recs[0].Reserved != 512 || recs[0].PID != 42 {
		t.Fatalf("records %+v %v", recs, err)
	}
	if err := j.Delete("STEP_1", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work dir left: %v", err)
	}
	if recs, _ := j.List(); len(recs) != 0 {
		t.Fatalf("records after delete %+v", recs)
	}
	if err := j.Delete("STEP_1", 2); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestJournalRefusesUnsafeNames(t *testing.T) {
	t.Parallel()
	j := Dir{Path: t.TempDir()}
	for _, step := range []string{"../x", "a/b", ""} {
		if _, err := j.Prepare(step, 1); err == nil {
			t.Errorf("Prepare(%q) succeeded", step)
		}
		if err := j.Save(domain.Record{Step: step, Fence: 1}); err == nil {
			t.Errorf("Save(%q) succeeded", step)
		}
	}
	if _, err := j.Prepare("ok", 0); err == nil {
		t.Error("fence 0 accepted")
	}
}
