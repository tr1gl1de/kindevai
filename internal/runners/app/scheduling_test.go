package app

import (
	"slices"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Only APPROVED runners granted the project, connected and online by their heartbeats are candidates; a PENDING
// runner never gets work, an UNREACHABLE one neither.
func TestCandidates(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	ctx := t.Context()
	a := f.approved(t, keyA)
	pending := f.enrolled(t, keyB)
	for _, id := range []string{a.ID, pending.ID} {
		if err := f.tel.Heartbeat(ctx, id, "s1"); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := domain.NewCapabilities("linux", "amd64", false, false, []domain.Tool{{Name: "git", Version: "2"}}, f.clock.Now())
	if err := f.tel.Capabilities(ctx, a.ID, "s1", c); err != nil {
		t.Fatal(err)
	}
	connected := map[string]bool{a.ID: true, pending.ID: true}
	online := func(id string) bool { return connected[id] }

	got, err := f.tel.Candidates(ctx, "p1", online)
	if err != nil || len(got) != 1 || got[0].ID != a.ID || !slices.Equal(got[0].Tools, []string{"git"}) || !got[0].DiskHeavyAllowed {
		t.Fatalf("candidates %+v, %v", got, err)
	}
	if got, _ := f.tel.Candidates(ctx, "p2", online); len(got) != 0 {
		t.Fatalf("project not granted: %+v", got)
	}
	f.clock.add(time.Second)
	if err := f.tel.Storage(ctx, a.ID, "s1", []domain.Volume{volume(t, domain.RoleWorkspaces, 100*gib, gib, f.clock.Now())}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.tel.Candidates(ctx, "p1", online); len(got) != 1 || got[0].DiskHeavyAllowed {
		t.Fatalf("critical disk: %+v", got)
	}
	connected[a.ID] = false
	if got, _ := f.tel.Candidates(ctx, "p1", online); len(got) != 0 {
		t.Fatalf("disconnected: %+v", got)
	}
	connected[a.ID] = true
	f.clock.add(time.Minute)
	if err := f.tel.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.tel.Candidates(ctx, "p1", online); len(got) != 0 {
		t.Fatalf("unreachable: %+v", got)
	}
}
