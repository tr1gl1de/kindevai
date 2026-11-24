package domain_test

import (
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
)

// AC: notifications in the user's language, without secrets (only the fields listed in the message).
func TestRenderAlertInUserLanguage(t *testing.T) {
	t.Parallel()
	a, _ := domain.NewAlert("a1", domain.Occurrence{Kind: domain.KindStorageLevel, Severity: domain.SeverityCritical,
		DedupKey: "storage/m1/data", Subject: "m1/data",
		Details: map[string]string{"usedPercent": "96", "token": "ghp_secret"}}, t0)
	en, ru := domain.RenderAlert(domain.LangEN, a), domain.RenderAlert(domain.LangRU, a)
	if !strings.Contains(en, "Disk") || !strings.Contains(en, "96%") || !strings.Contains(en, "m1/data") {
		t.Fatalf("en = %q", en)
	}
	if !strings.Contains(ru, "Диск") || !strings.Contains(ru, "96%") {
		t.Fatalf("ru = %q", ru)
	}
	if strings.Contains(en+ru, "ghp_secret") {
		t.Fatal("unlisted detail leaked into the message")
	}
	r, _ := domain.NewAlert("a2", occ(domain.SeverityWarning), t0)
	if got := domain.RenderAlert(domain.LangRU, r); !strings.Contains(got, "недоступен") || !strings.Contains(got, "r1") {
		t.Fatalf("runner ru = %q", got)
	}
	if domain.ButtonLabel(domain.LangRU, domain.NotifyApprovalRequested) == domain.ButtonLabel(domain.LangEN, domain.NotifyApprovalRequested) {
		t.Fatal("button label not translated")
	}
}

// Review: free-form values are clipped and stripped of control characters, so a long reason cannot exceed
// Telegram's message limit and turn the notification into a permanent 400.
func TestRenderApprovalClipsValues(t *testing.T) {
	t.Parallel()
	got := domain.RenderApproval(domain.LangEN, domain.ApprovalNotice{TaskID: "t1", Reason: strings.Repeat("x", 10_000) + "\n\x00"})
	if len([]rune(got)) > 600 || strings.ContainsRune(got, 0) {
		t.Fatalf("len %d", len([]rune(got)))
	}
}

func TestRenderApproval(t *testing.T) {
	t.Parallel()
	n := domain.ApprovalNotice{ApprovalID: "ap1", TaskID: "t1", Project: "Shop", Risk: "L2", Reason: "execute"}
	en := domain.RenderApproval(domain.LangEN, n)
	if !strings.Contains(en, "Shop") || !strings.Contains(en, "L2") || !strings.Contains(en, "t1") {
		t.Fatalf("en = %q", en)
	}
	if ru := domain.RenderApproval(domain.LangRU, n); !strings.Contains(ru, "одобрение") {
		t.Fatalf("ru = %q", ru)
	}
}

// KIN-59: the runner output storage alert in both languages.
func TestRenderRunnerOutputFull(t *testing.T) {
	t.Parallel()
	a, _ := domain.NewAlert("a1", domain.Occurrence{Kind: domain.KindRunnerOutputFull, Severity: domain.SeverityWarning,
		DedupKey: "runner-output/full", Subject: "control-plane", Details: map[string]string{"limitBytes": "1073741824", "limitMiB": "1024"}}, t0)
	en, ru := domain.RenderAlert(domain.LangEN, a), domain.RenderAlert(domain.LangRU, a)
	if !strings.Contains(en, "Runner output storage is full (1024 MiB)") || !strings.Contains(ru, "Хранилище вывода раннеров заполнено (1024 МиБ)") {
		t.Fatalf("en = %q, ru = %q", en, ru)
	}
}
