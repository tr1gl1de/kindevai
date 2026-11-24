package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

func request(t *testing.T) app.EnrollRequest {
	t.Helper()
	tok, _ := domain.ParseToken("tok-abc")
	return app.EnrollRequest{Token: tok, PublicKey: "cHVi", Host: domain.Host{Hostname: "box", OS: "linux", Arch: "arm64", Version: "1.2.3"}}
}

func serve(t *testing.T, h http.HandlerFunc) domain.ServerURL {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	srv, err := domain.ParseServerURL(ts.URL + "/base/")
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestEnrollSendsContractRequest(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/base"+protocol.EnrollPath || r.URL.RawQuery != "" {
			t.Errorf("%s %s", r.Method, r.URL)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type %q", ct)
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var in protocol.EnrollRequest
		if err := dec.Decode(&in); err != nil {
			t.Error(err)
		}
		want := protocol.EnrollRequest{Token: "tok-abc", PublicKey: "cHVi", Hostname: "box", OS: "linux", Arch: "arm64", Version: "1.2.3"}
		if in != want {
			t.Errorf("body = %+v", in)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(protocol.EnrollResult{RunnerID: "r1", Status: "pending", Fingerprint: "SHA256:x"})
	})
	res, err := New().Enroll(t.Context(), srv, request(t))
	if err != nil || res != (app.EnrollResult{RunnerID: "r1", Status: "pending", Fingerprint: "SHA256:x"}) {
		t.Fatalf("res = %+v, %v", res, err)
	}
}

func TestEnrollMapsErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{401, `{"error":"token_invalid","message":"invalid enrollment token"}`, domain.ErrTokenInvalid},
		{410, `{"error":"token_used"}`, domain.ErrTokenUsed},
		{410, `{"error":"token_expired"}`, domain.ErrTokenExpired},
		{410, `{"error":"token_revoked"}`, domain.ErrTokenRevoked},
		{409, `{"error":"key_enrolled"}`, domain.ErrKeyEnrolled},
		{429, `{"error":"rate_limited"}`, domain.ErrRateLimited},
		{422, `{"error":"validation","message":"hostname_format","field":"hostname"}`, domain.ErrRejected},
		{500, `{"error":"internal"}`, ErrUnexpected},
		{502, `<html>bad gateway</html>`, ErrUnexpected},
		{200, `not json`, ErrUnexpected},
		{200, `{"runnerId":"","status":"pending","fingerprint":"x"}`, ErrUnexpected},
		{201, `{"runnerId":"r1\u001b[2J","status":"pending","fingerprint":"SHA256:x"}`, ErrUnexpected},
		{201, `{"runnerId":"r1","status":"\u001b[31mpending","fingerprint":"SHA256:x"}`, ErrUnexpected},
		{201, `{"runnerId":"r1","status":"pending","fingerprint":"SHA256:x\u001b"}`, ErrUnexpected},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			t.Parallel()
			srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := New().Enroll(t.Context(), srv, request(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), "tok-abc") {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

func TestEnrollValidationNamesField(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"error":"validation","message":"hostname_format","field":"hostname"}`)
	})
	_, err := New().Enroll(t.Context(), srv, request(t))
	if err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("err = %v", err)
	}
}

// A redirect could downgrade to http or resend the token elsewhere; the client never follows one.
func TestEnrollDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	var hit atomic.Bool
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			hit.Store(true)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	if _, err := New().Enroll(t.Context(), srv, request(t)); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Fatal("redirect followed")
	}
}

// AC: TLS verification is mandatory; a certificate the system does not trust fails the enrollment.
func TestEnrollVerifiesTLS(t *testing.T) {
	t.Parallel()
	var hit atomic.Bool
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	t.Cleanup(ts.Close)
	srv, err := domain.ParseServerURL(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New().Enroll(t.Context(), srv, request(t)); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err = %v", err)
	}
	if hit.Load() {
		t.Fatal("request reached an untrusted server")
	}
	// The same server works once its CA is trusted, so the failure above is about trust, not the protocol.
	c := New()
	c.rootCAs = ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	_, _ = c.Enroll(t.Context(), srv, request(t))
	if !hit.Load() {
		t.Fatal("trusted server not reached")
	}
}

func TestEnrollBoundsResponse(t *testing.T) {
	t.Parallel()
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"runnerId":"`+strings.Repeat("a", 1<<20)+`"}`)
	})
	if _, err := New().Enroll(t.Context(), srv, request(t)); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("err = %v", err)
	}
}

// Plain http goes only to a loopback address, even when "localhost" resolves elsewhere.
func TestPlainHTTPDialsOnlyLoopback(t *testing.T) {
	t.Parallel()
	if err := loopbackOnly("tcp4", "10.0.0.1:80", nil); err == nil {
		t.Fatal("non-loopback dial allowed")
	}
	if err := loopbackOnly("tcp6", "[::1]:80", nil); err != nil {
		t.Fatal(err)
	}
}
