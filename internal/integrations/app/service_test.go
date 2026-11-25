package app_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

const token = "perm:YWRtaW4=.NDQtMQ==.s3cr3tT0kenValue1234"

func noLeak(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, "s3cr3t") {
		t.Errorf("%s leaks the token: %s", where, s)
	}
}

func TestVaultEncryptsPerFieldAndBindsName(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sec, _ := domain.NewSecret(token)
	if err := e.svc.Vault.Put(t.Context(), domain.SecretYouTrackToken, sec, "owner1"); err != nil {
		t.Fatal(err)
	}
	ct := e.store.secrets[domain.SecretYouTrackToken].ciphertext
	if bytes.Contains(ct, []byte("s3cr3t")) || len(ct) == 0 {
		t.Fatal("stored value is not encrypted")
	}
	got, err := e.svc.Vault.Credential(t.Context(), domain.SecretYouTrackToken)
	if err != nil || got.Reveal() != token {
		t.Fatalf("Credential = %v", err)
	}
	// A ciphertext moved to another slot does not decrypt: the name is the associated data.
	e.store.secrets[domain.SecretTelegramBotToken] = storedSecret{ciphertext: ct}
	if _, err := e.svc.Vault.Credential(t.Context(), domain.SecretTelegramBotToken); err == nil {
		t.Fatal("ciphertext decrypted under another name")
	}
	if _, err := e.svc.Vault.Credential(t.Context(), domain.SecretGitHubWebhookSecret); !errors.Is(err, domain.ErrSecretNotSet) {
		t.Fatalf("missing secret: %v", err)
	}
	meta, err := e.svc.Vault.Meta(t.Context(), domain.SecretYouTrackToken)
	if err != nil || !meta.Set || !meta.UpdatedAt.Equal(t0) || meta.UpdatedBy != "owner1" {
		t.Fatalf("meta = %+v, %v", meta, err)
	}
}

func TestConfigureYouTrackIsOwnerOnlyWriteOnlyAndAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if _, err := e.svc.ConfigureYouTrack(ctx, bob, "https://acme.youtrack.cloud", token); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	if _, err := e.svc.YouTrack(ctx, bob); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner read: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud", ""); !errors.As(err, &ve) || ve.Field != "token" {
		t.Fatalf("first configuration without token: %v", err)
	}
	v, err := e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud/", "  "+token+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if v.Connection.BaseURL != "https://acme.youtrack.cloud" || !v.Token.Set || v.Connection.Check.Status != domain.CheckNone {
		t.Fatalf("view = %+v", v)
	}
	got, _ := e.svc.Vault.Credential(ctx, domain.SecretYouTrackToken)
	if got.Reveal() != token {
		t.Fatal("token not stored trimmed")
	}

	// Saving without a token keeps it; the view never carries it.
	e.now = t0.Add(time.Hour)
	v, err = e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud", "")
	if err != nil || !v.Token.Set || !v.Token.UpdatedAt.Equal(t0) {
		t.Fatalf("save without token: %+v %v", v, err)
	}
	if got, _ := e.svc.Vault.Credential(ctx, domain.SecretYouTrackToken); got.Reveal() != token {
		t.Fatal("token lost on save without token")
	}
	// The stored token never goes to a new host: changing the URL needs the token again.
	if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://attacker.example", ""); !errors.As(err, &ve) || ve.Field != "token" || ve.Code != "token_required_for_url" {
		t.Fatalf("url change without token: %v", err)
	}
	if v, _ := e.svc.YouTrack(ctx, owner); v.Connection.BaseURL != "https://acme.youtrack.cloud" {
		t.Fatalf("url changed: %+v", v)
	}
	b, _ := json.Marshal(v)
	noLeak(t, "view", string(b))
	noLeak(t, "audit", e.audit.dump())
	noLeak(t, "logs", e.logs.String())
	if k := e.audit.kinds(); !slices.Equal(k, []string{app.KindYouTrackConfigured, app.KindYouTrackConfigured}) {
		t.Fatalf("audit kinds %v", k)
	}
	if d := e.audit.entries[0].Details; d["tokenChanged"] != "true" || d["baseUrl"] != "https://acme.youtrack.cloud" {
		t.Fatalf("details %v", d)
	}
	if d := e.audit.entries[1].Details; d["tokenChanged"] != "false" {
		t.Fatalf("details %v", d)
	}

	if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud", "bad token"); !errors.As(err, &ve) || ve.Code != "token_format" {
		t.Fatalf("token with space: %v", err)
	}
	if err := e.svc.DisconnectYouTrack(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if v, err := e.svc.YouTrack(ctx, owner); err != nil || v != nil {
		t.Fatalf("after disconnect: %+v %v", v, err)
	}
	if _, err := e.svc.Vault.Credential(ctx, domain.SecretYouTrackToken); !errors.Is(err, domain.ErrSecretNotSet) {
		t.Fatal("token kept after disconnect")
	}
	if err := e.svc.DisconnectYouTrack(ctx, owner); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("second disconnect: %v", err)
	}
}

func TestCheckYouTrackRunsThroughOutbox(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if _, err := e.svc.CheckYouTrack(ctx, owner); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
	if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud", token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CheckYouTrack(ctx, bob); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	v, err := e.svc.CheckYouTrack(ctx, owner)
	if err != nil || v.Connection.Check.Status != domain.CheckPending {
		t.Fatalf("check: %+v %v", v, err)
	}
	if len(e.probe.calls) != 0 {
		t.Fatal("the use case called YouTrack directly")
	}
	if len(e.out.msgs) != 1 {
		t.Fatalf("outbox: %+v", e.out.msgs)
	}
	m := e.out.msgs[0]
	if m.Adapter != app.AdapterYouTrack || m.Topic != app.TopicYouTrackCheck || m.Key == "" {
		t.Fatalf("message %+v", m)
	}
	noLeak(t, "payload", string(m.Payload))

	// The delivery probes with the stored token and records a redacted result.
	e.probe.result = domain.CheckResult{Code: "unauthorized", Detail: "HTTP 401 Authorization: Bearer " + token}
	if err := e.svc.HandleYouTrackCheck(ctx, messaging.Delivery{Payload: m.Payload, Key: m.Key, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if len(e.probe.calls) != 1 || e.probe.calls[0] != "https://acme.youtrack.cloud "+token {
		t.Fatalf("probe calls %v", e.probe.calls)
	}
	yv, _ := e.svc.YouTrack(ctx, owner)
	if c := yv.Connection.Check; c.Status != domain.CheckFailed || c.Code != "unauthorized" || c.CheckedAt == nil {
		t.Fatalf("check %+v", c)
	}
	noLeak(t, "check detail", yv.Connection.Check.Detail)
	noLeak(t, "logs", e.logs.String())
	noLeak(t, "audit", e.audit.dump())

	// A redelivery of a finished check does not probe again.
	if err := e.svc.HandleYouTrackCheck(ctx, messaging.Delivery{Payload: m.Payload, Key: m.Key, Attempt: 2}); err != nil {
		t.Fatal(err)
	}
	if len(e.probe.calls) != 1 {
		t.Fatal("finished check probed again")
	}

	// A check superseded by a configuration change is dropped.
	if _, err := e.svc.CheckYouTrack(ctx, owner); err != nil {
		t.Fatal(err)
	}
	stale := e.out.msgs[1]
	if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://new.youtrack.cloud", token); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HandleYouTrackCheck(ctx, messaging.Delivery{Payload: stale.Payload, Key: stale.Key, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.svc.YouTrack(ctx, owner); v.Connection.Check.Status != domain.CheckNone || len(e.probe.calls) != 1 {
		t.Fatalf("stale check applied: %+v", v.Connection.Check)
	}

	// Success shows the account.
	e.probe.result = domain.CheckResult{OK: true, Detail: "robot"}
	_, _ = e.svc.CheckYouTrack(ctx, owner)
	last := e.out.msgs[len(e.out.msgs)-1]
	if err := e.svc.HandleYouTrackCheck(ctx, messaging.Delivery{Payload: last.Payload, Key: last.Key, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.svc.YouTrack(ctx, owner); v.Connection.Check.Status != domain.CheckOK || v.Connection.Check.Detail != "robot" {
		t.Fatalf("success: %+v", v.Connection.Check)
	}

	if err := e.svc.HandleYouTrackCheck(ctx, messaging.Delivery{Payload: []byte("{")}); err == nil {
		t.Fatal("malformed payload accepted")
	}
}

func TestYouTrackSettingsPort(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.svc.YouTrackSettings(t.Context()); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("unconfigured: %v", err)
	}
	_, _ = e.svc.ConfigureYouTrack(t.Context(), owner, "https://acme.youtrack.cloud", token)
	s, err := e.svc.YouTrackSettings(t.Context())
	if err != nil || s.BaseURL != "https://acme.youtrack.cloud" || s.Token.Reveal() != token {
		t.Fatalf("settings %v", err)
	}
	var creds app.Credentials = e.svc.Vault
	if c, err := creds.Credential(t.Context(), domain.SecretYouTrackToken); err != nil || c.Reveal() != token {
		t.Fatalf("Credentials port: %v", err)
	}
}

func TestProjectYouTrackBinding(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if _, err := e.svc.BindYouTrackProject(ctx, bob, "p1", "KIN"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	if _, err := e.svc.BindYouTrackProject(ctx, owner, "nope", "KIN"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	pi, err := e.svc.BindYouTrackProject(ctx, owner, "p1", " kin ")
	if err != nil || pi.YouTrackProject != "KIN" {
		t.Fatalf("bind: %+v %v", pi, err)
	}
	if _, err := e.svc.BindYouTrackProject(ctx, owner, "p2", "KIN"); !errors.Is(err, domain.ErrShortNameTaken) {
		t.Fatalf("taken: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := e.svc.BindYouTrackProject(ctx, owner, "p1", "1X"); !errors.As(err, &ve) {
		t.Fatalf("invalid: %v", err)
	}
	if sn, _ := e.svc.YouTrackProjectOf(ctx, "p1"); sn != "KIN" {
		t.Fatalf("port: %q", sn)
	}
	if pi, err := e.svc.BindYouTrackProject(ctx, owner, "p1", ""); err != nil || pi.YouTrackProject != "" {
		t.Fatalf("unbind: %+v %v", pi, err)
	}
	if k := e.audit.kinds(); !slices.Equal(k, []string{app.KindYouTrackProjectBound, app.KindYouTrackProjectUnbound}) {
		t.Fatalf("audit %v", k)
	}
	if e.audit.entries[0].ProjectID != "p1" || e.audit.entries[0].Details["shortName"] != "KIN" {
		t.Fatalf("entry %+v", e.audit.entries[0])
	}
}

func TestRepositoryRegistry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := app.RepositoryInput{Owner: "tr1gl1de", Name: "kindevai"}
	if _, err := e.svc.AddRepository(ctx, bob, "p1", in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	if _, err := e.svc.AddRepository(ctx, owner, "nope", in); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	r, err := e.svc.AddRepository(ctx, owner, "p1", in)
	if err != nil || r.IntegrationBranch != "dev" || r.ProjectID != "p1" {
		t.Fatalf("add: %+v %v", r, err)
	}
	if _, err := e.svc.AddRepository(ctx, owner, "p2", app.RepositoryInput{Owner: "TR1GL1DE", Name: "KinDevAI"}); !errors.Is(err, domain.ErrRepositoryExists) {
		t.Fatalf("duplicate: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := e.svc.AddRepository(ctx, owner, "p1", app.RepositoryInput{Owner: "x", Name: "y", IntegrationBranch: "main"}); !errors.As(err, &ve) || ve.Code != "branch_protected" {
		t.Fatalf("protected: %v", err)
	}
	if _, err := e.svc.UpdateRepository(ctx, owner, "p2", r.ID, "next", nil); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other project's repo: %v", err)
	}
	r2, err := e.svc.UpdateRepository(ctx, owner, "p1", r.ID, "next", nil)
	if err != nil || r2.IntegrationBranch != "next" {
		t.Fatalf("update: %+v %v", r2, err)
	}
	// KIN-35: the test command is set with the branch, kept when absent and refused when it is no single line.
	cmd := "  make test  "
	if r2, err = e.svc.UpdateRepository(ctx, owner, "p1", r.ID, "next", &cmd); err != nil || r2.TestCommand != "make test" {
		t.Fatalf("test command: %+v %v", r2, err)
	}
	if r2, err = e.svc.UpdateRepository(ctx, owner, "p1", r.ID, "next", nil); err != nil || r2.TestCommand != "make test" {
		t.Fatalf("test command kept: %+v %v", r2, err)
	}
	bad := "make test\nrm -rf /"
	if _, err := e.svc.UpdateRepository(ctx, owner, "p1", r.ID, "next", &bad); !errors.As(err, &ve) || ve.Code != "test_command_format" {
		t.Fatalf("multi-line test command: %v", err)
	}
	pi, err := e.svc.ProjectIntegrations(ctx, owner, "p1")
	if err != nil || len(pi.Repositories) != 1 || pi.Repositories[0].IntegrationBranch != "next" {
		t.Fatalf("list: %+v %v", pi, err)
	}
	if _, err := e.svc.ProjectIntegrations(ctx, bob, "p1"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner list: %v", err)
	}
	if rs, _ := e.svc.RepositoriesOf(ctx, "p1"); len(rs) != 1 {
		t.Fatalf("port: %v", rs)
	}
	if err := e.svc.RemoveRepository(ctx, owner, "p1", r.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RemoveRepository(ctx, owner, "p1", r.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
	want := []string{app.KindRepositoryAdded, app.KindRepositoryUpdated, app.KindRepositoryUpdated, app.KindRepositoryUpdated,
		app.KindRepositoryRemoved}
	if k := e.audit.kinds(); !slices.Equal(k, want) {
		t.Fatalf("audit %v", k)
	}
	if d := e.audit.entries[1].Details; d["repository"] != "tr1gl1de/kindevai" || d["integrationBranch"] != "next" {
		t.Fatalf("details %v", d)
	}
}

func TestCheckYouTrackEdgeCases(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud", token); err != nil {
		t.Fatal(err)
	}
	deliver := func(m messaging.Message) {
		t.Helper()
		if err := e.svc.HandleYouTrackCheck(ctx, messaging.Delivery{Payload: m.Payload, Key: m.Key, Attempt: 1}); err != nil {
			t.Fatal(err)
		}
	}

	// A second request while a test is running does not queue another one.
	if _, err := e.svc.CheckYouTrack(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if v, err := e.svc.CheckYouTrack(ctx, owner); err != nil || v.Connection.Check.Status != domain.CheckPending || len(e.out.msgs) != 1 {
		t.Fatalf("double check: %+v %v, %d messages", v, err, len(e.out.msgs))
	}

	// A test that never reports back reads as timed out and can be restarted.
	e.now = t0.Add(domain.CheckTimeout + time.Second)
	if v, _ := e.svc.YouTrack(ctx, owner); v.Connection.Check.Status != domain.CheckFailed || v.Connection.Check.Code != "timeout" {
		t.Fatalf("stuck check: %+v", v.Connection.Check)
	}
	if _, err := e.svc.CheckYouTrack(ctx, owner); err != nil || len(e.out.msgs) != 2 {
		t.Fatalf("restart: %v, %d messages", err, len(e.out.msgs))
	}

	// Settings changed while the probe ran: its result is dropped.
	e.probe.during = func() {
		if _, err := e.svc.ConfigureYouTrack(ctx, owner, "https://acme.youtrack.cloud", token+"2"); err != nil {
			t.Error(err)
		}
	}
	e.probe.result = domain.CheckResult{OK: true, Detail: "robot"}
	deliver(e.out.msgs[1])
	if v, _ := e.svc.YouTrack(ctx, owner); v.Connection.Check.Status != domain.CheckNone {
		t.Fatalf("result of a superseded probe applied: %+v", v.Connection.Check)
	}
	e.probe.during = nil

	// The token disappeared (e.g. deleted by hand): the test fails instead of staying pending.
	if _, err := e.svc.CheckYouTrack(ctx, owner); err != nil {
		t.Fatal(err)
	}
	_ = e.store.DeleteSecret(ctx, domain.SecretYouTrackToken)
	deliver(e.out.msgs[len(e.out.msgs)-1])
	if v, _ := e.svc.YouTrack(ctx, owner); v.Connection.Check.Status != domain.CheckFailed || v.Connection.Check.Code != "not_configured" {
		t.Fatalf("missing token: %+v", v.Connection.Check)
	}
}
