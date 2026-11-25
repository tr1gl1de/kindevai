package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/redact"
)

// Audit event kinds of analysis tasks (KIN-61).
const (
	KindAnalysisRequested = "alert.analysis_task_requested"
	KindAnalysisCreated   = "alert.analysis_task_created"
)

// IssueRequest asks the integrations context to create a YouTrack issue through the outbox. Key makes it
// idempotent; Marker is a token in the description that identifies the issue if a retry must look for it.
type IssueRequest struct {
	Key, Project, Summary, Description, Marker string
}

// IssueTracker is the YouTrack integration as the Alert Center needs it.
type IssueTracker interface {
	Configured(ctx context.Context) (bool, error)                  // a YouTrack connection with a token exists
	Binding(ctx context.Context, projectID string) (string, error) // the bound YouTrack short name, "" if unbound
	// RequestIssue enqueues the creation in the caller's transaction; nothing is sent from here.
	RequestIssue(ctx context.Context, r IssueRequest) error
}

// AnalysisTarget is a project whose YouTrack project may receive the issue.
type AnalysisTarget struct {
	ProjectID, ProjectName, YouTrackProject string
}

// AnalysisPreview is what the user confirms: the draft, where it can go, and an earlier request if there is one.
type AnalysisPreview struct {
	Draft      domain.AnalysisDraft
	Targets    []AnalysisTarget
	Configured bool
	Task       *domain.AnalysisTask
}

// managed loads an alert p manages: one p may not see is domain.ErrNotFound, one p sees but may not manage is
// domain.ErrForbidden.
func (s *Service) managed(ctx context.Context, p auth.Principal, id string) (access, domain.Alert, error) {
	acc, err := s.access(ctx, p)
	if err != nil {
		return access{}, domain.Alert{}, err
	}
	a, err := s.Repo.Get(ctx, id)
	if err != nil {
		return access{}, domain.Alert{}, err
	}
	if !acc.sees(a) {
		return access{}, domain.Alert{}, domain.ErrNotFound
	}
	if !acc.manages(a) {
		return access{}, domain.Alert{}, domain.ErrForbidden
	}
	return acc, a, nil
}

// targets lists the projects that may receive an analysis task of a: the alert's own project for a project alert,
// any visible project for a platform alert (Owner only); only projects bound to YouTrack.
func (s *Service) targets(ctx context.Context, acc access, a domain.Alert) ([]AnalysisTarget, error) {
	if s.Tracker == nil {
		return nil, nil
	}
	ids := []string{a.ProjectID}
	if a.ProjectID == "" {
		ids = ids[:0]
		for id := range acc.roles {
			ids = append(ids, id)
		}
	}
	out := []AnalysisTarget{}
	for _, id := range ids {
		short, err := s.Tracker.Binding(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("loading youtrack binding: %w", err)
		}
		if short != "" {
			out = append(out, AnalysisTarget{ProjectID: id, ProjectName: acc.roles[id].Name, YouTrackProject: short})
		}
	}
	sortTargets(out)
	return out, nil
}

func (s *Service) configured(ctx context.Context) (bool, error) {
	if s.Tracker == nil {
		return false, nil
	}
	ok, err := s.Tracker.Configured(ctx)
	if err != nil {
		return false, fmt.Errorf("checking youtrack: %w", err)
	}
	return ok, nil
}

// draft is the issue text of a, with anything credential-shaped masked: alert details come from monitors and
// webhooks, so they are not trusted to be free of secrets.
func draft(l domain.Language, a domain.Alert, projectName string) domain.AnalysisDraft {
	d := domain.DraftAnalysis(l, a, projectName)
	return domain.AnalysisDraft{Summary: redact.Text(d.Summary), Description: redact.Text(d.Description)}
}

func (s *Service) analysis(ctx context.Context, alertID string) (*domain.AnalysisTask, error) {
	t, err := s.Repo.Analysis(ctx, alertID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading analysis task: %w", err)
	}
	return &t, nil
}

// PreviewAnalysis drafts the analysis task of an alert for the Owner or the alert's Project Admin. It changes
// nothing.
func (s *Service) PreviewAnalysis(ctx context.Context, p auth.Principal, id string, l domain.Language) (AnalysisPreview, error) {
	acc, a, err := s.managed(ctx, p, id)
	if err != nil {
		return AnalysisPreview{}, err
	}
	out := AnalysisPreview{Draft: draft(l, a, acc.roles[a.ProjectID].Name)}
	if out.Configured, err = s.configured(ctx); err != nil {
		return AnalysisPreview{}, err
	}
	if out.Targets, err = s.targets(ctx, acc, a); err != nil {
		return AnalysisPreview{}, err
	}
	if out.Task, err = s.analysis(ctx, a.ID); err != nil {
		return AnalysisPreview{}, err
	}
	return out, nil
}

// SuggestAnalysis is the user's confirmation: it records the request and enqueues the YouTrack issue in one
// transaction. projectID picks the target of a platform alert (ignored for a project alert). An alert that already
// has an analysis task is returned unchanged: one issue per alert.
func (s *Service) SuggestAnalysis(ctx context.Context, p auth.Principal, id, projectID string, l domain.Language) (View, error) {
	var out View
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		acc, a, err := s.managed(ctx, p, id)
		if err != nil {
			return err
		}
		out = View{Alert: a, ProjectName: acc.roles[a.ProjectID].Name, CanManage: true, CanSuggest: true}
		if out.Analysis, err = s.analysis(ctx, a.ID); err != nil || out.Analysis != nil {
			return err
		}
		if ok, err := s.configured(ctx); err != nil {
			return err
		} else if !ok {
			return &domain.ValidationError{Field: "youtrack", Code: "youtrack_not_configured"}
		}
		if a.ProjectID != "" {
			projectID = a.ProjectID
		}
		if projectID == "" {
			return &domain.ValidationError{Field: "projectId", Code: "required"}
		}
		targets, err := s.targets(ctx, acc, a)
		if err != nil {
			return err
		}
		var target *AnalysisTarget
		for i := range targets {
			if targets[i].ProjectID == projectID {
				target = &targets[i]
			}
		}
		if target == nil {
			return &domain.ValidationError{Field: "projectId", Code: "youtrack_unbound"}
		}
		task, err := domain.NewAnalysisTask(a.ID, target.ProjectID, target.YouTrackProject, p.UserID, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.CreateAnalysis(ctx, task); err != nil {
			return fmt.Errorf("saving analysis task: %w", err)
		}
		d := draft(l, a, acc.roles[a.ProjectID].Name)
		if err := s.Tracker.RequestIssue(ctx, IssueRequest{Key: "alert/" + a.ID + "/analysis", Project: task.YouTrackProject,
			Summary: d.Summary, Description: d.Description, Marker: domain.AnalysisMarker(a.ID)}); err != nil {
			return fmt.Errorf("requesting youtrack issue: %w", err)
		}
		out.Analysis = &task
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindAnalysisRequested,
			ProjectID: task.ProjectID, Subject: "alert:" + a.ID, Details: map[string]string{"kind": string(a.Kind),
				"youtrackProject": task.YouTrackProject}})
	})
	return out, err
}

// AnalysisCreated stores the issue the outbox created for an alert's analysis task. A redelivery of the same issue
// and an alert deleted meanwhile (retention) are no-ops.
func (s *Service) AnalysisCreated(ctx context.Context, alertID, issueID, issueURL string) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		t, err := s.Repo.Analysis(ctx, alertID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loading analysis task: %w", err)
		}
		if t.Created() && t.IssueID == issueID {
			return nil
		}
		if err := t.MarkCreated(issueID, issueURL, s.Now()); err != nil {
			return err
		}
		if err := s.Repo.SaveAnalysis(ctx, t); err != nil {
			return fmt.Errorf("saving analysis task: %w", err)
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.System(), Kind: KindAnalysisCreated,
			ProjectID: t.ProjectID, Subject: "alert:" + alertID, Details: map[string]string{"issue": issueID}})
	})
}

func sortTargets(ts []AnalysisTarget) {
	slices.SortFunc(ts, func(a, b AnalysisTarget) int {
		return strings.Compare(a.ProjectName+"\x00"+a.ProjectID, b.ProjectName+"\x00"+b.ProjectID)
	})
}
