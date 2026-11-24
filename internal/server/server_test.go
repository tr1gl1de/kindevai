package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"favicon.svg":      {Data: []byte("<svg/>")},
		"assets/style.css": {Data: []byte("body{}")},
	}
}

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

func TestHealth(t *testing.T) {
	t.Parallel()
	h := New(Options{Version: "v-test", Assets: testAssets()})
	rec := do(t, h, http.MethodGet, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" || body["version"] != "v-test" {
		t.Fatalf("body = %v", body)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff header = %q", got)
	}
}

func TestRouting(t *testing.T) {
	t.Parallel()
	h := New(Options{Assets: testAssets()})
	tests := []struct {
		name, method, target string
		wantCode             int
		wantBody             string
		wantCache            string
	}{
		{"root serves index", http.MethodGet, "/", 200, "<html>app</html>", "no-cache"},
		{"client route falls back to index", http.MethodGet, "/projects/42", 200, "<html>app</html>", "no-cache"},
		{"explicit index", http.MethodGet, "/index.html", 200, "<html>app</html>", "no-cache"},
		{"hashed asset is immutable", http.MethodGet, "/assets/app-1.js", 200, "console.log(1)", "public, max-age=31536000, immutable"},
		{"root file", http.MethodGet, "/favicon.svg", 200, "<svg/>", ""},
		{"missing asset is 404", http.MethodGet, "/assets/missing.js", 404, "", ""},
		{"dot segments are redirected to the clean path", http.MethodGet, "/../../etc/passwd", 307, "", ""},
		{"encoded traversal stays inside assets", http.MethodGet, "/assets/..%2f..%2fetc%2fpasswd", 200, "<html>app</html>", "no-cache"},
		{"unknown api is JSON 404", http.MethodGet, "/api/nope", 404, `"error":"not_found"`, "no-store"},
		{"health wrong method", http.MethodPost, "/api/health", 404, `"error":"not_found"`, "no-store"},
		{"spa rejects POST", http.MethodPost, "/projects", 405, "", ""},
		{"HEAD on route", http.MethodHead, "/projects", 200, "", "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, h, tt.method, tt.target)
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantBody != "" && !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want to contain %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantCache != "" && rec.Header().Get("Cache-Control") != tt.wantCache {
				t.Fatalf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tt.wantCache)
			}
		})
	}
}
