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
	if err != nil || len(got) != 1 || got[0].ID != a.ID || !slices.Equal(got[0].Tools, []string{"git"}) ||
		!got[0].DiskHeavyAllowed {
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

// KIN-76: a candidate carries its declared parallel-step limit and the level of its fullest volume below critical.
func TestCandidateLimitAndDiskLevel(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	ctx := t.Context()
	a := f.approved(t, keyA)
	if err := f.tel.Heartbeat(ctx, a.ID, "s1"); err != nil {
		t.Fatal(err)
	}
	c, _ := domain.NewCapabilities("linux", "amd64", false, false, nil, f.clock.Now())
	c, _ = c.WithMaxParallelSteps(2)
	if err := f.tel.Capabilities(ctx, a.ID, "s1", c); err != nil {
		t.Fatal(err)
	}
	online := func(string) bool { return true }
	if got, _ := f.tel.Candidates(ctx, "p1", online); len(got) != 1 || got[0].MaxParallelSteps != 2 || got[0].Disk != domain.LevelOK {
		t.Fatalf("candidates %+v", got)
	}
	f.clock.add(time.Second)
	host, err := domain.NewVolume("/mnt/c", domain.RoleWindowsHost, "/mnt/c", 100*gib, 15*gib, 15*gib, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := f.tel.Storage(ctx, a.ID, "s1", []domain.Volume{volume(t, domain.RoleWorkspaces, 100*gib, 50*gib, f.clock.Now()),
		host}); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.tel.Candidates(ctx, "p1", online); len(got) != 1 || got[0].Disk != domain.LevelHigh || !got[0].DiskHeavyAllowed {
		t.Fatalf("high disk: %+v", got)
	}
}
