package youtrack

import (
	"context"
	"net/url"
	"regexp"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

var _ app.IssueTextSource = (*Client)(nil)

// IssueDescription implements app.IssueTextSource: the issue's description (Markdown), read for the planner's
// context package (KIN-34) from outbox deliveries only.
func (c *Client) IssueDescription(ctx context.Context, baseURL string, token domain.Secret, issueID string) (string, error) {
	id, err := domain.NewIssueID(issueID)
	if err != nil {
		return "", err
	}
	var j struct {
		Description string `json:"description"`
	}
	if err := c.get(ctx, baseURL+"/api/issues/"+url.PathEscape(id)+"?fields=description", token, &j); err != nil {
		return "", err
	}
	return j.Description, nil
}

var articleIDRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,19}-A-[1-9][0-9]{0,9}$`)

// Article implements app.IssueTextSource: a Knowledge Base article's title and content.
func (c *Client) Article(ctx context.Context, baseURL string, token domain.Secret, articleID string) (domain.Article, error) {
	if !articleIDRe.MatchString(articleID) {
		return domain.Article{}, domain.ErrIssueNotFound
	}
	var j struct {
		IDReadable string `json:"idReadable"`
		Summary    string `json:"summary"`
		Content    string `json:"content"`
	}
	if err := c.get(ctx, baseURL+"/api/articles/"+url.PathEscape(articleID)+"?fields=idReadable,summary,content", token, &j); err != nil {
		return domain.Article{}, err
	}
	return domain.Article{ID: articleID, Title: j.Summary, Content: j.Content}, nil
}
