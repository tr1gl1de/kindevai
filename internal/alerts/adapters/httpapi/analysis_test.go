package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/alerts/adapters/httpapi"
	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
)

type tracker struct {
	mu   sync.Mutex
	reqs []app.IssueRequest
}

func (*tracker) Configured(context.Context) (bool, error) { return true, nil }
func (*tracker) Binding(_ context.Context, p string) (string, error) {
	if p == "p1" {
		return "ALPHA", nil
	}
	return "", nil
}
func (t *tracker) RequestIssue(_ context.Context, r app.IssueRequest) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reqs = append(t.reqs, r)
	return nil
}

// AC: preview (no secrets, no effect) → explicit confirmation → one request per alert, shown on the alert.
func TestAnalysisTaskAPI(t *testing.T) {
	t.Parallel()
	h, svc := setup(t)
	tr := &tracker{}
	svc.Tracker = tr
	ctx := t.Context()
	if err := svc.Raise(ctx, "e1", domain.Occurrence{ProjectID: "p1", Kind: domain.KindCIFailed, Severity: domain.SeverityCritical,
		DedupKey: "ci/p1", Subject: "dev", Details: map[string]string{"api_token": "s3cr3t-value"}}); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, h, "owner", "GET", "/api/alerts/A1/analysis-task?lang=ru", nil)
	var p httpapi.AnalysisPreviewView
	if err := json.Unmarshal(body, &p); code != 200 || err != nil {
		t.Fatalf("preview = %d %s", code, body)
	}
	if !strings.HasPrefix(p.Summary, "Разобрать алерт") || strings.Contains(p.Description, "s3cr3t") || !p.Configured ||
		len(p.Targets) != 1 || p.Targets[0].YouTrackProject != "ALPHA" || p.Task != nil {
		t.Fatalf("preview = %+v", p)
	}
	if len(tr.reqs) != 0 {
		t.Fatal("the preview requested an issue")
	}
	if code, _ := do(t, h, "member", "GET", "/api/alerts/A1/analysis-task", nil); code != http.StatusForbidden {
		t.Fatalf("member preview = %d", code)
	}
	if code, _ := do(t, h, "member", "POST", "/api/alerts/A1/analysis-task", map[string]string{}); code != http.StatusForbidden {
		t.Fatalf("member confirm = %d", code)
	}
	if code, _ := do(t, h, "owner", "GET", "/api/alerts/A1/analysis-task?lang=xx", nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad lang = %d", code)
	}
	code, body = do(t, h, "owner", "POST", "/api/alerts/A1/analysis-task", map[string]string{"language": "en"})
	var v httpapi.AlertView
	if err := json.Unmarshal(body, &v); code != 200 || err != nil || v.AnalysisTask == nil || v.AnalysisTask.YouTrackProject != "ALPHA" ||
		v.AnalysisTask.IssueID != "" {
		t.Fatalf("confirm = %d %s", code, body)
	}
	if code, _ := do(t, h, "owner", "POST", "/api/alerts/A1/analysis-task", map[string]string{"language": "en"}); code != 200 {
		t.Fatalf("repeat = %d", code)
	}
	if len(tr.reqs) != 1 || strings.Contains(tr.reqs[0].Description, "s3cr3t") {
		t.Fatalf("requests = %+v", tr.reqs)
	}
	if err := svc.AnalysisCreated(ctx, "A1", "ALPHA-3", "https://yt.example/issue/ALPHA-3"); err != nil {
		t.Fatal(err)
	}
	code, body = do(t, h, "owner", "GET", "/api/alerts", nil)
	var list []httpapi.AlertView
	if err := json.Unmarshal(body, &list); code != 200 || err != nil || len(list) != 1 || !list[0].CanSuggestAnalysis ||
		list[0].AnalysisTask == nil || list[0].AnalysisTask.IssueURL != "https://yt.example/issue/ALPHA-3" {
		t.Fatalf("list = %d %s", code, body)
	}
}
