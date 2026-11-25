package github

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// maxCheckRuns bounds the check runs read per commit (one page).
const maxCheckRuns = 100

// CheckRuns lists the latest check runs of commit sha (KIN-37), at most maxCheckRuns, and how many there are.
func (c *Client) CheckRuns(ctx context.Context, tok domain.Secret, owner, name, sha string) ([]domain.CheckRun, int, error) {
	q := url.Values{"filter": {"latest"}, "per_page": {strconv.Itoa(maxCheckRuns)}}
	var out struct {
		TotalCount int `json:"total_count"`
		CheckRuns  []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoPath(owner, name)+"/commits/"+url.PathEscape(sha)+"/check-runs?"+q.Encode(),
		tok.Reveal(), nil, &out, http.StatusOK); err != nil {
		return nil, 0, err
	}
	runs := make([]domain.CheckRun, 0, len(out.CheckRuns))
	for _, r := range out.CheckRuns {
		runs = append(runs, domain.CheckRun{Name: r.Name, Status: r.Status, Conclusion: r.Conclusion})
	}
	return runs, max(out.TotalCount, len(runs)), nil
}

// PullRequest reads pull request number.
func (c *Client) PullRequest(ctx context.Context, tok domain.Secret, owner, name string, number int) (domain.PullRequestState, error) {
	var p struct {
		Number         int    `json:"number"`
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Head           ghRef  `json:"head"`
		Base           ghRef  `json:"base"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoPath(owner, name)+"/pulls/"+strconv.Itoa(number), tok.Reveal(), nil, &p,
		http.StatusOK); err != nil {
		return domain.PullRequestState{}, err
	}
	return domain.PullRequestState{Number: p.Number, State: p.State, Merged: p.Merged, MergeCommitSHA: p.MergeCommitSHA,
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA, Base: p.Base.Ref}, nil
}

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// MergePullRequest merges pull request number into base with a merge commit, only while its head is sha (GitHub
// checks it), and returns the merge commit. base is the base the caller has just read: main/master are refused
// before anything is sent (and by the transport guard). 409 is domain.ErrHeadMoved, 405 domain.ErrNotMergeable.
func (c *Client) MergePullRequest(ctx context.Context, tok domain.Secret, owner, name string, number int, sha, base string) (string, error) {
	ctx, err := AllowMerge(ctx, base)
	if err != nil {
		return "", err
	}
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	code, err := c.call(ctx, http.MethodPut, repoPath(owner, name)+"/pulls/"+strconv.Itoa(number)+"/merge", tok.Reveal(),
		map[string]any{"sha": sha, "merge_method": "merge"}, &out, http.StatusOK)
	switch {
	case code == http.StatusConflict:
		return "", domain.ErrHeadMoved
	case code == http.StatusMethodNotAllowed || code == http.StatusUnprocessableEntity:
		return "", domain.ErrNotMergeable
	case err != nil:
		return "", err
	case !out.Merged:
		return "", domain.ErrNotMergeable
	}
	return out.SHA, nil
}
