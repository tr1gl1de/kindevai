package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/maintenance/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/maintenance/app"
	"github.com/tr1gl1de/kindevai/internal/maintenance/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

type memStore struct{ m domain.Mode }

func (s *memStore) Load(context.Context) (domain.Mode, error) { return s.m, nil }
func (s *memStore) Save(_ context.Context, m domain.Mode) error {
	s.m = m
	return nil
}

type nopAudit struct{}

func (nopAudit) Record(context.Context, auditapp.Entry) error { return nil }

type passTx struct{}

func (passTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

func newServer(t *testing.T) http.Handler {
	t.Helper()
	svc := &app.Service{Store: &memStore{}, Audit: nopAudit{}, Tx: passTx{}, Now: time.Now}
	h := &httpapi.Handler{Service: svc}
	mux := http.NewServeMux()
	h.Register(mux)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return h.ReadOnly(mux)
}

func do(t *testing.T, h http.Handler, p *auth.Principal, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if p != nil {
		r = r.WithContext(auth.WithPrincipal(r.Context(), *p))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Owner-only switch; every signed-in user sees the mode (banner); while it is on user changes get 503 maintenance
// and reads still work.
func TestMaintenanceAPIAndReadOnly(t *testing.T) {
	t.Parallel()
	h := newServer(t)
	owner := &auth.Principal{UserID: "u1", Login: "owner", Owner: true}
	dev := &auth.Principal{UserID: "u2", Login: "dev"}
	if w := do(t, h, nil, "GET", "/api/maintenance", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous get %d", w.Code)
	}
	if w := do(t, h, dev, "PUT", "/api/maintenance", `{"enabled":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("member put %d", w.Code)
	}
	if w := do(t, h, owner, "PUT", "/api/maintenance", `{"enabled":true,"reason":"`+strings.Repeat("x", 501)+`"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("long reason %d", w.Code)
	}
	if w := do(t, h, dev, "POST", "/api/tasks", `{}`); w.Code != http.StatusNoContent {
		t.Fatalf("before maintenance %d", w.Code)
	}
	w := do(t, h, owner, "PUT", "/api/maintenance", `{"enabled":true,"reason":"moving"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("enable %d %s", w.Code, w.Body)
	}
	var v httpapi.StatusView
	if err := json.Unmarshal(do(t, h, dev, "GET", "/api/maintenance", "").Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || v.Reason != "moving" || v.By != "owner" || v.Since == nil {
		t.Fatalf("view %+v", v)
	}
	if w := do(t, h, owner, "POST", "/api/tasks", `{}`); w.Code != http.StatusServiceUnavailable ||
		!strings.Contains(w.Body.String(), `"maintenance"`) {
		t.Fatalf("change in maintenance %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/tasks/t1/cancel", "/api/auth/logout"} {
		if w := do(t, h, dev, "POST", path, `{}`); w.Code != http.StatusNoContent {
			t.Fatalf("%s in maintenance %d", path, w.Code)
		}
	}
	if w := do(t, h, dev, "GET", "/api/tasks", ""); w.Code != http.StatusNoContent {
		t.Fatalf("read in maintenance %d", w.Code)
	}
	if w := do(t, h, owner, "PUT", "/api/maintenance", `{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("disable %d", w.Code)
	}
	if w := do(t, h, dev, "POST", "/api/tasks", `{}`); w.Code != http.StatusNoContent {
		t.Fatalf("after maintenance %d", w.Code)
	}
}
