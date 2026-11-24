package youtrack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// maxIssuesBytes bounds one issues response (a page of 50 snapshots is a few KiB; summaries are short).
const maxIssuesBytes = 4 << 20

// issueFields are the only fields requested: the description and comments are never read by the intake.
const issueFields = "idReadable,summary,updated,resolved,reporter(login),tags(name),project(shortName)"

// Client reads issues for the intake (KIN-27). It runs from the poller and from outbox deliveries, never from
// HTTP handlers. Redirects are not followed, so the token only ever goes to the configured base URL.
type Client struct {
	HTTP    *http.Client     // default: a plain client
	Timeout time.Duration    // per request, default 30s
	Now     func() time.Time // default time.Now; bounds the date window of a poll query
}

func (c *Client) client() *http.Client {
	h := http.Client{}
	if c.HTTP != nil {
		h = *c.HTTP
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &h
}

func (c *Client) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

type issueJSON struct {
	IDReadable string `json:"idReadable"`
	Summary    string `json:"summary"`
	Updated    int64  `json:"updated"`
	Resolved   *int64 `json:"resolved"`
	Reporter   *struct {
		Login string `json:"login"`
	} `json:"reporter"`
	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Project *struct {
		ShortName string `json:"shortName"`
	} `json:"project"`
}

func (j issueJSON) snapshot() domain.IssueSnapshot {
	s := domain.IssueSnapshot{ID: j.IDReadable, Summary: j.Summary, Updated: j.Updated, Resolved: j.Resolved != nil}
	if j.Reporter != nil {
		s.Reporter = j.Reporter.Login
	}
	if j.Project != nil {
		s.Project = j.Project.ShortName
	}
	for i, t := range j.Tags {
		if i == domain.MaxSnapshotTags {
			break
		}
		s.Tags = append(s.Tags, t.Name)
	}
	return s
}

// query is the YouTrack search for a poll page. The date window is coarse (YouTrack compares whole days in the
// token owner's time zone), so it starts a day before the cursor; already received issues are deduplicated by the
// inbox and a late page never loses an issue.
func (c *Client) query(q app.IssueQuery) string {
	s := fmt.Sprintf("project: {%s} tag: {%s}", q.ShortName, q.Tag)
	if q.Since > 0 {
		from := time.UnixMilli(q.Since).UTC().AddDate(0, 0, -1).Format(time.DateOnly)
		to := c.now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
		s += fmt.Sprintf(" updated: %s .. %s", from, to)
	}
	return s + " sort by: updated asc"
}

// UpdatedIssues implements app.IssueSource.
func (c *Client) UpdatedIssues(ctx context.Context, baseURL string, token domain.Secret, q app.IssueQuery) ([]domain.IssueSnapshot, error) {
	v := url.Values{}
	v.Set("query", c.query(q))
	v.Set("fields", issueFields)
	v.Set("$skip", strconv.Itoa(q.Skip))
	v.Set("$top", strconv.Itoa(q.Top))
	var out []issueJSON
	if err := c.get(ctx, baseURL+"/api/issues?"+v.Encode(), token, &out); err != nil {
		return nil, err
	}
	issues := make([]domain.IssueSnapshot, 0, len(out))
	for _, j := range out {
		issues = append(issues, j.snapshot())
	}
	return issues, nil
}

// Issue implements app.IssueSource.
func (c *Client) Issue(ctx context.Context, baseURL string, token domain.Secret, issueID string) (domain.IssueSnapshot, error) {
	id, err := domain.NewIssueID(issueID)
	if err != nil {
		return domain.IssueSnapshot{}, err
	}
	var j issueJSON
	if err := c.get(ctx, baseURL+"/api/issues/"+url.PathEscape(id)+"?fields="+url.QueryEscape(issueFields), token, &j); err != nil {
		return domain.IssueSnapshot{}, err
	}
	return j.snapshot(), nil
}

func (c *Client) get(ctx context.Context, u string, token domain.Secret, dst any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("youtrack: bad base URL")
	}
	req.Header.Set("Authorization", "Bearer "+token.Reveal())
	req.Header.Set("Accept", "application/json")
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("youtrack: %s", networkDetail(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return domain.ErrIssueNotFound
	case resp.StatusCode != http.StatusOK:
		return &app.StatusError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIssuesBytes+1))
	if err != nil {
		return fmt.Errorf("youtrack: reading response: %s", networkDetail(err))
	}
	if len(body) > maxIssuesBytes {
		return errors.New("youtrack: response too large")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errors.New("youtrack: malformed response")
	}
	return nil
}

// retryAfter parses a Retry-After in seconds (HTTP dates are rare from YouTrack and ignored), capped at an hour.
func retryAfter(h string) time.Duration {
	n, err := strconv.Atoi(h)
	if err != nil || n <= 0 {
		return 0
	}
	return min(time.Duration(n)*time.Second, time.Hour)
}
