//go:build unix

package cliagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// TestRealCLISmoke runs the installed CLIs once through the adapter. It spends subscription quota and needs the
// runner user's login, so it runs only with KINDEV_REAL_CLI_SMOKE=1 (never in CI):
//
//	KINDEV_REAL_CLI_SMOKE=1 go test -run TestRealCLISmoke -v ./internal/runner/session/adapters/cliagent/
//
// The proxy variables of the calling shell are passed on (on the host they are the operator's network config).
func TestRealCLISmoke(t *testing.T) {
	t.Parallel()
	if os.Getenv("KINDEV_REAL_CLI_SMOKE") != "1" {
		t.Skip("set KINDEV_REAL_CLI_SMOKE=1 to run the installed CLIs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, d := range []Dialect{Claude{}, Codex{}} {
		t.Run(d.Name(), func(t *testing.T) {
			t.Parallel()
			bin, err := exec.LookPath(d.Name())
			if err != nil {
				t.Skipf("%s is not installed", d.Name())
			}
			root := t.TempDir()
			ws, prompt := filepath.Join(root, "ws"), filepath.Join(root, "prompt.md")
			if err := os.Mkdir(ws, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("kindevai smoke\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(prompt, []byte("Read hello.txt and reply with exactly its content.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			a := &Adapter{Dialect: d, Config: Config{Binary: bin, Home: home, Env: env}}
			r, err := a.Start(t.Context(), domain.AgentRequest{Agent: d.Name(), Workdir: ws, PromptFile: prompt,
				Timeout: 3 * time.Minute}, Hooks{OnEvent: func(e domain.AgentEvent) { t.Logf("%s %s%s", e.Kind, e.Tool, e.Text) }})
			if err != nil {
				t.Fatal(err)
			}
			res := r.Wait()
			if ok, reason := res.Outcome(); !ok {
				t.Fatalf("%s failed: %s", d.Name(), reason)
			}
			t.Logf("session %s, version %s, usage %+v, result %q", res.Session, res.Version, res.Usage, res.Text)
			if res.Session == "" {
				t.Fatal("no native session id")
			}
			// Resume the native session.
			if err := os.WriteFile(prompt, []byte("Repeat your previous reply exactly.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err = a.Start(t.Context(), domain.AgentRequest{Agent: d.Name(), Workdir: ws, PromptFile: prompt,
				Resume: res.Session, Timeout: 3 * time.Minute}, Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			again := r.Wait()
			if ok, reason := again.Outcome(); !ok || again.Text != res.Text {
				t.Fatalf("resume: %s, result %q", reason, again.Text)
			}
			t.Logf("resumed session %s", again.Session)
		})
	}
}
