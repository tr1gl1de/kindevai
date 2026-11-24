package youtrack_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

const tok = "perm:cm9ib3Q=.s3cr3tT0ken.Xy12AbCd"

func TestProbe(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok/api/users/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok || r.URL.Query().Get("fields") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"login":"robot","$type":"Me"}`))
	})
	mux.HandleFunc("GET /deny/api/users/me", func(w http.ResponseWriter, r *http.Request) {
		// A server that echoes the credential must not get it into the stored result.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad token ` + r.Header.Get("Authorization") + `"}`))
	})
	mux.HandleFunc("GET /broken/api/users/me", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	mux.HandleFunc("GET /garbage/api/users/me", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) })
	mux.HandleFunc("GET /redirect/api/users/me", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	secret, _ := domain.NewYouTrackToken(tok)
	p := &youtrack.Probe{Client: srv.Client()}
	cases := []struct {
		base string
		want domain.CheckResult
	}{
		{srv.URL + "/ok", domain.CheckResult{OK: true, Detail: "robot"}},
		{srv.URL + "/deny", domain.CheckResult{Code: "unauthorized", Detail: "HTTP 401"}},
		{srv.URL + "/missing", domain.CheckResult{Code: "not_found", Detail: "HTTP 404"}},
		{srv.URL + "/broken", domain.CheckResult{Code: "unexpected_status", Detail: "HTTP 502"}},
		{srv.URL + "/garbage", domain.CheckResult{Code: "bad_response"}},
		{srv.URL + "/redirect", domain.CheckResult{Code: "redirected", Detail: "HTTP 302"}},
	}
	for _, c := range cases {
		got := p.Probe(t.Context(), c.base, secret)
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.base, got, c.want)
		}
	}
	got := p.Probe(t.Context(), deadURL, secret)
	if got.OK || got.Code != "unreachable" || strings.Contains(got.Detail, "s3cr3t") {
		t.Errorf("unreachable: %+v", got)
	}
}
