package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// KIN-38 AC4: the documentation decision of a run is stored — DOCS_NOT_APPLICABLE only with its reason.
func TestDocsDecisionsAreStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, func() time.Time { return t0 })
	repo := sqlite.New(d)
	_, e := running(t, svc)
	if _, err := repo.LatestDocs(ctx, e.TaskID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("none: %v", err)
	}
	na, err := domain.NewDocsNotApplicable(e.TaskID, e.ID, "internal refactoring")
	if err != nil {
		t.Fatal(err)
	}
	na.Rejected = []string{"KIN-A-12"}
	if err := repo.SaveDocs(ctx, na); err != nil {
		t.Fatal(err)
	}
	got, err := repo.LatestDocs(ctx, e.TaskID)
	if err != nil || got.Status != domain.DocsNotApplicable || got.Reason != "internal refactoring" ||
		len(got.Rejected) != 1 || got.ExecutionID != e.ID {
		t.Fatalf("stored %+v %v", got, err)
	}
	if err := repo.SaveDocs(ctx, domain.DocsDecision{TaskID: e.TaskID, ExecutionID: "e2", Status: domain.DocsNotApplicable}); err == nil {
		t.Fatal("DOCS_NOT_APPLICABLE without a reason was stored")
	}
	up := domain.DocsDecision{TaskID: e.TaskID, ExecutionID: "e3", Status: domain.DocsUpdated,
		Articles: []domain.ArticleChange{{Article: "KIN-A-19", Section: "S", Content: "C"}}}
	if err := repo.SaveDocs(ctx, up); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.LatestDocs(ctx, e.TaskID); got.Status != domain.DocsUpdated || len(got.Articles) != 1 ||
		got.Articles[0].Article != "KIN-A-19" {
		t.Fatalf("latest %+v", got)
	}
}
