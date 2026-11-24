package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServeHealthAndShutdown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan net.Addr, 1)
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve", "--addr", "127.0.0.1:0"}, io.Discard, &stderr, ready) }()

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
	err := run(t.Context(), []string{"serve", "--addr", "256.0.0.1:99999"}, io.Discard, io.Discard, nil)
	if err == nil {
		t.Fatal("want listen error")
	}
}
