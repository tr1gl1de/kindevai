package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
)

func TestServeHealthAndShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan net.Addr, 1)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	dataDir := filepath.Join(t.TempDir(), "data")
	go func() {
		done <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir}, io.Discard, &stderr, ready)
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr.String()+"/api/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("health = %d %s", resp.StatusCode, body)
	}

	for _, p := range []string{bootstrap.DBPath(dataDir), bootstrap.KeyPath(dataDir)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("bootstrap did not create %s: %v", p, err)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRunCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		wantOut string
	}{
		{"no command", nil, true, ""},
		{"unknown", []string{"bogus"}, true, ""},
		{"version", []string{"version"}, false, "dev"},
		{"help", []string{"help"}, false, "serve"},
		{"bad flag", []string{"serve", "--nope"}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := run(t.Context(), tt.args, &out, io.Discard, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestServeBadAddr(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), []string{"serve", "--addr", "256.0.0.1:99999", "--data-dir", t.TempDir()}, io.Discard, io.Discard, nil)
	if err == nil {
		t.Fatal("want listen error")
	}
}

func TestServeFailsWhenKeyMissingForExistingDB(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	inst, err := bootstrap.Run(t.Context(), bootstrap.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	_ = inst.Close()
	if err := os.Remove(bootstrap.KeyPath(dataDir)); err != nil {
		t.Fatal(err)
	}
	err = run(t.Context(), []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "encryption key") {
		t.Fatalf("err = %v, want a clear encryption key error", err)
	}
}

// startServe runs `serve` on a random port and returns its base URL and a stop function.
func startServe(t *testing.T, dataDir string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", dataDir}, io.Discard, io.Discard, ready)
	}()
	select {
	case addr := <-ready:
		stop := func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("serve: %v", err)
			}
		}
		return "http://" + addr.String(), stop
	case err := <-done:
		cancel()
		t.Fatalf("serve exited: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("serve did not start")
	}
	return "", nil
}

func call(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestFirstRunSetupEndToEnd(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	base, stop := startServe(t, dataDir)
	tokenFile := filepath.Join(dataDir, "secrets", "setup-token")
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("setup token file: %v", err)
	}
	if code, body := call(t, "GET", base+"/api/setup/status", ""); code != 200 || !strings.Contains(body, `"setupRequired":true`) {
		t.Fatalf("status = %d %s", code, body)
	}
	payload := `{"token":"` + strings.TrimSpace(string(tok)) + `","login":"owner","password":"correct horse battery","platformName":"Acme"}`
	if code, body := call(t, "POST", base+"/api/setup", payload); code != 201 {
		t.Fatalf("setup = %d %s", code, body)
	}
	stop()

	base, stop = startServe(t, dataDir)
	defer stop()
	if code, body := call(t, "GET", base+"/api/setup/status", ""); !strings.Contains(body, `"setupRequired":false`) {
		t.Fatalf("status after restart = %d %s", code, body)
	}
	if code, _ := call(t, "POST", base+"/api/setup", payload); code != 409 {
		t.Fatalf("setup after restart = %d, want 409", code)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("setup token file still present after setup")
	}
}
