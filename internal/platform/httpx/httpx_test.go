package httpx

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientIPIgnoresProxyHeaders(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "203.0.113.5:4321"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.Header.Set("Forwarded", "for=127.0.0.1")
	if got := ClientIP(r); got != "203.0.113.5" {
		t.Fatalf("ClientIP = %q", got)
	}
}

func TestDecodeJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, ctype, body string
		ok                bool
		code              int
	}{
		{"ok", "application/json", `{"a":"x"}`, true, 200},
		{"charset ok", "application/json; charset=utf-8", `{"a":"x"}`, true, 200},
		{"trailing", "application/json", `{"a":"x"}{"a":"y"}`, false, 400},
		{"unknown field", "application/json", `{"b":1}`, false, 400},
		{"text/plain", "text/plain", `{"a":"x"}`, false, 415},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.ctype)
			rec := httptest.NewRecorder()
			var dst struct {
				A string `json:"a"`
			}
			ok := DecodeJSON(rec, r, &dst)
			if ok != tt.ok {
				t.Fatalf("ok = %v", ok)
			}
			if !ok && rec.Code != tt.code {
				t.Fatalf("code = %d, want %d", rec.Code, tt.code)
			}
			if ok && dst.A != "x" {
				t.Fatalf("dst = %+v", dst)
			}
		})
	}
}
