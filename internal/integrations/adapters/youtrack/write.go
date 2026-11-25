package youtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// Issue creation (KIN-61). Like the reads, it runs only from outbox deliveries.

// ProjectID implements app.IssueClient: YouTrack creates issues by the project's database id.
func (c *Client) ProjectID(ctx context.Context, baseURL string, token domain.Secret, shortName string) (string, error) {
	v := url.Values{}
	v.Set("fields", "id,shortName")
	v.Set("query", shortName)
	v.Set("$top", "50")
	var out []struct {
		ID        string `json:"id"`
		ShortName string `json:"shortName"`
	}
	if err := c.get(ctx, baseURL+"/api/admin/projects?"+v.Encode(), token, &out); err != nil {
		return "", err
	}
	for _, p := range out {
		if strings.EqualFold(p.ShortName, shortName) && p.ID != "" {
			return p.ID, nil
		}
	}
	return "", domain.ErrNotFound
}

// FindIssue implements app.IssueClient: the project's issue whose description contains marker as a whole word.
func (c *Client) FindIssue(ctx context.Context, baseURL string, token domain.Secret, shortName, marker string) (string, bool, error) {
	v := url.Values{}
	v.Set("query", fmt.Sprintf("project: {%s} %q", shortName, marker))
	v.Set("fields", "idReadable,description")
	v.Set("$top", "10")
	var out []struct {
		IDReadable  string `json:"idReadable"`
		Description string `json:"description"`
	}
	if err := c.get(ctx, baseURL+"/api/issues?"+v.Encode(), token, &out); err != nil {
		return "", false, err
	}
	for _, j := range out {
		if hasToken(j.Description, marker) {
			return j.IDReadable, true, nil
		}
	}
	return "", false, nil
}

func tokenByte(b byte) bool {
	return b == '-' || b == '_' || ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// hasToken reports tok in s, not as part of a longer token ("alert-A1" is not in "alert-A12").
func hasToken(s, tok string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		if (start == 0 || !tokenByte(s[start-1])) && (end == len(s) || !tokenByte(s[end])) {
			return true
		}
		i = start + 1
	}
}

// CreateIssue implements app.IssueClient.
func (c *Client) CreateIssue(ctx context.Context, baseURL string, token domain.Secret, projectID string, n app.NewIssue) (string, error) {
	body, err := json.Marshal(map[string]any{
		"project":     map[string]string{"id": projectID},
		"summary":     n.Summary,
		"description": n.Description,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		IDReadable string `json:"idReadable"`
	}
	if err := c.do(ctx, "POST", baseURL+"/api/issues?fields=idReadable", token, body, &out); err != nil {
		return "", err
	}
	return out.IDReadable, nil
}
