package github

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

type pullJSON struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Base    struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p pullJSON) ref(reused bool) domain.PullRequestRef {
	return domain.PullRequestRef{Number: p.Number, URL: p.HTMLURL, Base: p.Base.Ref, Reused: reused}
}

// OpenPullRequest opens the pull request of in.Head into in.Base with the installation token tok, or returns the
// open pull request of the head branch (KIN-36: idempotent, so an outbox redelivery never opens a second one). A
// protected base is refused before anything is sent (and again by the transport guard).
func (c *Client) OpenPullRequest(ctx context.Context, tok domain.Secret, owner, name string, in domain.PullRequestInput) (domain.PullRequestRef, error) {
	if err := domain.GuardBranch(in.Base); err != nil {
		return domain.PullRequestRef{}, err
	}
	if pr, ok, err := c.openPullRequest(ctx, tok, owner, name, in.Head, in.Base); err != nil || ok {
		return pr, err
	}
	var created pullJSON
	code, err := c.call(ctx, http.MethodPost, repoPath(owner, name)+"/pulls", tok.Reveal(), map[string]any{
		"title": in.Title, "head": in.Head, "base": in.Base, "body": in.Body, "maintainer_can_modify": false,
	}, &created, http.StatusCreated)
	if code != http.StatusUnprocessableEntity {
		if err != nil {
			return domain.PullRequestRef{}, err
		}
		return created.ref(false), nil
	}
	// 422: one exists already (a concurrent create, or one into another base), or GitHub refused the pull request
	// (no commits, bad head).
	if pr, ok, err := c.openPullRequest(ctx, tok, owner, name, in.Head, in.Base); err != nil || ok {
		return pr, err
	}
	return domain.PullRequestRef{}, &failure{"unprocessable", "GitHub refused the pull request (HTTP 422)"}
}

// openPullRequest finds the open pull request whose head is branch of the repository itself and whose base is base
// (one someone opened into another base is never adopted).
func (c *Client) openPullRequest(ctx context.Context, tok domain.Secret, owner, name, branch, base string) (domain.PullRequestRef, bool, error) {
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}, "base": {base}, "per_page": {"5"}}
	var list []pullJSON
	if _, err := c.call(ctx, http.MethodGet, repoPath(owner, name)+"/pulls?"+q.Encode(), tok.Reveal(), nil, &list,
		http.StatusOK); err != nil {
		return domain.PullRequestRef{}, false, err
	}
	for _, p := range list {
		if p.Base.Ref == base {
			return p.ref(true), true, nil
		}
	}
	return domain.PullRequestRef{}, false, nil
}
