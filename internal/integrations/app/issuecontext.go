package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// MaxContextArticles bounds the linked Knowledge Base articles read for one issue's context package.
const MaxContextArticles = 3

// ErrContextUnavailable: the issue's context cannot be read and retrying will not help (YouTrack not connected,
// the token refused, the issue gone).
var ErrContextUnavailable = errors.New("youtrack context unavailable")

// IssueTextSource reads an issue's description and Knowledge Base articles (the YouTrack client).
type IssueTextSource interface {
	IssueDescription(ctx context.Context, baseURL string, token domain.Secret, issueID string) (string, error)  // domain.ErrIssueNotFound
	Article(ctx context.Context, baseURL string, token domain.Secret, articleID string) (domain.Article, error) // domain.ErrIssueNotFound
}

// IssueContext is an issue's text and the articles it links, as YouTrack returned them (untrusted text).
type IssueContext struct {
	Description string
	Articles    []domain.Article
	Missing     []string // linked articles that could not be read
}

// IssueContextReader reads the planner's context package of a YouTrack issue (KIN-34). It calls YouTrack, so it
// only runs from outbox deliveries, never from HTTP handlers.
type IssueContextReader struct {
	Settings func(ctx context.Context) (YouTrackSettings, error) // Service.YouTrackSettings
	Source   IssueTextSource
}

// Read returns the issue's description and up to MaxContextArticles linked articles. An article that cannot be
// read is listed as missing; a final failure of the issue itself wraps ErrContextUnavailable, anything else is
// transient.
func (r IssueContextReader) Read(ctx context.Context, issueID string) (IssueContext, error) {
	id, err := domain.NewIssueID(issueID)
	if err != nil {
		return IssueContext{}, fmt.Errorf("%w: %w", ErrContextUnavailable, err)
	}
	s, err := r.Settings(ctx)
	if errors.Is(err, domain.ErrNotConfigured) || errors.Is(err, domain.ErrSecretNotSet) {
		return IssueContext{}, fmt.Errorf("%w: YouTrack is not connected", ErrContextUnavailable)
	}
	if err != nil {
		return IssueContext{}, err
	}
	desc, err := r.Source.IssueDescription(ctx, s.BaseURL, s.Token, id)
	if err := final(err); err != nil {
		return IssueContext{}, err
	}
	out := IssueContext{Description: desc}
	for _, aid := range domain.ArticleIDs(desc, MaxContextArticles) {
		a, err := r.Source.Article(ctx, s.BaseURL, s.Token, aid)
		if err != nil {
			if ctx.Err() != nil {
				return IssueContext{}, ctx.Err()
			}
			out.Missing = append(out.Missing, aid)
			continue
		}
		out.Articles = append(out.Articles, a)
	}
	return out, nil
}

// final marks the issue failures retrying cannot fix.
func final(err error) error {
	var se *StatusError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrIssueNotFound):
		return fmt.Errorf("%w: the issue is not readable", ErrContextUnavailable)
	case errors.As(err, &se) && se.Permanent():
		return fmt.Errorf("%w: YouTrack refused the request (%d)", ErrContextUnavailable, se.Status)
	}
	return err
}
