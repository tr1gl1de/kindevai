package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type session struct {
	t      *testing.T
	base   string
	client *http.Client
}

func (s session) do(method, path, body string) (int, string) {
	s.t.Helper()
	req, _ := http.NewRequestWithContext(s.t.Context(), method, s.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func ownerSession(t *testing.T, base string) session {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	s := session{t: t, base: base, client: &http.Client{Jar: jar}}
	if code, body := s.do("POST", "/api/auth/login", `{"login":"owner","password":"correct horse battery"}`); code != 200 {
		t.Fatalf("login = %d %s", code, body)
	}
	return s
}

// KIN-41 end to end: the Owner switches maintenance mode on (user changes are refused), exports the running
// installation with the key in a separate file, and imports it into an empty data directory; the imported
// Control Plane starts, keeps the accounts and is still in maintenance mode until the Owner switches it off.
func TestMaintenanceExportImportEndToEnd(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	base, stop := startServe(t, src)
	tok, _ := os.ReadFile(filepath.Join(src, "secrets", "setup-token"))
	setup := `{"token":"` + strings.TrimSpace(string(tok)) + `","login":"owner","password":"correct horse battery","platformName":"Acme"}`
	if code, body := call(t, "POST", base+"/api/setup", setup); code != 201 {
		t.Fatalf("setup = %d %s", code, body)
	}
	s := ownerSession(t, base)
	if code, body := s.do("PUT", "/api/maintenance", `{"enabled":true,"reason":"moving"}`); code != 200 {
		t.Fatalf("maintenance on = %d %s", code, body)
	}
	if code, body := s.do("POST", "/api/projects", `{"name":"Web","slug":"web"}`); code != 503 || !strings.Contains(body, "maintenance") {
		t.Fatalf("change in maintenance = %d %s", code, body)
	}

	out := t.TempDir()
	archive, key := filepath.Join(out, "backup.tar.gz"), filepath.Join(out, "backup.key")
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"export", "--data-dir", src, "--out", archive}, &stdout, &stderr, nil); err == nil {
		t.Fatal("export without a key decision succeeded")
	}
	if err := run(t.Context(), []string{"export", "--data-dir", src, "--out", archive, "--key-out", key}, &stdout, &stderr, nil); err != nil {
		t.Fatalf("export while serving: %v", err)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Fatalf("no key warning: %s", stderr.String())
	}
	stop()

	dst := filepath.Join(t.TempDir(), "dst")
	if err := run(t.Context(), []string{"import", "--data-dir", dst, "--archive", archive}, io.Discard, io.Discard, nil); err == nil ||
		!strings.Contains(err.Error(), "key") {
		t.Fatalf("import without key: %v", err)
	}
	if err := run(t.Context(), []string{"import", "--data-dir", dst, "--archive", archive, "--key", key}, io.Discard, io.Discard, nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	base, stop = startServe(t, dst)
	defer stop()
	s = ownerSession(t, base)
	if code, body := s.do("GET", "/api/maintenance", ""); code != 200 || !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("maintenance after import = %d %s", code, body)
	}
	if code, _ := s.do("PUT", "/api/maintenance", `{"enabled":false}`); code != 200 {
		t.Fatalf("maintenance off = %d", code)
	}
	if code, body := s.do("POST", "/api/projects", `{"name":"Web","slug":"web"}`); code != 201 {
		t.Fatalf("create project after maintenance = %d %s", code, body)
	}
}
