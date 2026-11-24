package domain_test

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func TestGuardBranchRefusesMainAndMaster(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"main", "master", "MAIN", " Master ", "refs/heads/main", "REFS/HEADS/master"} {
		if err := domain.GuardBranch(b); !errors.Is(err, domain.ErrProtectedBranch) {
			t.Errorf("GuardBranch(%q) = %v, want ErrProtectedBranch", b, err)
		}
	}
	for _, b := range []string{"dev", "feature/KIN-1-x", "mainline", "master-old", "refs/heads/dev", "release/main"} {
		if err := domain.GuardBranch(b); err != nil {
			t.Errorf("GuardBranch(%q) = %v, want nil", b, err)
		}
	}
}

func TestNewAppID(t *testing.T) {
	t.Parallel()
	if id, err := domain.NewAppID(" 123456 "); err != nil || id != 123456 {
		t.Fatalf("NewAppID = %d, %v", id, err)
	}
	for _, in := range []string{"", "0", "-1", "12a", "1e5", "99999999999999999999"} {
		if _, err := domain.NewAppID(in); err == nil {
			t.Errorf("NewAppID(%q) accepted", in)
		}
	}
}

func rsaPEM(t *testing.T, pkcs8 bool) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if pkcs8 {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func TestNewPrivateKey(t *testing.T) {
	t.Parallel()
	for name, pkcs8 := range map[string]bool{"pkcs1": false, "pkcs8": true} {
		s, err := domain.NewPrivateKey("\n" + rsaPEM(t, pkcs8))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := domain.ParsePrivateKey(s); err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
	}
	for name, in := range map[string]string{
		"empty":   "",
		"garbage": "not a key",
		"bad der": "-----BEGIN RSA PRIVATE KEY-----\nYWJj\n-----END RSA PRIVATE KEY-----",
		"two":     rsaPEM(t, false) + rsaPEM(t, false),
	} {
		_, err := domain.NewPrivateKey(in)
		var ve *domain.ValidationError
		if !errors.As(err, &ve) || ve.Field != "privateKey" {
			t.Errorf("%s: err = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("%s: error leaks key material: %v", name, err)
		}
	}
}

func TestNewWebhookSecret(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewWebhookSecret("0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	for in, code := range map[string]string{"": "required", "short-secret": "webhook_secret_short",
		"0123456789 abcdef": "webhook_secret_format", "0123456789abcdefé": "webhook_secret_format"} {
		_, err := domain.NewWebhookSecret(in)
		var ve *domain.ValidationError
		if !errors.As(err, &ve) || ve.Code != code {
			t.Errorf("%q: err = %v, want %s", in, err, code)
		}
	}
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	t.Parallel()
	sec, _ := domain.NewWebhookSecret("0123456789abcdef")
	body := []byte(`{"zen":"Keep it logically awesome."}`)
	if !domain.VerifySignature(sec, body, sign("0123456789abcdef", body)) {
		t.Fatal("valid signature rejected")
	}
	for name, h := range map[string]string{
		"empty": "", "other secret": sign("fedcba9876543210", body), "sha1": "sha1=" + strings.Repeat("0", 40),
		"tampered body": sign("0123456789abcdef", append(body, ' ')), "not hex": "sha256=zz",
		"truncated": sign("0123456789abcdef", body)[:40],
	} {
		if domain.VerifySignature(sec, body, h) {
			t.Errorf("%s accepted", name)
		}
	}
	if domain.VerifySignature(domain.Secret{}, body, sign("", body)) {
		t.Error("unset secret accepted")
	}
}

func TestCheckLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	a := domain.NewGitHubApp(42, now)
	a.Check.Start("c1", now)
	if !a.Check.Running(now.Add(time.Minute)) || a.Check.At(now.Add(3*time.Minute)).Code != "timeout" {
		t.Fatalf("pending check: %+v", a.Check)
	}
	if a.Check.Finish("other", domain.CheckResult{OK: true}, now) {
		t.Fatal("superseded result applied")
	}
	if !a.Check.Finish("c1", domain.CheckResult{OK: true, Detail: "my-app"}, now) || a.Check.Status != domain.CheckOK {
		t.Fatalf("finish: %+v", a.Check)
	}
}

func TestRepoStateProtection(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		branches []domain.BranchProtection
		want     domain.ProtectionStatus
		missing  []string
	}{
		"all protected": {[]domain.BranchProtection{{"main", true, true}, {"master", false, false}}, domain.ProtectionProtected, nil},
		"main open":     {[]domain.BranchProtection{{"main", true, false}, {"master", true, true}}, domain.ProtectionUnprotected, []string{"main"}},
		"none exist":    {[]domain.BranchProtection{{"main", false, false}, {"master", false, false}}, domain.ProtectionProtected, nil},
	} {
		st, missing := domain.RepoState{Branches: tc.branches}.Protection()
		if st != tc.want || strings.Join(missing, ",") != strings.Join(tc.missing, ",") {
			t.Errorf("%s: %s %v", name, st, missing)
		}
	}
}

func TestRepositorySync(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	r, err := domain.NewRepository("r1", "p1", "acme", "api", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.GitHub.Protection != domain.ProtectionUnknown || r.GitHub.Sync.Status != domain.CheckNone {
		t.Fatalf("new repository: %+v", r.GitHub)
	}
	r.StartSync("s1", now)
	ok := r.FinishSync("s1", domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{
		InstallationID: 7, DefaultBranch: "main", Branches: []domain.BranchProtection{{"main", true, false}}}}, now)
	if !ok || r.GitHub.InstallationID != 7 || r.GitHub.DefaultBranch != "main" || r.GitHub.Protection != domain.ProtectionUnprotected {
		t.Fatalf("after sync: %+v", r.GitHub)
	}
	r.StartSync("s2", now)
	r.FinishSync("s2", domain.SyncResult{CheckResult: domain.CheckResult{Code: "unreachable"}}, now)
	if r.GitHub.InstallationID != 7 || r.GitHub.Sync.Code != "unreachable" {
		t.Fatalf("failed read must keep the last state: %+v", r.GitHub)
	}
	r.StartSync("s3", now)
	r.FinishSync("s3", domain.SyncResult{CheckResult: domain.CheckResult{Code: "not_installed"}}, now)
	if r.GitHub.InstallationID != 0 || r.GitHub.Protection != domain.ProtectionUnknown {
		t.Fatalf("not installed: %+v", r.GitHub)
	}
}

func TestParseGitHubEvent(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		event, body string
		want        domain.GitHubEvent
	}{
		"push": {"push", `{"ref":"refs/heads/feature/x","after":"abc123","repository":{"full_name":"acme/api"},"installation":{"id":9}}`,
			domain.GitHubEvent{Event: "push", Repository: "acme/api", InstallationID: 9, Branch: "feature/x", HeadSHA: "abc123"}},
		"pr": {"pull_request", `{"action":"closed","repository":{"full_name":"acme/api"},"pull_request":{"number":5,"merged":true,"head":{"ref":"feature/x","sha":"h1"},"base":{"ref":"dev"}}}`,
			domain.GitHubEvent{Event: "pull_request", Action: "closed", Repository: "acme/api", PullRequest: 5, Merged: true, Branch: "feature/x", Base: "dev", HeadSHA: "h1"}},
		"suite": {"check_suite", `{"action":"completed","repository":{"full_name":"acme/api"},"check_suite":{"head_branch":"dev","head_sha":"s","status":"completed","conclusion":"failure"}}`,
			domain.GitHubEvent{Event: "check_suite", Action: "completed", Repository: "acme/api", Branch: "dev", HeadSHA: "s", Status: "completed", Conclusion: "failure"}},
		"run": {"check_run", `{"action":"created","repository":{"full_name":"acme/api"},"check_run":{"name":"ci","head_sha":"s","status":"queued"}}`,
			domain.GitHubEvent{Event: "check_run", Action: "created", Repository: "acme/api", Name: "ci", HeadSHA: "s", Status: "queued"}},
		"ping": {"ping", `{"zen":"z","hook_id":1}`, domain.GitHubEvent{Event: "ping"}},
	} {
		got, err := domain.ParseGitHubEvent(tc.event, []byte(tc.body))
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
	for name, tc := range map[string][2]string{
		"unsupported": {"issues", `{}`}, "no repo": {"push", `{"ref":"refs/heads/x"}`}, "bad json": {"push", `{`},
	} {
		if _, err := domain.ParseGitHubEvent(tc[0], []byte(tc[1])); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
