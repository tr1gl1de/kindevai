package domain_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
)

func ciAlert(t *testing.T) domain.Alert {
	t.Helper()
	a, err := domain.NewAlert("A1", domain.Occurrence{ProjectID: "p1", Kind: domain.KindCIFailed, Severity: domain.SeverityWarning,
		DedupKey: "ci/acme/app/branch/dev", Subject: "acme/app dev",
		Details: map[string]string{"conclusion": "failure", "repository": "acme/app"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAnalysisDraftDescribesTheAlertInTheRequestedLanguage(t *testing.T) {
	t.Parallel()
	a := ciAlert(t)
	en := domain.DraftAnalysis(domain.LangEN, a, "Alpha")
	if en.Summary != "Analyze alert: CI failed on acme/app dev (failure)." {
		t.Fatalf("summary = %q", en.Summary)
	}
	for _, want := range []string{"Warning: CI failed on acme/app dev (failure).", "Project: Alpha", "ci_failed",
		"conclusion: failure", "repository: acme/app", "Occurrences: 1", "Alert First", domain.AnalysisMarker("A1")} {
		if !strings.Contains(en.Description, want) {
			t.Errorf("EN description lacks %q:\n%s", want, en.Description)
		}
	}
	ru := domain.DraftAnalysis(domain.LangRU, a, "Alpha")
	if ru.Summary != "Разобрать алерт: CI упал на acme/app dev (failure)." {
		t.Fatalf("RU summary = %q", ru.Summary)
	}
	for _, want := range []string{"Проект: Alpha", "Повторений: 1", domain.AnalysisMarker("A1")} {
		if !strings.Contains(ru.Description, want) {
			t.Errorf("RU description lacks %q:\n%s", want, ru.Description)
		}
	}
	if p := domain.DraftAnalysis(domain.LangEN, domain.Alert{ID: "A2", Kind: domain.KindRunnerUnreachable,
		Severity: domain.SeverityWarning, Subject: "r1"}, ""); !strings.Contains(p.Description, "Project: Platform") {
		t.Errorf("platform alert description:\n%s", p.Description)
	}
}

func TestAnalysisDraftIsBoundedAndHasNoControlCharacters(t *testing.T) {
	t.Parallel()
	a := ciAlert(t)
	a.Subject = strings.Repeat("x", 190) + "\x1b[31m"
	a.Details = map[string]string{}
	for i := range domain.MaxDetails {
		a.Details[strings.Repeat("k", 10)+string(rune('a'+i))] = strings.Repeat("v\n", 90)
	}
	d := domain.DraftAnalysis(domain.LangEN, a, "Alpha")
	if utf8.RuneCountInString(d.Summary) > domain.MaxAnalysisSummary {
		t.Fatalf("summary has %d runes", utf8.RuneCountInString(d.Summary))
	}
	if strings.ContainsAny(d.Summary, "\n\x1b") || strings.Contains(d.Description, "\x1b") {
		t.Fatalf("control characters leaked: %q", d.Summary)
	}
	if len(d.Description) > domain.MaxAnalysisDescription {
		t.Fatalf("description is %d bytes", len(d.Description))
	}
}

func TestAnalysisTaskIsRequestedOnceThenCreated(t *testing.T) {
	t.Parallel()
	task, err := domain.NewAnalysisTask("A1", "p1", "KIN", "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if task.Created() {
		t.Fatal("a new request is not created yet")
	}
	if err := task.MarkCreated("KIN-7", "https://yt.example/issue/KIN-7", t0); err != nil {
		t.Fatal(err)
	}
	if !task.Created() || task.IssueID != "KIN-7" || task.IssueURL != "https://yt.example/issue/KIN-7" {
		t.Fatalf("task = %+v", task)
	}
	// A redelivery with the same issue is fine; another issue is a conflict.
	if err := task.MarkCreated("KIN-7", "https://yt.example/issue/KIN-7", t0); err != nil {
		t.Fatalf("same issue again: %v", err)
	}
	if err := task.MarkCreated("KIN-8", "https://yt.example/issue/KIN-8", t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("other issue: %v", err)
	}
	for name, c := range map[string][3]string{
		"no project":  {"", "u1", "KIN"},
		"bad project": {"p1", "u1", "kin x"},
		"no user":     {"p1", "", "KIN"},
	} {
		if _, err := domain.NewAnalysisTask("A1", c[0], c[2], c[1], t0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := task.MarkCreated("bad id", "javascript:alert(1)", t0); err == nil {
		t.Error("a malformed issue id / URL is accepted")
	}
}

// A permanently failed request can be retried; the retry is a new attempt (new outbox key, same marker).
func TestAnalysisTaskFailsAndIsRetried(t *testing.T) {
	t.Parallel()
	task, err := domain.NewAnalysisTask("A1", "p1", "KIN", "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if task.Attempt != 1 || task.Failed() {
		t.Fatalf("new task = %+v", task)
	}
	if err := task.Retry("p1", "KIN", "u2", t0); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("retry of a pending task: %v", err)
	}
	if !task.MarkFailed(1, "rejected", t0) || !task.Failed() || task.Failure != "rejected" {
		t.Fatalf("failed = %+v", task)
	}
	if task.MarkFailed(0, "rejected", t0) {
		t.Fatal("a stale attempt's failure was applied")
	}
	if err := task.Retry("p2", "OPS", "u2", t0); err != nil {
		t.Fatal(err)
	}
	if task.Attempt != 2 || task.Failed() || task.RequestedBy != "u2" || task.YouTrackProject != "OPS" || task.ProjectID != "p2" {
		t.Fatalf("retried = %+v", task)
	}
	if err := task.MarkCreated("OPS-1", "https://yt.example/issue/OPS-1", t0); err != nil {
		t.Fatal(err)
	}
	if task.MarkFailed(2, "rejected", t0) || task.Failed() {
		t.Fatal("a created task was marked failed")
	}
}
