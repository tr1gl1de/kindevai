//go:build linux

package nsbox

import (
	"io"
	"os"
	"path/filepath"
	"testing"
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
