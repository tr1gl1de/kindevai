package redact

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	ghToken  = "ghp_Ab1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx2"
	proxyPwd = "s3cret proxy pass"
)

// output is a realistic agent/CLI transcript with secrets of every kind the task names: git credentials in a URL,
// API keys, a proxy password (a known value, not secret-shaped), an env dump and a PEM key.
var output = strings.Join([]string{
	"Cloning into 'repo'...",
	"remote: https://x-access-token:" + ghToken + "@github.com/acme/repo.git",
	"  export ANTHROPIC_API_KEY=sk-ant-Ab12Cd34Ef56Gh78Ij90",
	"\tAuthorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl",
	"proxy: http://user:" + proxyPwd + "@proxy.local:3128 failed",
	"PASSWORD=hunter2",
	"-----BEGIN RSA PRIVATE KEY-----",
	"MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn",
	"abc",
	"-----END RSA PRIVATE KEY-----",
	"",
	"commit 3f2a9c1e5b7d4f6a8c0e2b4d6f8a0c2e4b6d8f0a done",
	"Done.\r",
	"",
}, "\n")

var leaks = []string{ghToken, "sk-ant-Ab12Cd34Ef56Gh78Ij90", "eyJhbGciOiJIUzI1NiJ9", proxyPwd, "hunter2",
	"MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn", "\nabc\n"}

func assertRedacted(t *testing.T, got string) {
	t.Helper()
	for _, s := range leaks {
		if strings.Contains(got, s) {
			t.Errorf("output leaks %q:\n%s", s, got)
		}
	}
	if g, w := strings.Count(got, "\n"), strings.Count(output, "\n"); g != w {
		t.Errorf("redaction changed the line count: %d lines, want %d:\n%s", g, w, got)
	}
	for _, keep := range []string{"Cloning into 'repo'...", "commit 3f2a9c1e5b7d4f6a8c0e2b4d6f8a0c2e4b6d8f0a done",
		"Done.\r\n", "  export ANTHROPIC_API_KEY=" + Placeholder, "\tAuthorization: Bearer " + Placeholder} {
		if !strings.Contains(got, keep) {
			t.Errorf("output lost %q:\n%s", keep, got)
		}
	}
}

func TestLinesRedactsSecretsAndKeepsLines(t *testing.T) {
	t.Parallel()
	assertRedacted(t, Lines(output, proxyPwd))
	if got := Lines("plain\n\nline two\n"); got != "plain\n\nline two\n" {
		t.Errorf("clean text changed: %q", got)
	}
}

func TestLinesIgnoresShortKnownValues(t *testing.T) {
	t.Parallel()
	// A short known value would mask ordinary words everywhere (and reveal itself by the pattern of masks).
	if got := Lines("the test passed", "test"); got != "the test passed" {
		t.Errorf("short known value masked: %q", got)
	}
}

// collect is a writer that records everything written to it.
type collect struct {
	mu  sync.Mutex
	buf bytes.Buffer
	n   int
}

func (c *collect) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.buf.Write(p)
}

func (c *collect) String() string { c.mu.Lock(); defer c.mu.Unlock(); return c.buf.String() }

func TestStreamRedactsSecretsSplitAtEveryChunkBoundary(t *testing.T) {
	t.Parallel()
	for _, size := range []int{1, 2, 3, 7, 13, 64, len(output)} {
		var c collect
		s := NewStream(&c, 0, proxyPwd)
		for rest := output; rest != ""; {
			n := min(size, len(rest))
			if k, err := s.Write([]byte(rest[:n])); err != nil || k != n {
				t.Fatalf("chunk %d: Write = %d, %v", size, k, err)
			}
			rest = rest[n:]
		}
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		got := c.String()
		assertRedacted(t, got)
		if want := Lines(output, proxyPwd); got != want {
			t.Errorf("chunks of %d differ from the whole text:\n%s\nwant:\n%s", size, got, want)
		}
	}
}

func TestStreamEmitsCompleteLinesAndHoldsThePartialOne(t *testing.T) {
	t.Parallel()
	var c collect
	s := NewStream(&c, 0)
	_, _ = s.Write([]byte("line one\ntoken: ghp_Ab1Cd2"))
	if got := c.String(); got != "line one\n" {
		t.Fatalf("emitted %q, want only the complete line", got)
	}
	_, _ = s.Write([]byte("Ef3Gh4Ij5Kl6\nnext"))
	if got := c.String(); strings.Contains(got, "Ef3Gh4") || !strings.HasSuffix(got, "token: "+Placeholder+"\n") {
		t.Fatalf("split token not masked: %q", got)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := c.String(); !strings.HasSuffix(got, "\nnext") {
		t.Errorf("Flush lost the partial line: %q", got)
	}
}

func TestStreamBoundsAVeryLongLine(t *testing.T) {
	t.Parallel()
	var c collect
	s := NewStream(&c, 0)
	word := "lorem ipsum "
	line := strings.Repeat(word, 4*MaxCarry/len(word)) + "password: hunter2 tail"
	for i := 0; i < len(line); i += 100 {
		_, _ = s.Write([]byte(line[i:min(i+100, len(line))]))
		if s.buffered() > MaxCarry {
			t.Fatalf("carry grew to %d bytes (limit %d)", s.buffered(), MaxCarry)
		}
	}
	_ = s.Flush()
	got := c.String()
	if strings.Contains(got, "hunter2") || strings.Count(got, "\n") != 0 {
		t.Errorf("long line: leaked or split into lines: ...%s", got[max(0, len(got)-80):])
	}
	if len(got) < len(line)-len("hunter2") {
		t.Errorf("long line lost text: %d bytes, want about %d", len(got), len(line))
	}
	// One word longer than the carry cannot be held whole: it is emitted (random-looking blobs are masked).
	var c2 collect
	s2 := NewStream(&c2, 0)
	blob := strings.Repeat("Zm9vYmFy0", 2*MaxCarry/9)
	_, _ = s2.Write([]byte(blob))
	if s2.buffered() > MaxCarry {
		t.Errorf("a long word is held whole: %d bytes", s2.buffered())
	}
}

func TestStreamFlushesAPartialLineWhenIdle(t *testing.T) {
	t.Parallel()
	var c collect
	s := NewStream(&c, 10*time.Millisecond)
	_, _ = s.Write([]byte("Continue? [y/n] "))
	deadline := time.Now().Add(5 * time.Second)
	for c.String() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.String(); got != "Continue? [y/n] " {
		t.Errorf("idle partial line = %q", got)
	}
	_ = s.Flush()
}

// BenchmarkStream measures redaction throughput of typical CLI output in pipe-sized writes.
func BenchmarkStream(b *testing.B) {
	chunk := []byte(strings.Repeat(output, 32<<10/len(output)))
	b.SetBytes(int64(len(chunk)))
	s := NewStream(io.Discard, 0, proxyPwd)
	for b.Loop() {
		_, _ = s.Write(chunk)
	}
}
