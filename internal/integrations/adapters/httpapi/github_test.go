package httpapi

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

var keyPEM = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
})

const whSecret = "webhook-secret-0123456789"

func (e *env) configureGitHub(t *testing.T) {
	t.Helper()
	w := e.do(t, "owner", "PUT", "/api/integrations/github", GitHubRequest{AppID: "42", PrivateKey: keyPEM(), WebhookSecret: whSecret})
	if w.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", w.Code, w.Body)
	}
}

func (e *env) hook(t *testing.T, id, event, signature string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/api/webhooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", id)
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signature)
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func hubSign(body []byte) string {
	m := hmac.New(sha256.New, []byte(whSecret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestGitHubAppCredentialsAreWriteOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if w := e.do(t, "bob", "PUT", "/api/integrations/github", GitHubRequest{AppID: "42", PrivateKey: keyPEM(), WebhookSecret: whSecret}); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner: %d", w.Code)
	}
	if w := e.do(t, "owner", "PUT", "/api/integrations/github", GitHubRequest{AppID: "42", PrivateKey: "junk", WebhookSecret: whSecret}); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "privateKey") {
		t.Fatalf("bad key: %d %s", w.Code, w.Body)
	}
	e.configureGitHub(t)
	w := e.do(t, "owner", "GET", "/api/integrations", nil)
	got := decode[Integrations](t, w)
	if got.GitHub == nil || got.GitHub.AppID != 42 || !got.GitHub.PrivateKey.Set || !got.GitHub.WebhookSecret.Set || got.GitHub.Check.Status != "none" {
		t.Fatalf("view: %s", w.Body)
	}
	for _, leak := range []string{"PRIVATE KEY", whSecret} {
		if strings.Contains(w.Body.String(), leak) || strings.Contains(e.logs.String(), leak) {
			t.Fatalf("credential %q leaked", leak)
		}
	}
	if w := e.do(t, "owner", "POST", "/api/integrations/github/check", nil); w.Code != http.StatusAccepted || decode[GitHub](t, w).Check.Status != "pending" {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	if w := e.do(t, "owner", "DELETE", "/api/integrations/github", nil); w.Code != http.StatusNoContent {
		t.Fatalf("disconnect: %d", w.Code)
	}
	if w := e.do(t, "owner", "DELETE", "/api/integrations/github", nil); w.Code != http.StatusNotFound {
		t.Fatalf("disconnect twice: %d", w.Code)
	}
}

func TestRepositoryShowsGitHubStateAndSync(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.do(t, "owner", "POST", "/api/projects/p1/repositories", RepositoryRequest{Owner: "acme", Name: "api"})
	repo := decode[Repository](t, w)
	if repo.GitHub.Protection != "unknown" || repo.GitHub.Sync.Status != "none" || repo.GitHub.Installed || repo.GitHub.Unprotected == nil {
		t.Fatalf("new repository: %s", w.Body)
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/repositories/"+repo.ID+"/sync", nil); w.Code != http.StatusConflict {
		t.Fatalf("sync without app: %d %s", w.Code, w.Body)
	}
	e.configureGitHub(t) // reads registered repositories
	w = e.do(t, "owner", "GET", "/api/projects/p1/integrations", nil)
	if got := decode[ProjectIntegrations](t, w); got.Repositories[0].GitHub.Sync.Status != "pending" {
		t.Fatalf("after configure: %s", w.Body)
	}
	if w := e.do(t, "bob", "POST", "/api/projects/p1/repositories/"+repo.ID+"/sync", nil); w.Code != http.StatusForbidden {
		t.Fatalf("non-owner sync: %d", w.Code)
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/repositories/nope/sync", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown repo: %d", w.Code)
	}
	if w := e.do(t, "owner", "POST", "/api/projects/p1/repositories/"+repo.ID+"/sync", nil); w.Code != http.StatusAccepted {
		t.Fatalf("sync: %d %s", w.Code, w.Body)
	}
}

func TestGitHubWebhook(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	body := []byte(`{"action":"opened","repository":{"full_name":"acme/api"},"pull_request":{"number":1,"head":{"ref":"feature/x","sha":"s"},"base":{"ref":"dev"}}}`)
	if w := e.hook(t, "d1", "pull_request", hubSign(body), body); w.Code != http.StatusUnauthorized {
		t.Fatalf("unconfigured: %d", w.Code)
	}
	e.configureGitHub(t)
	for _, sig := range []string{"sha256=" + strings.Repeat("0", 64), "", "sha1=abc", "sha256=xyz"} {
		if w := e.hook(t, "d1", "pull_request", sig, body); w.Code != http.StatusUnauthorized {
			t.Fatalf("forged %q: %d", sig, w.Code)
		}
	}
	if w := e.hook(t, "d1", "pull_request", hubSign(body), body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
		t.Fatalf("unregistered repository: %d %s", w.Code, w.Body)
	}
	e.do(t, "owner", "POST", "/api/projects/p1/repositories", RepositoryRequest{Owner: "acme", Name: "api"})
	if w := e.hook(t, "d1", "pull_request", hubSign(body), body); w.Code != http.StatusAccepted {
		t.Fatalf("accepted: %d %s", w.Code, w.Body)
	}
	if w := e.hook(t, "d1", "pull_request", hubSign(body), body); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("redelivery: %d %s", w.Code, w.Body)
	}
	if w := e.hook(t, "", "pull_request", hubSign(body), body); w.Code != http.StatusBadRequest {
		t.Fatalf("no delivery id: %d", w.Code)
	}
	big := bytes.Repeat([]byte(" "), MaxWebhookBytes+1)
	if w := e.hook(t, "d2", "push", hubSign(big), big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large: %d", w.Code)
	}
	stats, err := e.inbox.Stats(t.Context())
	if err != nil || stats["github"].Pending != 1 {
		t.Fatalf("inbox: %+v %v", stats, err)
	}
	// Processing the stored event records the delivery on the App.
	e.inbox.Handle("github.event", e.gh.HandleGitHubEvent)
	if n, err := e.inbox.Poll(t.Context()); n != 1 || err != nil {
		t.Fatalf("poll: %d %v", n, err)
	}
	if got := decode[Integrations](t, e.do(t, "owner", "GET", "/api/integrations", nil)); got.GitHub.LastEvent != "pull_request" || got.GitHub.LastDeliveryAt == nil {
		t.Fatalf("delivery not recorded: %+v", got.GitHub)
	}
}
