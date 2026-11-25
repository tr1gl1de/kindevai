package youtrack

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// Write-back (KIN-38): comments, commands and Knowledge Base articles, written as the connection's (integration)
// user. Like the reads, it runs only from outbox deliveries.

var _ app.SyncClient = (*Client)(nil)

// maxCommentScan is how many of an issue's comments the marker search reads (the newest ones).
const maxCommentScan = 200

func issuePath(baseURL, issueID string) (string, error) {
	id, err := domain.NewIssueID(issueID)
	if err != nil {
		return "", err
	}
	return baseURL + "/api/issues/" + url.PathEscape(id), nil
}

// IssueComments implements app.SyncClient: the texts of the issue's newest comments.
func (c *Client) IssueComments(ctx context.Context, baseURL string, token domain.Secret, issueID string) ([]string, error) {
	p, err := issuePath(baseURL, issueID)
	if err != nil {
		return nil, err
	}
	var n struct {
		Comments []struct{} `json:"comments"`
	}
	if err := c.get(ctx, p+"?fields=comments(id)", token, &n); err != nil {
		return nil, err
	}
	v := url.Values{}
	v.Set("fields", "text")
	v.Set("$skip", "0")
	if len(n.Comments) > maxCommentScan {
		v.Set("$skip", strconv.Itoa(len(n.Comments)-maxCommentScan))
	}
	v.Set("$top", strconv.Itoa(maxCommentScan))
	var out []struct {
		Text string `json:"text"`
	}
	if err := c.get(ctx, p+"/comments?"+v.Encode(), token, &out); err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(out))
	for _, x := range out {
		texts = append(texts, x.Text)
	}
	return texts, nil
}

// AddComment implements app.SyncClient.
func (c *Client) AddComment(ctx context.Context, baseURL string, token domain.Secret, issueID, text string) error {
	p, err := issuePath(baseURL, issueID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	var out struct{}
	return c.do(ctx, "POST", p+"/comments?fields=id", token, body, &out)
}

// ApplyCommand implements app.SyncClient: a YouTrack command (e.g. "State {In Progress}") on one issue.
func (c *Client) ApplyCommand(ctx context.Context, baseURL string, token domain.Secret, issueID, command string) error {
	id, err := domain.NewIssueID(issueID)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": command, "issues": []map[string]string{{"idReadable": id}}})
	if err != nil {
		return err
	}
	var out struct{}
	return c.do(ctx, "POST", baseURL+"/api/commands?fields=id", token, body, &out)
}

// ArticleRevision implements app.SyncClient: the article's content and `updated` time.
func (c *Client) ArticleRevision(ctx context.Context, baseURL string, token domain.Secret, articleID string) (app.ArticleRevision, error) {
	if !articleIDRe.MatchString(articleID) {
		return app.ArticleRevision{}, domain.ErrIssueNotFound
	}
	var j struct {
		Content string `json:"content"`
		Updated int64  `json:"updated"`
	}
	if err := c.get(ctx, baseURL+"/api/articles/"+url.PathEscape(articleID)+"?fields=content,updated", token, &j); err != nil {
		return app.ArticleRevision{}, err
	}
	return app.ArticleRevision{Content: j.Content, Updated: j.Updated}, nil
}

// UpdateArticle implements app.SyncClient: replaces the article's content.
func (c *Client) UpdateArticle(ctx context.Context, baseURL string, token domain.Secret, articleID, content string) error {
	if !articleIDRe.MatchString(articleID) {
		return domain.ErrIssueNotFound
	}
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}
	var out struct{}
	return c.do(ctx, "POST", baseURL+"/api/articles/"+url.PathEscape(articleID)+"?fields=id", token, body, &out)
}
