package httpx

import (
	"net/http"
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

func TestTrustedProxies(t *testing.T) {
	t.Parallel()
	tp, err := ParseTrustedProxies("172.29.240.0/24, 10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTrustedProxies("not-a-cidr"); err == nil {
		t.Fatal("want error for a bad CIDR")
	}
	tests := []struct {
		name, peer, xff, want string
	}{
		{"untrusted peer keeps its address", "203.0.113.9:1", "1.2.3.4", "203.0.113.9"},
		{"trusted proxy forwards the client", "172.29.240.5:1", "198.51.100.7", "198.51.100.7"},
		{"spoofed prefix is ignored", "172.29.240.5:1", "127.0.0.1, 198.51.100.7", "198.51.100.7"},
		{"chain of trusted proxies", "172.29.240.5:1", "198.51.100.7, 10.0.0.1", "198.51.100.7"},
		{"trusted proxy without header", "172.29.240.5:1", "", "172.29.240.5"},
		{"garbage header", "172.29.240.5:1", "nonsense", "172.29.240.5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got string
			h := tp.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientIP(r) }))
			r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
			r.RemoteAddr = tt.peer
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)
			if got != tt.want {
				t.Fatalf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
	var empty TrustedProxies
	var got string
	h := empty.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = ClientIP(r) }))
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "172.29.240.5:1"
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got != "172.29.240.5" {
		t.Fatalf("no trusted proxies configured, but header was used: %q", got)
	}
}

func TestPeerAndForwardedProto(t *testing.T) {
	t.Parallel()
	tp, _ := ParseTrustedProxies("172.29.240.0/24")
	tests := []struct {
		name, peer, xff, proto string
		wantPeer, wantProto    string
	}{
		{"direct request", "127.0.0.1:5", "", "https", "127.0.0.1", ""},
		{"untrusted peer cannot claim https", "203.0.113.9:1", "1.2.3.4", "https", "203.0.113.9", ""},
		{"trusted proxy keeps the TCP peer", "172.29.240.5:1", "198.51.100.7", "HTTPS", "172.29.240.5", "https"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var peer, proto string
			h := tp.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				peer, proto = Peer(r).String(), ForwardedProto(r)
			}))
			r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
			r.RemoteAddr = tt.peer
			r.Header.Set("X-Forwarded-For", tt.xff)
			r.Header.Set("X-Forwarded-Proto", tt.proto)
			h.ServeHTTP(httptest.NewRecorder(), r)
			if peer != tt.wantPeer || proto != tt.wantProto {
				t.Fatalf("peer %q proto %q, want %q %q", peer, proto, tt.wantPeer, tt.wantProto)
			}
		})
	}
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
	r.RemoteAddr = "[::1]:9"
	if Peer(r).String() != "::1" || ForwardedProto(r) != "" {
		t.Fatal("without the middleware Peer is RemoteAddr and no proto is trusted")
	}
}
