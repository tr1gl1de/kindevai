package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

func named(n, enf string) protocol.NetworkProfile {
	return protocol.NetworkProfile{Name: n, Enforcement: enf, DNS: "proxy"}
}

func TestProfileFor(t *testing.T) {
	t.Parallel()
	c := protocol.NetworkConfig{Default: named("default", "off"), Scoped: []protocol.ScopedNetworkProfile{
		{Project: "p1", Role: "claude", Profile: named("p1-claude", "strict")},
		{Project: "p1", Profile: named("p1", "monitor")},
		{Role: "codex", Profile: named("codex", "strict")},
	}}
	tests := []struct {
		project, role, want string
	}{
		{"p1", "claude", "p1-claude"},
		{"p1", "codex", "p1"}, // project before role
		{"p1", "", "p1"},
		{"p2", "codex", "codex"},
		{"p2", "claude", "default"},
		{"", "", "default"},
		{"p2", "mock", "default"}, // not a CLI role
	}
	for _, tc := range tests {
		t.Run(tc.project+"/"+tc.role, func(t *testing.T) {
			t.Parallel()
			if got := ProfileFor(c, tc.project, tc.role).Name; got != tc.want {
				t.Fatalf("ProfileFor(%q, %q) = %q, want %q", tc.project, tc.role, got, tc.want)
			}
		})
	}
}

type fakeBox struct{ released bool }

func (b *fakeBox) Release() { b.released = true }

type fakeBoxes struct {
	mu      sync.Mutex
	ok      bool
	why     string
	support int
	opened  []BoxRequest
	err     error
}

func (f *fakeBoxes) Open(_ context.Context, r BoxRequest) (Box, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.opened = append(f.opened, r)
	return &fakeBox{}, nil
}

func (f *fakeBoxes) Support(context.Context) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.support++
	return f.ok, f.why
}

func config(enf string) func() (protocol.ConfigUpdate, bool, error) {
	return func() (protocol.ConfigUpdate, bool, error) {
		return protocol.ConfigUpdate{Revision: 1, Network: protocol.NetworkConfig{Default: named("d", enf)}}, true, nil
	}
}

func TestConfine(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	t.Run("not strict: no sandbox", func(t *testing.T) {
		t.Parallel()
		for _, enf := range []string{"off", "monitor"} {
			b := &fakeBoxes{ok: true}
			box, err := (&Sandboxes{Config: config(enf), Boxes: b}).Confine(ctx, "p", "", BoxRequest{Dir: "/d", Workdir: "/w"})
			if box != nil || err != nil || len(b.opened) != 0 {
				t.Fatalf("%s: box %v, err %v, opened %v", enf, box, err, b.opened)
			}
		}
	})
	t.Run("no configuration yet fails closed", func(t *testing.T) {
		t.Parallel()
		none := func() (protocol.ConfigUpdate, bool, error) { return protocol.ConfigUpdate{}, false, nil }
		if box, err := (&Sandboxes{Config: none, Boxes: &fakeBoxes{}}).Confine(ctx, "p", "", BoxRequest{Dir: "/d", Workdir: ""}); box != nil || err == nil {
			t.Fatalf("box %v, err %v", box, err)
		}
	})
	t.Run("strict on a verified runner: a sandbox for the profile", func(t *testing.T) {
		t.Parallel()
		b := &fakeBoxes{ok: true}
		box, err := (&Sandboxes{Config: config("strict"), Boxes: b}).Confine(ctx, "p", "claude", BoxRequest{Dir: "/d", Workdir: "/w"})
		if err != nil || box == nil {
			t.Fatalf("box %v, err %v", box, err)
		}
		if len(b.opened) != 1 || b.opened[0].Dir != "/d" || b.opened[0].Workdir != "/w" || b.opened[0].Profile.Enforcement != "strict" {
			t.Fatalf("opened %+v", b.opened)
		}
	})
	t.Run("strict on an unsupported runner fails closed", func(t *testing.T) {
		t.Parallel()
		b := &fakeBoxes{why: "unsupported on darwin"}
		box, err := (&Sandboxes{Config: config("strict"), Boxes: b}).Confine(ctx, "p", "", BoxRequest{Dir: "/d", Workdir: ""})
		if box != nil || !errors.Is(err, ErrUnsupported) || len(b.opened) != 0 {
			t.Fatalf("box %v, err %v, opened %v", box, err, b.opened)
		}
	})
	t.Run("a sandbox that cannot be built fails closed", func(t *testing.T) {
		t.Parallel()
		b := &fakeBoxes{ok: true, err: errors.New("nft failed")}
		if box, err := (&Sandboxes{Config: config("strict"), Boxes: b}).Confine(ctx, "p", "", BoxRequest{Dir: "/d", Workdir: ""}); box != nil || err == nil {
			t.Fatalf("box %v, err %v", box, err)
		}
	})
	t.Run("an unreadable configuration fails closed", func(t *testing.T) {
		t.Parallel()
		bad := func() (protocol.ConfigUpdate, bool, error) {
			return protocol.ConfigUpdate{}, false, errors.New("corrupt")
		}
		if box, err := (&Sandboxes{Config: bad, Boxes: &fakeBoxes{ok: true}}).Confine(ctx, "p", "", BoxRequest{Dir: "/d", Workdir: ""}); box != nil || err == nil {
			t.Fatalf("box %v, err %v", box, err)
		}
	})
}

func TestSupportedIsCached(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	b := &fakeBoxes{ok: true}
	s := &Sandboxes{Boxes: b, MaxAge: time.Minute, Now: func() time.Time { return now }}
	for range 3 {
		if ok, _ := s.Supported(t.Context()); !ok {
			t.Fatal("not supported")
		}
	}
	if b.support != 1 {
		t.Fatalf("self-test ran %d times, want 1", b.support)
	}
	now = now.Add(time.Minute)
	b.ok, b.why = false, "self-test failed"
	if ok, why := s.Supported(t.Context()); ok || why != "self-test failed" || b.support != 2 {
		t.Fatalf("after MaxAge: %v %q (runs %d)", ok, why, b.support)
	}
}
