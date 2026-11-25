//go:build integration && linux

package nsbox

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// TestRealClaudeInSandbox runs the installed Claude Code inside real strict egress sandboxes, through a CONNECT
// proxy on the host, logged in only by a long-lived OAuth token in CLAUDE_CODE_OAUTH_TOKEN (what
// `ai-runner cli-login claude` stores, KIN-86): no credentials file in the sandbox at all. A second attempt, in a
// fresh sandbox with the task's session store bound at ~/.claude/projects, resumes the first one's session. It
// spends subscription quota, so it runs only on request (never in CI):
//
//	KINDEV_REAL_CLI_SMOKE=1 KINDEV_SMOKE_CLAUDE_TOKEN=<token from `claude setup-token`> \
//	  go test -tags=integration -run TestRealClaudeInSandbox -v ./internal/runner/egress/adapters/nsbox/
//
// Without KINDEV_SMOKE_CLAUDE_TOKEN the access token of the runner user's own login is used (valid for hours). On
// a host that reaches the Internet only through a proxy, HTTPS_PROXY is the test proxy's upstream (dialOut). The
// test logs every destination the CLI asked the proxy for: the allowlist a strict profile needs.
func TestRealClaudeInSandbox(t *testing.T) {
	if os.Getenv("KINDEV_REAL_CLI_SMOKE") != "1" {
		t.Skip("set KINDEV_REAL_CLI_SMOKE=1 to run the installed CLI")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	token := os.Getenv("KINDEV_SMOKE_CLAUDE_TOKEN")
	if token == "" {
		token = hostAccessToken(t)
	}
	f := newFixture(t)
	var mu sync.Mutex
	var seen []string
	px := loggingUpstream(t, func(target string) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, target)
	})
	p := f.profile("203.0.113.7")
	p.Proxy = &px
	cliHome := filepath.Join(t.TempDir(), "cli-home")
	store := filepath.Join(f.m.DataDir, "cli-sessions", "p", "t", "claude")
	for _, d := range []string{cliHome, store} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.workdir, "hello.txt"), []byte("kindevai sandbox smoke\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	attempt := func(prompt string, extra ...string) string {
		box, err := f.m.Open(t.Context(), egress.BoxRequest{Dir: f.dir, Workdir: f.workdir, Profile: p,
			HomeBinds: []egress.HomeBind{{Source: store, Rel: ".claude/projects"}}})
		if err != nil {
			t.Fatal(err)
		}
		b := box.(*Box)
		defer b.Release()
		argv := append([]string{bin, "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}, extra...)
		cmd, err := b.Command(argv, []string{"HOME=" + cliHome, "PATH=/usr/local/bin:/usr/bin:/bin:" + filepath.Dir(bin),
			"LANG=C.UTF-8", "TERM=dumb", "NO_COLOR=1", "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_OAUTH_TOKEN=" + token}, f.workdir)
		if err != nil {
			t.Fatal(err)
		}
		var out, errs strings.Builder
		cmd.Stdin = strings.NewReader(prompt)
		cmd.Stdout, cmd.Stderr = &out, &errs
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		if err := b.Await(ctx); err != nil {
			t.Fatalf("verdict: %v\n%s", err, errs.String())
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
		case <-time.After(4 * time.Minute):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			t.Fatalf("claude hung\nstdout:\n%s\nstderr:\n%s", out.String(), errs.String())
		}
		if err != nil {
			t.Fatalf("claude: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errs.String())
		}
		if strings.Contains(out.String()+errs.String(), token) {
			t.Fatal("the token appeared in the CLI's output")
		}
		return out.String()
	}
	first := attempt("Read hello.txt and reply with exactly its content.\n")
	if !strings.Contains(first, "kindevai sandbox smoke") {
		t.Fatalf("first attempt:\n%s", first)
	}
	var session string
	for l := range strings.SplitSeq(first, "\n") {
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(l), &ev) == nil && ev.Type == "result" {
			session = ev.SessionID
		}
	}
	if session == "" {
		t.Fatalf("no session id:\n%s", first)
	}
	second := attempt("What exact text did you reply with in your previous answer? Reply with that text only.\n",
		"--resume", session)
	if !strings.Contains(second, "kindevai sandbox smoke") || strings.Contains(second, "No conversation found") {
		t.Fatalf("resume in a fresh sandbox:\n%s", second)
	}
	mu.Lock()
	t.Logf("resumed session %s; destinations asked for: %v", session, seen)
	mu.Unlock()
}

// hostAccessToken is the access token of the runner user's Claude login (never printed).
func hostAccessToken(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		t.Skip("no token: set KINDEV_SMOKE_CLAUDE_TOKEN")
	}
	var c struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &c) != nil || c.OAuth.AccessToken == "" || time.UnixMilli(c.OAuth.ExpiresAt).Before(time.Now().Add(10*time.Minute)) {
		t.Skip("the runner user's Claude access token is missing or about to expire: set KINDEV_SMOKE_CLAUDE_TOKEN")
	}
	return c.OAuth.AccessToken
}

// dialOut reaches target: the fixture's diagnostic server directly, anything else through the host's own
// HTTPS_PROXY when one is set (a host that reaches the Internet only through a proxy: a direct dial fails there
// and the CLI retries for minutes, which looked like a hang in KIN-80).
func dialOut(target string) (net.Conn, error) {
	d := net.Dialer{Timeout: 10 * time.Second}
	if strings.HasPrefix(target, "diag.test:") {
		return d.Dial("tcp", strings.Replace(target, "diag.test", "127.0.0.1", 1))
	}
	hp := os.Getenv("HTTPS_PROXY")
	if hp == "" {
		hp = os.Getenv("https_proxy")
	}
	u, err := url.Parse(hp)
	if hp == "" || err != nil || u.Scheme != "http" {
		return d.Dial("tcp", target)
	}
	c, err := d.Dial("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK || br.Buffered() > 0 {
		_ = c.Close()
		return nil, fmt.Errorf("the host proxy refused %s", target)
	}
	return c, nil
}

// loggingUpstream is an HTTP CONNECT proxy on the host that reaches any destination (dialOut) and reports each one.
func loggingUpstream(t *testing.T, seen func(string)) protocol.NetworkProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	t.Cleanup(func() { _ = l.Close() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				seen(req.RequestURI)
				up, err := dialOut(req.RequestURI)
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n\r\n")
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
				<-done
			})
		}
	})
	return protocol.NetworkProxy{Scheme: "http", Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port}
}
