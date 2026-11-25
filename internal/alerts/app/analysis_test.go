package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
)

// memRepo's analysis tasks.
type memAnalyses struct {
	mu    sync.Mutex
	tasks map[string]domain.AnalysisTask
}

func (r *memRepo) store() *memAnalyses {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.analyses == nil {
		r.analyses = &memAnalyses{tasks: map[string]domain.AnalysisTask{}}
	}
	return r.analyses
}

func (r *memRepo) Analysis(_ context.Context, alertID string) (domain.AnalysisTask, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[alertID]
	if !ok {
		return domain.AnalysisTask{}, domain.ErrNotFound
	}
	return t, nil
}

func (r *memRepo) Analyses(_ context.Context, ids []string) (map[string]domain.AnalysisTask, error) {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]domain.AnalysisTask{}
	for _, id := range ids {
		if t, ok := s.tasks[id]; ok {
			out[id] = t
		}
	}
	return out, nil
}

func (r *memRepo) CreateAnalysis(_ context.Context, t domain.AnalysisTask) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[t.AlertID]; ok {
		return errors.New("duplicate analysis task")
	}
	s.tasks[t.AlertID] = t
	return nil
}

func (r *memRepo) SaveAnalysis(_ context.Context, t domain.AnalysisTask) error {
	s := r.store()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.AlertID] = t
	return nil
}

type fakeTracker struct {
	mu         sync.Mutex
	configured bool
	bindings   map[string]string
	requests   []app.IssueRequest
}

func (f *fakeTracker) Configured(context.Context) (bool, error) { return f.configured, nil }

func (f *fakeTracker) Binding(_ context.Context, projectID string) (string, error) {
	return f.bindings[projectID], nil
}

func (f *fakeTracker) RequestIssue(_ context.Context, r app.IssueRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	return nil
}

func analysisFixture(t *testing.T) (fixture, *fakeTracker) {
	t.Helper()
	f := newFixture()
	tr := &fakeTracker{configured: true, bindings: map[string]string{"p1": "SHOP"}}
	f.svc.Tracker = tr
	ctx := t.Context()
	ci := domain.Occurrence{ProjectID: "p1", Kind: domain.KindCIFailed, Severity: domain.SeverityWarning,
		DedupKey: "ci/acme/app/branch/dev", Subject: "acme/app dev",
		Details: map[string]string{"conclusion": "failure", "log": "Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789"}}
	if err := f.svc.Raise(ctx, "e1", ci); err != nil { // A1: project alert
		t.Fatal(err)
	}
	if err := f.svc.Raise(ctx, "e2", runnerDown(domain.SeverityWarning)); err != nil { // A2: platform alert
		t.Fatal(err)
	}
	return f, tr
}

// AC: the preview is built from the alert, without secrets, and creates nothing.
func TestPreviewAnalysisDraftsWithoutSecretsAndCreatesNothing(t *testing.T) {
	t.Parallel()
	f, tr := analysisFixture(t)
	p, err := f.svc.PreviewAnalysis(t.Context(), admin, "A1", domain.LangEN)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Draft.Summary, "Analyze alert: CI failed on acme/app dev") {
		t.Fatalf("summary = %q", p.Draft.Summary)
	}
	if strings.Contains(p.Draft.Description, "ghp_") {
		t.Fatalf("secret in description:\n%s", p.Draft.Description)
	}
	if len(p.Targets) != 1 || p.Targets[0].ProjectID != "p1" || p.Targets[0].YouTrackProject != "SHOP" || !p.Configured {
		t.Fatalf("preview = %+v", p)
	}
	if p.Task != nil || len(tr.requests) != 0 || len(f.audit.kinds) != 0 {
		t.Fatalf("preview had effects: task=%v requests=%v audit=%v", p.Task, tr.requests, f.audit.kinds)
	}
	ru, err := f.svc.PreviewAnalysis(t.Context(), admin, "A1", domain.LangRU)
	if err != nil || !strings.HasPrefix(ru.Draft.Summary, "Разобрать алерт") {
		t.Fatalf("ru preview = %+v, %v", ru.Draft, err)
	}
}

// AC: confirmation enqueues one create-issue request per alert and records an audit event; a repeat is a no-op.
func TestSuggestAnalysisEnqueuesOncePerAlert(t *testing.T) {
	t.Parallel()
	f, tr := analysisFixture(t)
	ctx := t.Context()
	v, err := f.svc.SuggestAnalysis(ctx, admin, "A1", "", domain.LangEN)
	if err != nil {
		t.Fatal(err)
	}
	if v.Analysis == nil || v.Analysis.YouTrackProject != "SHOP" || v.Analysis.RequestedBy != "admin" || v.Analysis.Created() {
		t.Fatalf("view analysis = %+v", v.Analysis)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("requests = %+v", tr.requests)
	}
	r := tr.requests[0]
	if r.Key != "alert/A1/analysis" || r.Project != "SHOP" || r.Marker != domain.AnalysisMarker("A1") ||
		!strings.HasPrefix(r.Summary, "Analyze alert") || strings.Contains(r.Description, "ghp_") {
		t.Fatalf("request = %+v", r)
	}
	if _, err := f.svc.SuggestAnalysis(ctx, owner, "A1", "", domain.LangRU); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("a repeat enqueued again: %+v", tr.requests)
	}
	if got := strings.Join(f.audit.kinds, ","); got != app.KindAnalysisRequested {
		t.Fatalf("audit = %s", got)
	}
	// The created issue is stored on the alert (and a redelivery changes nothing).
	for range 2 {
		if err := f.svc.AnalysisCreated(ctx, "A1", "SHOP-7", "https://yt.example/issue/SHOP-7"); err != nil {
			t.Fatal(err)
		}
	}
	vs, err := f.svc.List(ctx, admin, true)
	if err != nil || len(vs) != 1 || vs[0].Analysis == nil || vs[0].Analysis.IssueID != "SHOP-7" || !vs[0].CanSuggest {
		t.Fatalf("list = %+v, %v", vs, err)
	}
	if got := strings.Join(f.audit.kinds, ","); got != app.KindAnalysisRequested+","+app.KindAnalysisCreated {
		t.Fatalf("audit = %s", got)
	}
	// A deleted alert (retention) is fine.
	if err := f.svc.AnalysisCreated(ctx, "gone", "SHOP-8", "https://yt.example/issue/SHOP-8"); err != nil {
		t.Fatal(err)
	}
}

// AC: only the Owner / Project Admin of the alert's project; members get 403, outsiders 404.
func TestSuggestAnalysisRBAC(t *testing.T) {
	t.Parallel()
	f, tr := analysisFixture(t)
	ctx := t.Context()
	if _, err := f.svc.SuggestAnalysis(ctx, member, "A1", "", domain.LangEN); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	if _, err := f.svc.PreviewAnalysis(ctx, member, "A1", domain.LangEN); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member preview: %v", err)
	}
	if _, err := f.svc.SuggestAnalysis(ctx, alien, "A1", "", domain.LangEN); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alien: %v", err)
	}
	if _, err := f.svc.SuggestAnalysis(ctx, admin, "A2", "p1", domain.LangEN); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("admin on a platform alert: %v", err)
	}
	if len(tr.requests) != 0 || len(f.audit.kinds) != 0 {
		t.Fatalf("effects: %v %v", tr.requests, f.audit.kinds)
	}
}

// Platform alerts: the Owner picks one of the projects bound to YouTrack.
func TestSuggestAnalysisForAPlatformAlertNeedsABoundProject(t *testing.T) {
	t.Parallel()
	f, tr := analysisFixture(t)
	ctx := t.Context()
	p, err := f.svc.PreviewAnalysis(ctx, owner, "A2", domain.LangEN)
	if err != nil || len(p.Targets) != 1 || p.Targets[0].ProjectID != "p1" {
		t.Fatalf("targets = %+v, %v", p.Targets, err)
	}
	var ve *domain.ValidationError
	if _, err := f.svc.SuggestAnalysis(ctx, owner, "A2", "", domain.LangEN); !errors.As(err, &ve) || ve.Code != "required" {
		t.Fatalf("no project: %v", err)
	}
	if _, err := f.svc.SuggestAnalysis(ctx, owner, "A2", "p2", domain.LangEN); !errors.As(err, &ve) || ve.Code != "youtrack_unbound" {
		t.Fatalf("unbound project: %v", err)
	}
	v, err := f.svc.SuggestAnalysis(ctx, owner, "A2", "p1", domain.LangEN)
	if err != nil || v.Analysis == nil || v.Analysis.ProjectID != "p1" || len(tr.requests) != 1 {
		t.Fatalf("view = %+v, %v", v.Analysis, err)
	}
}

// Nothing is requested while YouTrack is not configured or the project is unbound.
func TestSuggestAnalysisNeedsYouTrack(t *testing.T) {
	t.Parallel()
	f, tr := analysisFixture(t)
	ctx := t.Context()
	tr.bindings = map[string]string{}
	var ve *domain.ValidationError
	if _, err := f.svc.SuggestAnalysis(ctx, admin, "A1", "", domain.LangEN); !errors.As(err, &ve) || ve.Code != "youtrack_unbound" {
		t.Fatalf("unbound: %v", err)
	}
	tr.configured = false
	if _, err := f.svc.SuggestAnalysis(ctx, admin, "A1", "", domain.LangEN); !errors.As(err, &ve) || ve.Code != "youtrack_not_configured" {
		t.Fatalf("not configured: %v", err)
	}
	if p, err := f.svc.PreviewAnalysis(ctx, admin, "A1", domain.LangEN); err != nil || p.Configured {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	if len(tr.requests) != 0 {
		t.Fatalf("requests = %+v", tr.requests)
	}
}
