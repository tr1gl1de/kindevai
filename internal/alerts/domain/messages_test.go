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

// KIN-106: a pending integrate_dev approval says the verified work waits for the merge into dev, in both languages.
func TestRenderMergeApproval(t *testing.T) {
	t.Parallel()
	n := domain.ApprovalNotice{ApprovalID: "ap1", TaskID: "t1", Project: "Shop", Risk: "L2", Reason: "plan",
		Scope: "integrate_dev"}
	if en := domain.RenderApproval(domain.LangEN, n); !strings.Contains(en, "merge into dev") || !strings.Contains(en, "Shop") {
		t.Fatalf("en = %q", en)
	}
	if ru := domain.RenderApproval(domain.LangRU, n); !strings.Contains(ru, "мерж в dev") || !strings.Contains(ru, "t1") {
		t.Fatalf("ru = %q", ru)
	}
}

// KIN-60: every new source has its own text in both languages, built only from the listed fields.
func TestRenderAlertNewSources(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind    domain.Kind
		subject string
		details map[string]string
		en, ru  string
	}{
		{domain.KindEgressCheckFailed, "r1", map[string]string{"code": "egress_ip_mismatch"}, "egress check failed", "проверка egress"},
		{domain.KindCIFailed, "acme/shop#7", map[string]string{"conclusion": "failure"}, "CI failed", "CI упал"},
		{domain.KindIntegrationCircuitOpen, "youtrack", nil, "suspended", "приостановлены"},
		{domain.KindTaskBlocked, "t1", map[string]string{"reason": "runner"}, "is blocked", "заблокирована"},
	}
	for _, c := range cases {
		a, err := domain.NewAlert("a", domain.Occurrence{Kind: c.kind, Severity: domain.SeverityWarning, DedupKey: "k",
			Subject: c.subject, Details: c.details}, t0)
		if err != nil {
			t.Fatal(err)
		}
		en, ru := domain.RenderAlert(domain.LangEN, a), domain.RenderAlert(domain.LangRU, a)
		if !strings.Contains(en, c.en) || !strings.Contains(en, c.subject) {
			t.Errorf("%s en = %q", c.kind, en)
		}
		if !strings.Contains(ru, c.ru) || !strings.Contains(ru, c.subject) {
			t.Errorf("%s ru = %q", c.kind, ru)
		}
	}
}

// KIN-62: a finished task's message names the outcome, the task and the project, in the user's language.
func TestRenderTaskFinished(t *testing.T) {
	t.Parallel()
	done := domain.TaskNotice{TaskID: "t1", Title: "Fix login", Project: "Shop", Failed: false}
	failed := domain.TaskNotice{TaskID: "t1", Title: "Fix login", Project: "Shop", Failed: true}
	en, ru := domain.RenderTask(domain.LangEN, done), domain.RenderTask(domain.LangRU, done)
	if !strings.Contains(en, "completed") || !strings.Contains(en, "Fix login") || !strings.Contains(en, "Shop") {
		t.Fatalf("en = %q", en)
	}
	if !strings.Contains(ru, "выполнена") || !strings.Contains(ru, "Fix login") {
		t.Fatalf("ru = %q", ru)
	}
	if got := domain.RenderTask(domain.LangEN, failed); !strings.Contains(got, "failed") {
		t.Fatalf("failed en = %q", got)
	}
	if got := domain.RenderTask(domain.LangRU, failed); !strings.Contains(got, "ошибкой") {
		t.Fatalf("failed ru = %q", got)
	}
	long := domain.RenderTask(domain.LangEN, domain.TaskNotice{TaskID: "t1", Title: strings.Repeat("x", 10_000) + "\n\x00"})
	if len([]rune(long)) > 400 || strings.ContainsRune(long, 0) {
		t.Fatalf("title not clipped: %d runes", len([]rune(long)))
	}
	en, ru = domain.ButtonLabel(domain.LangEN, domain.NotifyTaskFinished), domain.ButtonLabel(domain.LangRU, domain.NotifyTaskFinished)
	if en != "Open task" || ru != "Открыть задачу" {
		t.Fatalf("buttons = %q / %q", en, ru)
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

// KIN-83: a held CLI's message names the CLI and the runner and tells the Owner what to do, in EN and RU.
func TestRenderCLIHeld(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cause, parked string
		en, ru        []string
	}{
		{"auth", "false", []string{"claude is logged out on runner r1", "Log in to claude on runner r1", "3 times in a row"},
			[]string{"claude на раннере r1 не авторизован", "Войдите в claude на раннере r1", "3 раз подряд"}},
		{"billing", "true", []string{"refused by its subscription or billing", "Automatic retries have stopped"},
			[]string{"подписка или оплата", "Автоматические повторы остановлены"}},
		{"rate_limit", "false", []string{"keeps hitting its usage limit", "bind another CLI"}, []string{"упирается в лимит"}},
	}
	for _, c := range cases {
		a, err := domain.NewAlert("a", domain.Occurrence{Kind: domain.KindCLIHeld, Severity: domain.SeverityCritical,
			DedupKey: "cli/r1/claude", Subject: "claude @ r1", Details: map[string]string{"runnerId": "r1", "agent": "claude",
				"cause": c.cause, "strikes": "3", "parked": c.parked}}, t0)
		if err != nil {
			t.Fatal(err)
		}
		en, ru := domain.RenderAlert(domain.LangEN, a), domain.RenderAlert(domain.LangRU, a)
		for _, w := range c.en {
			if !strings.Contains(en, w) || !strings.HasPrefix(en, "Critical: ") {
				t.Errorf("%s en = %q, want %q", c.cause, en, w)
			}
		}
		for _, w := range c.ru {
			if !strings.Contains(ru, w) {
				t.Errorf("%s ru = %q, want %q", c.cause, ru, w)
			}
		}
	}
}
