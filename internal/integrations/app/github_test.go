package app_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

var testKeyPEM = sync.OnceValue(func() string {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
})

const hookSecret = "whsec-0123456789abcdef"

// githubAPI is a fake app.GitHubAPI.
type githubAPI struct {
	mu     sync.Mutex
	check  domain.CheckResult
	read   domain.SyncResult
	calls  []string
	tokens int
	runs   []domain.CheckRun       // KIN-37: check runs of any commit
	pr     domain.PullRequestState // KIN-37: the pull request read before a merge
}

func (g *githubAPI) CheckApp(_ context.Context, c app.AppCredentials) domain.CheckResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "check")
	if c.PrivateKey.IsZero() {
		return domain.CheckResult{Code: "no key"}
	}
	return g.check
}

func (g *githubAPI) ReadRepository(_ context.Context, _ app.AppCredentials, owner, name string) domain.SyncResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "read "+owner+"/"+name)
	return g.read
}

func (g *githubAPI) InstallationToken(_ context.Context, c app.AppCredentials, inst int64, repo string) (domain.Secret, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tokens++
	g.calls = append(g.calls, "token "+repo)
	return domain.NewSecret("ghs_token-for-" + repo)
}

// inbox is a fake app.Inbox deduplicating by delivery id.
type inbox struct {
	mu     sync.Mutex
	events []messaging.InboundEvent
}

func (in *inbox) Receive(_ context.Context, e messaging.InboundEvent) (bool, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, o := range in.events {
		if o.Source == e.Source && o.DeliveryID == e.DeliveryID {
			return false, nil
		}
	}
	in.events = append(in.events, e)
	return true, nil
}

type ghEnv struct {
	*env
	gh   *app.GitHubService
	api  *githubAPI
	in   *inbox
	seen []string
}

func newGitHubEnv(t *testing.T) *ghEnv {
	t.Helper()
	e := &ghEnv{env: newEnv(t), api: &githubAPI{}, in: &inbox{}}
	e.gh = &app.GitHubService{
		Vault: e.svc.Vault, Store: e.store, Projects: projects{}, Tx: tx{}, Audit: e.audit, Outbox: e.out,
		Inbox: e.in, API: e.api, Now: e.svc.Now, NewID: e.svc.NewID, Logger: e.svc.Logger,
		Handlers: []app.GitHubEventHandler{func(_ context.Context, r domain.Repository, ev domain.GitHubEvent) error {
			e.seen = append(e.seen, r.ProjectID+" "+ev.Event+" "+ev.Repository)
			return nil
		}},
	}
	e.svc.RepositoryAdded = e.gh.RepositoryAdded
	return e
}

func (e *ghEnv) configure(t *testing.T) {
	t.Helper()
	if _, err := e.gh.ConfigureGitHub(t.Context(), owner, app.GitHubInput{AppID: "42", PrivateKey: testKeyPEM(), WebhookSecret: hookSecret}); err != nil {
		t.Fatal(err)
	}
}

func (e *ghEnv) addRepo(t *testing.T, project, ownerName, name string) domain.Repository {
	t.Helper()
	r, err := e.svc.AddRepository(t.Context(), owner, project, app.RepositoryInput{Owner: ownerName, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *ghEnv) deliver(t *testing.T, topic string) {
	t.Helper()
	e.out.mu.Lock()
	msgs := slices.Clone(e.out.msgs)
	e.out.mu.Unlock()
	for _, m := range msgs {
		if m.Topic != topic {
			continue
		}
		var err error
		switch topic {
		case app.TopicGitHubCheck:
			err = e.gh.HandleGitHubCheck(t.Context(), messaging.Delivery{Topic: topic, Key: m.Key, Payload: m.Payload})
		case app.TopicRepositorySync:
			err = e.gh.HandleRepositorySync(t.Context(), messaging.Delivery{Topic: topic, Key: m.Key, Payload: m.Payload})
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigureGitHubStoresCredentialsEncryptedAndWriteOnly(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	ctx := t.Context()
	if _, err := e.gh.ConfigureGitHub(ctx, bob, app.GitHubInput{AppID: "42"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	for name, in := range map[string]app.GitHubInput{
		"no key":    {AppID: "42", WebhookSecret: hookSecret},
		"no secret": {AppID: "42", PrivateKey: testKeyPEM()},
		"bad key":   {AppID: "42", PrivateKey: "nope", WebhookSecret: hookSecret},
		"bad id":    {AppID: "x", PrivateKey: testKeyPEM(), WebhookSecret: hookSecret},
	} {
		var ve *domain.ValidationError
		if _, err := e.gh.ConfigureGitHub(ctx, owner, in); !errors.As(err, &ve) {
			t.Errorf("%s: %v", name, err)
		}
	}
	v, err := e.gh.ConfigureGitHub(ctx, owner, app.GitHubInput{AppID: "42", PrivateKey: testKeyPEM(), WebhookSecret: hookSecret})
	if err != nil {
		t.Fatal(err)
	}
	if v.App.AppID != 42 || !v.PrivateKey.Set || !v.WebhookSecret.Set {
		t.Fatalf("view: %+v", v)
	}
	for name, st := range e.store.secrets {
		if strings.Contains(string(st.ciphertext), "PRIVATE KEY") || strings.Contains(string(st.ciphertext), hookSecret) {
			t.Fatalf("%s stored in plaintext", name)
		}
	}
	if got, err := e.svc.Vault.Credential(ctx, domain.SecretGitHubWebhookSecret); err != nil || got.Reveal() != hookSecret {
		t.Fatalf("webhook secret round trip: %v", err)
	}
	for what, out := range map[string]string{"audit": e.audit.dump(), "logs": e.logs.String()} {
		if strings.Contains(out, "PRIVATE KEY") || strings.Contains(out, hookSecret) {
			t.Fatalf("%s leaks a credential: %s", what, out)
		}
	}
	// Another App needs its own key; the stored secrets are kept when fields are empty.
	var ve *domain.ValidationError
	if _, err := e.gh.ConfigureGitHub(ctx, owner, app.GitHubInput{AppID: "43"}); !errors.As(err, &ve) || ve.Code != "key_required_for_app" {
		t.Fatalf("new app id without key: %v", err)
	}
	if v, err := e.gh.ConfigureGitHub(ctx, owner, app.GitHubInput{AppID: "42"}); err != nil || !v.WebhookSecret.Set {
		t.Fatalf("keep credentials: %+v %v", v, err)
	}
	if err := e.gh.DisconnectGitHub(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if v, err := e.gh.GitHub(ctx, owner); err != nil || v != nil || len(e.store.secrets) != 0 {
		t.Fatalf("after disconnect: %+v %v %d", v, err, len(e.store.secrets))
	}
	if !slices.Contains(e.audit.kinds(), app.KindGitHubDisconnected) {
		t.Fatal("disconnect not audited")
	}
}

func TestCheckGitHubRunsThroughOutbox(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	if _, err := e.gh.CheckGitHub(t.Context(), owner); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
	e.configure(t)
	e.api.check = domain.CheckResult{OK: true, Detail: "kindev-bot"}
	v, err := e.gh.CheckGitHub(t.Context(), owner)
	if err != nil || v.App.Check.Status != domain.CheckPending || len(e.api.calls) != 0 {
		t.Fatalf("check must only be enqueued: %+v %v %v", v.App.Check, err, e.api.calls)
	}
	e.deliver(t, app.TopicGitHubCheck)
	v2, _ := e.gh.GitHub(t.Context(), owner)
	if v2.App.Check.Status != domain.CheckOK || v2.App.Check.Detail != "kindev-bot" {
		t.Fatalf("after delivery: %+v", v2.App.Check)
	}
	e.deliver(t, app.TopicGitHubCheck) // redelivery of a finished check is a no-op
	if n := len(e.api.calls); n != 1 {
		t.Fatalf("API calls = %d", n)
	}
}

func TestRepositoryReadFromGitHub(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	before := e.addRepo(t, "p1", "acme", "early") // before the App: nothing to read yet
	if before.GitHub.Sync.Status != domain.CheckNone {
		t.Fatalf("repository added before the app: %+v", before.GitHub)
	}
	e.configure(t) // configuring the App reads every registered repository
	r := e.addRepo(t, "p1", "acme", "api")
	if r.GitHub.Sync.Status != domain.CheckPending {
		t.Fatalf("new repository not read: %+v", r.GitHub)
	}
	e.api.read = domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 7,
		DefaultBranch: "main", Branches: []domain.BranchProtection{{Branch: "main", Exists: true}, {Branch: "master"}}}}
	e.deliver(t, app.TopicRepositorySync)
	pi, err := e.svc.ProjectIntegrations(t.Context(), owner, "p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range pi.Repositories {
		if got.GitHub.InstallationID != 7 || got.GitHub.DefaultBranch != "main" || got.GitHub.Protection != domain.ProtectionUnprotected ||
			!slices.Equal(got.GitHub.Unprotected, []string{"main"}) {
			t.Fatalf("%s: %+v", got.FullName(), got.GitHub)
		}
	}
	if !slices.Contains(e.audit.kinds(), app.KindRepositorySynced) {
		t.Fatal("read not audited")
	}
	// The Owner reads it again; another project cannot reach the repository.
	if _, err := e.gh.SyncRepository(t.Context(), owner, "p2", r.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-project sync: %v", err)
	}
	if _, err := e.gh.SyncRepository(t.Context(), bob, "p1", r.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner sync: %v", err)
	}
	if got, err := e.gh.SyncRepository(t.Context(), owner, "p1", r.ID); err != nil || got.GitHub.Sync.Status != domain.CheckPending {
		t.Fatalf("sync: %+v %v", got.GitHub, err)
	}
}

func signBody(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestReceiveWebhook(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	ctx := t.Context()
	body := []byte(`{"ref":"refs/heads/feature/x","after":"abc","repository":{"full_name":"Acme/API"},"installation":{"id":7},"pusher":{"name":"x"}}`)
	d := app.WebhookDelivery{ID: "d-1", Event: "push", Signature: signBody(hookSecret, body), Body: body}
	if _, err := e.gh.ReceiveWebhook(ctx, d); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("without secret: %v", err)
	}
	e.configure(t)
	for name, bad := range map[string]app.WebhookDelivery{
		"no signature":  {ID: "d-1", Event: "push", Body: body},
		"wrong secret":  {ID: "d-1", Event: "push", Signature: signBody("another-secret-123", body), Body: body},
		"tampered body": {ID: "d-1", Event: "push", Signature: d.Signature, Body: append(slices.Clone(body), ' ')},
	} {
		if _, err := e.gh.ReceiveWebhook(ctx, bad); !errors.Is(err, domain.ErrBadSignature) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if out, err := e.gh.ReceiveWebhook(ctx, d); err != nil || out != app.WebhookIgnored {
		t.Fatalf("unregistered repository: %s %v", out, err)
	}
	r := e.addRepo(t, "p1", "acme", "api")
	if out, err := e.gh.ReceiveWebhook(ctx, d); err != nil || out != app.WebhookAccepted {
		t.Fatalf("registered: %s %v", out, err)
	}
	if out, err := e.gh.ReceiveWebhook(ctx, d); err != nil || out != app.WebhookDuplicate {
		t.Fatalf("redelivery: %s %v", out, err)
	}
	// Once read from GitHub, the repository belongs to its installation: a delivery from another one is ignored.
	e.api.read = domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 8}}
	e.deliver(t, app.TopicRepositorySync)
	if out, err := e.gh.ReceiveWebhook(ctx, app.WebhookDelivery{ID: "d-9", Event: "push", Signature: d.Signature, Body: body}); err != nil || out != app.WebhookIgnored {
		t.Fatalf("other installation: %s %v", out, err)
	}
	issues := []byte(`{"action":"opened","repository":{"full_name":"acme/api"}}`)
	if out, err := e.gh.ReceiveWebhook(ctx, app.WebhookDelivery{ID: "d-2", Event: "issues", Signature: signBody(hookSecret, issues), Body: issues}); err != nil || out != app.WebhookIgnored {
		t.Fatalf("unsupported event: %s %v", out, err)
	}
	var ve *domain.ValidationError
	if _, err := e.gh.ReceiveWebhook(ctx, app.WebhookDelivery{Event: "push", Signature: d.Signature, Body: body}); !errors.As(err, &ve) {
		t.Fatalf("missing delivery id: %v", err)
	}
	if len(e.in.events) != 1 {
		t.Fatalf("inbox events = %d", len(e.in.events))
	}
	ev := e.in.events[0]
	if ev.Source != app.SourceGitHub || ev.DeliveryID != "d-1" || ev.Topic != app.TopicGitHubEvent || strings.Contains(string(ev.Payload), "pusher") {
		t.Fatalf("stored event: %+v %s", ev, ev.Payload)
	}

	// Processing hands the event to the handlers and records the delivery.
	if err := e.gh.HandleGitHubEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.seen, []string{"p1 push Acme/API"}) {
		t.Fatalf("handlers saw %v", e.seen)
	}
	v, _ := e.gh.GitHub(ctx, owner)
	if v.App.LastEvent != "push" || v.App.LastDeliveryAt == nil {
		t.Fatalf("delivery not recorded: %+v", v.App)
	}
	if err := e.svc.RemoveRepository(ctx, owner, "p1", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.gh.HandleGitHubEvent(ctx, ev); err != nil || len(e.seen) != 1 {
		t.Fatalf("event of a removed repository: %v %v", err, e.seen)
	}
	if err := e.gh.HandleGitHubEvent(ctx, messaging.InboundEvent{Payload: []byte(`{`)}); err == nil {
		t.Fatal("malformed event accepted")
	}
}

func TestPingNeedsNoRepository(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	e.configure(t)
	body := []byte(`{"zen":"Design for failure.","hook_id":1}`)
	out, err := e.gh.ReceiveWebhook(t.Context(), app.WebhookDelivery{ID: "p-1", Event: "ping", Signature: signBody(hookSecret, body), Body: body})
	if err != nil || out != app.WebhookAccepted {
		t.Fatalf("ping: %s %v", out, err)
	}
	if err := e.gh.HandleGitHubEvent(t.Context(), e.in.events[0]); err != nil || len(e.seen) != 0 {
		t.Fatalf("ping processing: %v %v", err, e.seen)
	}
}

func TestRepositoryTokenIsProjectScoped(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	e.configure(t)
	e.addRepo(t, "p1", "acme", "api")
	ctx := t.Context()
	if _, err := e.gh.RepositoryToken(ctx, "p1", "acme/api"); !errors.Is(err, domain.ErrNotInstalled) {
		t.Fatalf("before the read: %v", err)
	}
	e.api.read = domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 7, DefaultBranch: "dev"}}
	e.deliver(t, app.TopicRepositorySync)
	// Cross-project: p2 never gets a token for p1's repository; unregistered repositories are unknown.
	for _, tc := range [][2]string{{"p2", "acme/api"}, {"p1", "acme/unknown"}, {"p1", "other/api"}} {
		if _, err := e.gh.RepositoryToken(ctx, tc[0], tc[1]); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%v: %v", tc, err)
		}
	}
	if e.api.tokens != 0 {
		t.Fatalf("tokens minted for refused requests: %d", e.api.tokens)
	}
	tok, err := e.gh.RepositoryToken(ctx, "p1", "ACME/api")
	if err != nil || tok.Reveal() != "ghs_token-for-api" {
		t.Fatalf("token: %v", err)
	}
	js, _ := json.Marshal(tok)
	if strings.Contains(string(js), "ghs_") {
		t.Fatal("token marshals in clear")
	}
}

// KIN-29: the runner's checkout source is the registered repository's integration branch plus a token for it
// alone; an uninstalled App still gives the branch (public repositories), other projects get nothing.
func TestCheckoutSource(t *testing.T) {
	t.Parallel()
	e := newGitHubEnv(t)
	e.configure(t)
	e.addRepo(t, "p1", "acme", "api")
	ctx := t.Context()
	src, err := e.gh.CheckoutSource(ctx, "p1", "acme/api")
	if err != nil || src.Branch != "dev" || !src.Token.IsZero() {
		t.Fatalf("not installed: %+v %v", src, err)
	}
	if _, err := e.gh.CheckoutSource(ctx, "p2", "acme/api"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other project: %v", err)
	}
	e.api.read = domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 7, DefaultBranch: "dev"}}
	e.deliver(t, app.TopicRepositorySync)
	src, err = e.gh.CheckoutSource(ctx, "p1", "acme/api")
	if err != nil || src.Branch != "dev" || src.Token.Reveal() != "ghs_token-for-api" {
		t.Fatalf("installed: %+v %v", src, err)
	}
}
