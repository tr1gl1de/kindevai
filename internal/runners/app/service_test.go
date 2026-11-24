package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

func enrollReq(token, key string) EnrollRequest {
	return EnrollRequest{Token: token, PublicKey: key, Hostname: "build-box", OS: "linux", Arch: "amd64", Version: "0.1.0"}
}

func (f *fixture) token(t *testing.T) string {
	t.Helper()
	_, plain, err := f.svc.CreateToken(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func (f *fixture) enrolled(t *testing.T, key string) domain.Runner {
	t.Helper()
	res, err := f.svc.Enroll(t.Context(), enrollReq(f.token(t), key))
	if err != nil {
		t.Fatal(err)
	}
	return res.Runner
}

func assertNoSecret(t *testing.T, f *fixture, secret string) {
	t.Helper()
	if s := fmt.Sprintf("%+v", f.audit.entries); strings.Contains(s, secret) {
		t.Fatalf("audit log contains the token: %s", s)
	}
}

func TestCreateTokenOwnerOnlyShowsPlaintextOnceAndAudits(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, _, err := f.svc.CreateToken(t.Context(), member); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member creating a token = %v", err)
	}
	tok, plain, err := f.svc.CreateToken(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := f.repo.TokenByID(t.Context(), tok.ID)
	if plain == "" || strings.Contains(fmt.Sprintf("%+v", stored), plain) {
		t.Fatal("only the hash of the token is stored")
	}
	if !tok.ExpiresAt.Equal(f.now.Add(10 * time.Minute)) {
		t.Fatalf("expires %v", tok.ExpiresAt)
	}
	e := f.audit.last()
	if e.Kind != KindTokenCreated || e.Subject != "runner_enrollment:"+tok.ID || e.Actor.ID != "owner1" {
		t.Fatalf("audit = %+v", e)
	}
	assertNoSecret(t, f, plain)
}

func TestEnrollCreatesPendingRunnerAndConsumesToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plain := f.token(t)
	res, err := f.svc.Enroll(t.Context(), enrollReq(plain, keyA))
	if err != nil {
		t.Fatal(err)
	}
	r := res.Runner
	if !res.Created || r.Status != domain.StatusPending || r.CreatedBy != "owner1" || r.Metadata.Hostname != "build-box" {
		t.Fatalf("enroll = %+v", res)
	}
	tok, _ := f.repo.TokenByID(t.Context(), r.EnrollmentTokenID)
	if tok.State(f.now) != domain.TokenUsed || tok.RunnerID != r.ID {
		t.Fatalf("token after enroll = %+v", tok)
	}
	e := f.audit.last()
	if e.Kind != KindEnrolled || e.Actor.Type != "runner" || e.Actor.ID != r.ID || e.Details["fingerprint"] != r.Fingerprint {
		t.Fatalf("audit = %+v", e)
	}
	assertNoSecret(t, f, plain)
}

func TestEnrollRetryWithSameKeyIsIdempotent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plain := f.token(t)
	first, err := f.svc.Enroll(t.Context(), enrollReq(plain, keyA))
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Hour) // a retry after the token's TTL still finds its registration
	again, err := f.svc.Enroll(t.Context(), enrollReq(plain, keyA))
	if err != nil || again.Created || again.Runner.ID != first.Runner.ID {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if rs, _ := f.repo.Runners(t.Context()); len(rs) != 1 {
		t.Fatalf("runners = %d, want 1", len(rs))
	}
	if _, err := f.svc.Enroll(t.Context(), enrollReq(plain, keyB)); !errors.Is(err, domain.ErrTokenUsed) {
		t.Fatalf("other key with a used token = %v", err)
	}
}

func TestEnrollRejectsBadTokensWithoutCreatingRunner(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	expired := f.token(t)
	f.now = f.now.Add(11 * time.Minute)
	revokedTok, revoked, _ := f.svc.CreateToken(t.Context(), owner)
	if err := f.svc.RevokeToken(t.Context(), owner, revokedTok.ID); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		token string
		want  error
	}{
		{"", domain.ErrTokenInvalid},
		{"nope", domain.ErrTokenInvalid},
		{expired, domain.ErrTokenExpired},
		{revoked, domain.ErrTokenRevoked},
	} {
		if _, err := f.svc.Enroll(t.Context(), enrollReq(tt.token, keyA)); !errors.Is(err, tt.want) {
			t.Errorf("enroll with %q = %v, want %v", tt.token, err, tt.want)
		}
	}
	if rs, _ := f.repo.Runners(t.Context()); len(rs) != 0 {
		t.Fatalf("runners = %d, want 0", len(rs))
	}
	if e := f.audit.last(); e.Kind != KindEnrollRejected || e.Details["reason"] != "revoked" {
		t.Fatalf("rejected enrollment with a known token is audited: %+v", e)
	}
	assertNoSecret(t, f, expired)
	assertNoSecret(t, f, revoked)
}

func TestEnrollValidatesInputAndKeyUniqueness(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	plain := f.token(t)
	var ve *domain.ValidationError
	if _, err := f.svc.Enroll(t.Context(), enrollReq(plain, "short")); !errors.As(err, &ve) || ve.Field != "publicKey" {
		t.Fatalf("bad key = %v", err)
	}
	bad := enrollReq(plain, keyA)
	bad.OS = ""
	if _, err := f.svc.Enroll(t.Context(), bad); !errors.As(err, &ve) || ve.Field != "os" {
		t.Fatalf("bad metadata = %v", err)
	}
	f.enrolled(t, keyA)
	if _, err := f.svc.Enroll(t.Context(), enrollReq(plain, keyA)); !errors.Is(err, domain.ErrKeyEnrolled) {
		t.Fatalf("same key with another token = %v", err)
	}
	if e := f.audit.last(); e.Kind != KindEnrollRejected || e.Details["reason"] != "key_enrolled" {
		t.Fatalf("a replayed key is audited: %+v", e)
	}
	if tok, _ := f.repo.TokenByHash(t.Context(), domain.HashEnrollmentToken(plain)); tok.ConsumedAt != nil {
		t.Fatal("a failed enrollment leaves the token usable")
	}
}

func TestOwnerApprovesWithExplicitProjects(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.enrolled(t, keyA)
	if _, err := f.svc.Approve(t.Context(), member, r.ID, []string{"p1"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member approving = %v", err)
	}
	var ve *domain.ValidationError
	if _, err := f.svc.Approve(t.Context(), owner, r.ID, []string{"p1", "p9"}); !errors.As(err, &ve) || ve.Code != "project_not_found" {
		t.Fatalf("unknown project = %v", err)
	}
	if _, err := f.svc.Approve(t.Context(), owner, "missing", []string{"p1"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown runner = %v", err)
	}
	v, err := f.svc.Approve(t.Context(), owner, r.ID, []string{"p2", "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.Status != domain.StatusApproved || len(v.Projects) != 2 || v.Projects[0].Name != "Alpha" {
		t.Fatalf("approved = %+v", v)
	}
	if e := f.audit.last(); e.Kind != KindApproved || e.Details["projects"] != "p1,p2" || e.Subject != "runner:"+r.ID {
		t.Fatalf("audit = %+v", e)
	}
	if _, err := f.svc.Approve(t.Context(), owner, r.ID, []string{"p1"}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second approval = %v", err)
	}
}

func TestOwnerDisablesEnablesChangesProjectsAndRevokes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.enrolled(t, keyA)
	if _, err := f.svc.Approve(t.Context(), owner, r.ID, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		kind string
		do   func() (RunnerView, error)
		want domain.Status
	}{
		{KindDisabled, func() (RunnerView, error) { return f.svc.Disable(t.Context(), owner, r.ID) }, domain.StatusDisabled},
		{KindEnabled, func() (RunnerView, error) { return f.svc.Enable(t.Context(), owner, r.ID) }, domain.StatusApproved},
		{KindProjectsChanged, func() (RunnerView, error) { return f.svc.SetProjects(t.Context(), owner, r.ID, []string{"p2"}) }, domain.StatusApproved},
		{KindRevoked, func() (RunnerView, error) { return f.svc.Revoke(t.Context(), owner, r.ID) }, domain.StatusRevoked},
	}
	for _, s := range steps {
		v, err := s.do()
		if err != nil || v.Runner.Status != s.want {
			t.Fatalf("%s: %+v, %v", s.kind, v, err)
		}
		if got := f.audit.last().Kind; got != s.kind {
			t.Fatalf("audit kind = %s, want %s", got, s.kind)
		}
	}
	if _, err := f.svc.Enable(t.Context(), owner, r.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("enable revoked = %v", err)
	}
	for name, op := range map[string]func() error{
		"disable":  func() error { _, err := f.svc.Disable(t.Context(), member, r.ID); return err },
		"enable":   func() error { _, err := f.svc.Enable(t.Context(), member, r.ID); return err },
		"revoke":   func() error { _, err := f.svc.Revoke(t.Context(), member, r.ID); return err },
		"projects": func() error { _, err := f.svc.SetProjects(t.Context(), member, r.ID, []string{"p1"}); return err },
		"list":     func() error { _, err := f.svc.List(t.Context(), member); return err },
		"get":      func() error { _, err := f.svc.Get(t.Context(), member, r.ID); return err },
		"tokens":   func() error { _, err := f.svc.ListTokens(t.Context(), member); return err },
		"revokeTk": func() error { return f.svc.RevokeToken(t.Context(), member, "x") },
	} {
		if err := op(); !errors.Is(err, domain.ErrForbidden) {
			t.Errorf("member %s = %v, want ErrForbidden", name, err)
		}
	}
}

func TestRevokeTokenOnlyWhilePending(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tok, plain, _ := f.svc.CreateToken(t.Context(), owner)
	if err := f.svc.RevokeToken(t.Context(), owner, tok.ID); err != nil {
		t.Fatal(err)
	}
	if e := f.audit.last(); e.Kind != KindTokenRevoked {
		t.Fatalf("audit = %+v", e)
	}
	var ve *domain.ValidationError
	if err := f.svc.RevokeToken(t.Context(), owner, tok.ID); !errors.As(err, &ve) || ve.Code != "token_not_pending" {
		t.Fatalf("second revoke = %v", err)
	}
	if err := f.svc.RevokeToken(t.Context(), owner, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	ts, _ := f.svc.ListTokens(t.Context(), owner)
	if len(ts) != 1 || ts[0].State != domain.TokenRevoked {
		t.Fatalf("tokens = %+v", ts)
	}
	assertNoSecret(t, f, plain)
}

func TestListAndGetShowProjectNames(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := f.enrolled(t, keyA)
	f.enrolled(t, keyB)
	if _, err := f.svc.Approve(t.Context(), owner, r.ID, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	vs, err := f.svc.List(t.Context(), owner)
	if err != nil || len(vs) != 2 {
		t.Fatalf("list = %+v, %v", vs, err)
	}
	v, err := f.svc.Get(t.Context(), owner, r.ID)
	if err != nil || len(v.Projects) != 1 || v.Projects[0] != (ProjectRef{ID: "p1", Name: "Alpha"}) {
		t.Fatalf("get = %+v, %v", v, err)
	}
}
