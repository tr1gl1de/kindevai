package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

const plain = "perm:YWRtaW4=.NDQtMQ==.s3cr3tT0kenValue"

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()
	s, err := domain.NewSecret("  " + plain + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.Reveal() != plain {
		t.Fatalf("Reveal = %q, want the trimmed value", s.Reveal())
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "token", s, "struct", struct{ S domain.Secret }{s})
	js, _ := json.Marshal(map[string]any{"s": s})
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", s), "%s": fmt.Sprintf("%s", s), "%q": fmt.Sprintf("%q", s),
		"%+v": fmt.Sprintf("%+v", struct{ S domain.Secret }{s}), "%#v": fmt.Sprintf("%#v", s),
		"%x": fmt.Sprintf("%x", s), "slog": logs.String(), "json": string(js),
		"error": fmt.Errorf("calling: %v", s).Error(),
	} {
		if strings.Contains(out, "s3cr3t") || strings.Contains(out, "YWRtaW4") || strings.Contains(out, "73336372") {
			t.Errorf("%s leaks the secret: %s", name, out)
		}
	}
}

func TestNewSecretValidation(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]string{
		"empty": "", "blank": " \n\t", "nul": "ab\x00cd", "too long": strings.Repeat("a", domain.MaxSecretBytes+1),
	} {
		if _, err := domain.NewSecret(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	pem := "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"
	s, err := domain.NewSecret(pem)
	if err != nil || s.Reveal() != strings.TrimSpace(pem) {
		t.Fatalf("multi-line secret: %v", err)
	}
	if !(domain.Secret{}).IsZero() || s.IsZero() {
		t.Fatal("IsZero")
	}
}

func TestSecretName(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"youtrack.token", "github.app_private_key", "network_profile.abc-12.password"} {
		if _, err := domain.NewSecretName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "token", "YouTrack.token", "a..b", ".a", "a.", "a b.c", strings.Repeat("a", 60) + "." + strings.Repeat("b", 60)} {
		if _, err := domain.NewSecretName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBaseURL(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://acme.youtrack.cloud":            "https://acme.youtrack.cloud",
		" https://acme.youtrack.cloud/ ":         "https://acme.youtrack.cloud",
		"https://yt.example.com/youtrack/":       "https://yt.example.com/youtrack",
		"http://localhost:8080":                  "http://localhost:8080",
		"HTTPS://Acme.YouTrack.Cloud":            "https://acme.youtrack.cloud",
		"https://acme.youtrack.cloud/youtrack//": "https://acme.youtrack.cloud/youtrack",
		"http://127.0.0.1:8080/yt":               "http://127.0.0.1:8080/yt",
		"http://[::1]:8080":                      "http://[::1]:8080",
	} {
		got, err := domain.NewBaseURL(in)
		if err != nil || got != want {
			t.Errorf("NewBaseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, code := range map[string]string{
		"":                                 "required",
		"acme.youtrack.cloud":              "base_url_format",
		"ftp://acme.youtrack.cloud":        "base_url_scheme",
		"http://acme.youtrack.cloud":       "base_url_https",
		"http://10.0.0.5":                  "base_url_https",
		"https://":                         "base_url_format",
		"https://u:p@acme.youtrack.cloud":  "base_url_credentials",
		"https://acme.youtrack.cloud/?a=1": "base_url_format",
		"https://acme.youtrack.cloud/#x":   "base_url_format",
		"https://acme\x00.cloud":           "base_url_format",
	} {
		_, err := domain.NewBaseURL(in)
		var ve *domain.ValidationError
		if !errors.As(err, &ve) || ve.Field != "baseUrl" || ve.Code != code {
			t.Errorf("NewBaseURL(%q) = %v, want baseUrl/%s", in, err, code)
		}
	}
}

func TestYouTrackToken(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewYouTrackToken("perm:abc.def.ghi"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "two words", "line\nbreak", "tab\there", "non-ascii-é"} {
		var ve *domain.ValidationError
		if _, err := domain.NewYouTrackToken(bad); !errors.As(err, &ve) || ve.Field != "token" {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestShortName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"KIN": "KIN", " kin ": "KIN", "Web_2": "WEB_2"} {
		if got, err := domain.NewShortName(in); err != nil || got != want {
			t.Errorf("NewShortName(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "1AB", "A-B", "A B", strings.Repeat("A", 51)} {
		if _, err := domain.NewShortName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNewRepository(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	r, err := domain.NewRepository("r1", "p1", " tr1gl1de ", "kindevai", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "tr1gl1de" || r.Name != "kindevai" || r.IntegrationBranch != domain.DefaultIntegrationBranch ||
		r.FullName() != "tr1gl1de/kindevai" || !r.CreatedAt.Equal(now) {
		t.Fatalf("repository = %+v", r)
	}
	if got := r.ProtectedBranches(); len(got) != 2 || got[0] != "main" || got[1] != "master" {
		t.Fatalf("protected = %v", got)
	}
	cases := []struct{ owner, name, branch, field, code string }{
		{"", "x", "", "owner", "required"},
		{"-bad", "x", "", "owner", "owner_format"},
		{"a_b", "x", "", "owner", "owner_format"},
		{"ok", "", "", "name", "required"},
		{"ok", "..", "", "name", "repo_name_format"},
		{"ok", "a/b", "", "name", "repo_name_format"},
		{"ok", "x", "main", "integrationBranch", "branch_protected"},
		{"ok", "x", "Master", "integrationBranch", "branch_protected"},
		{"ok", "x", "a..b", "integrationBranch", "branch_format"},
		{"ok", "x", "-x", "integrationBranch", "branch_format"},
		{"ok", "x", "x.lock", "integrationBranch", "branch_format"},
		{"ok", "x", "a b", "integrationBranch", "branch_format"},
	}
	for _, c := range cases {
		_, err := domain.NewRepository("r", "p", c.owner, c.name, c.branch, now)
		var ve *domain.ValidationError
		if !errors.As(err, &ve) || ve.Field != c.field || ve.Code != c.code {
			t.Errorf("%+v: got %v", c, err)
		}
	}
	later := now.Add(time.Hour)
	if err := r.SetIntegrationBranch("release/1.x", later); err != nil || r.IntegrationBranch != "release/1.x" || !r.UpdatedAt.Equal(later) {
		t.Fatalf("SetIntegrationBranch: %v %+v", err, r)
	}
	if err := r.SetIntegrationBranch("master", later); err == nil {
		t.Fatal("protected branch accepted")
	}
}

func TestConnectionCheckLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	c := domain.NewYouTrackConnection("https://a.youtrack.cloud", now)
	if c.Check.Status != domain.CheckNone {
		t.Fatalf("initial status %q", c.Check.Status)
	}
	c.StartCheck("c1", now)
	if c.Check.Status != domain.CheckPending || c.Check.ID != "c1" {
		t.Fatalf("%+v", c.Check)
	}
	if c.FinishCheck("stale", domain.CheckResult{OK: true}, now) {
		t.Fatal("a stale check result was applied")
	}
	if !c.FinishCheck("c1", domain.CheckResult{Code: "unauthorized", Detail: "HTTP 401"}, now.Add(time.Second)) {
		t.Fatal("current result not applied")
	}
	if c.Check.Status != domain.CheckFailed || c.Check.Code != "unauthorized" || c.Check.Detail != "HTTP 401" ||
		c.Check.CheckedAt == nil {
		t.Fatalf("%+v", c.Check)
	}
	if c.FinishCheck("c1", domain.CheckResult{OK: true}, now) {
		t.Fatal("a finished check was applied twice")
	}
	// A check that never reports back is shown as timed out and may be restarted.
	c.StartCheck("c3", now)
	if !c.CheckRunning(now.Add(time.Minute)) || c.CheckRunning(now.Add(domain.CheckTimeout+time.Second)) {
		t.Fatal("CheckRunning")
	}
	if v := c.CheckAt(now.Add(time.Minute)); v.Status != domain.CheckPending {
		t.Fatalf("recent pending shown as %+v", v)
	}
	if v := c.CheckAt(now.Add(domain.CheckTimeout + time.Second)); v.Status != domain.CheckFailed || v.Code != "timeout" {
		t.Fatalf("stuck pending shown as %+v", v)
	}
	c.ResetCheck()
	if c.Check != (domain.Check{Status: domain.CheckNone}) {
		t.Fatalf("reset: %+v", c.Check)
	}
	c.StartCheck("c2", now)
	c.FinishCheck("c2", domain.CheckResult{OK: true, Detail: strings.Repeat("x", 1000)}, now)
	if c.Check.Status != domain.CheckOK || len(c.Check.Detail) > domain.MaxCheckDetail {
		t.Fatalf("%+v", c.Check)
	}
}
