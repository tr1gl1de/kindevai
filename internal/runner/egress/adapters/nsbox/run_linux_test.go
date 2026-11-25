//go:build linux

package nsbox

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// A launcher without a usable run description starts nothing.
func TestRunMainWithoutDescription(t *testing.T) { // not parallel: t.Setenv (RunMain reads its environment)
	garbage := filepath.Join(t.TempDir(), "run.json")
	if err := os.WriteFile(garbage, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "relative.json", garbage} {
		t.Setenv(RunEnv, v)
		if code := RunMain(io.Discard); code != ExitUsage {
			t.Fatalf("RunEnv %q: exit %d, want %d", v, code, ExitUsage)
		}
	}
	if _, err := os.Stat(garbage); !os.IsNotExist(err) {
		t.Fatal("the run description was not deleted")
	}
}

func TestRelayedKeepsReservedCodes(t *testing.T) {
	t.Parallel()
	for in, want := range map[int]int{0: 0, 1: 1, 2: 2, ExitUsage: 1, ExitUnverified: 1, ExitAgentStart: 1, 130: 130} {
		if got := relayed(in); got != want {
			t.Errorf("relayed(%d) = %d, want %d", in, got, want)
		}
	}
}

// KIN-88: a sandbox that does not verify is recorded where the confined CLI cannot write (a status file in the
// runner's data directory, outside every directory bound into the sandbox), written before any CLI could start;
// the launcher reads it once. Without it an exit 121 is the CLI's own.
func TestRunMainRecordsTheRefusalOutsideTheSandbox(t *testing.T) { // not parallel: t.Setenv
	data := t.TempDir()
	attempt := filepath.Join(data, "attempts", "a1")
	if err := os.MkdirAll(attempt, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{Program: "/x", DataDir: data}
	desc := filepath.Join(attempt, "sandbox-run-1.json")
	raw := `{"request":{"dir":"` + attempt + `","profile":{"enforcement":"none"}},"argv":["/bin/true"],"dataDir":"` + data + `"}`
	if err := os.WriteFile(desc, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=/h", RunEnv + "=" + desc}
	if m.Refused(env) {
		t.Fatal("refused before the run")
	}
	t.Setenv(RunEnv, desc)
	if code := RunMain(io.Discard); code != ExitUnverified {
		t.Fatalf("exit %d, want %d", code, ExitUnverified)
	}
	if left, _ := filepath.Glob(filepath.Join(attempt, "*")); len(left) != 0 {
		t.Fatalf("the refusal is visible in the attempt directory: %v", left)
	}
	if !m.Refused(env) {
		t.Fatal("the refusal was not recorded")
	}
	if m.Refused(env) {
		t.Fatal("the refusal was read twice")
	}
	if m.Refused([]string{RunEnv + "=relative.json"}) || m.Refused(nil) {
		t.Fatal("refused without a run description")
	}
}

// KIN-88: Wrap refuses when the refusal status would be visible inside the sandbox (or there is no data directory).
func TestWrapNeedsAHiddenStatusDirectory(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	strict := egress.BoxRequest{Profile: protocol.NetworkProfile{Enforcement: "strict",
		Proxy: &protocol.NetworkProxy{Scheme: "socks5", Host: "p", Port: 1}}}
	for name, c := range map[string]struct {
		data, dir, workdir string
	}{
		"no data directory":          {"", t.TempDir(), ""},
		"attempt is the data dir":    {data, data, ""},
		"workdir holds the data dir": {data, t.TempDir(), filepath.Dir(data)},
	} {
		r := strict
		r.Dir, r.Workdir = c.dir, c.workdir
		if _, _, _, err := (&Manager{Program: "/x", DataDir: c.data}).Wrap(r, []string{"/bin/true"}, nil, "/"); err == nil {
			t.Errorf("%s: Wrap accepted it", name)
		}
	}
}
