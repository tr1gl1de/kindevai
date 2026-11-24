// Package github is the GitHub REST adapter of the integrations context (KIN-28): it authenticates as the GitHub
// App (RS256 JWT), mints installation tokens restricted to one repository and caches them in memory only, tests
// the App and reads a repository's installation, default branch and main/master protection. Every request passes
// a guard that refuses writes aimed at main/master (KIN-A-20). It runs from outbox deliveries and adapters, never
// from HTTP handlers. Tokens and keys never reach logs, errors or results.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// DefaultBaseURL is the GitHub.com REST API.
const DefaultBaseURL = "https://api.github.com"

const (
	maxResponseBytes = 256 << 10
	// tokenMargin: a cached token is used only while it has at least this long left, so a caller never starts
	// work with a token about to expire.
	tokenMargin = 5 * time.Minute
	// maxTokenTTL caps how long a token is cached, whatever GitHub says (installation tokens live one hour).
	maxTokenTTL     = 50 * time.Minute
	maxCachedTokens = 1000
)

// TokenPermissions are requested for every installation token: the App's documented minimum, so a token stays
// least-privilege even if the App is later granted more (GitHub refuses the mint if the App has less).
var TokenPermissions = map[string]string{"contents": "write", "pull_requests": "write", "checks": "read", "metadata": "read"}

// Client talks to the GitHub REST API as the App. The zero value uses DefaultBaseURL, a 15s timeout and time.Now.
type Client struct {
	BaseURL string           // without trailing slash; tests point it at an httptest server
	HTTP    *http.Client     // its Transport is wrapped by the main/master guard
	Timeout time.Duration    // per request, default 15s
	Now     func() time.Time // default time.Now

	once   sync.Once
	hc     *http.Client
	mu     sync.Mutex
	tokens map[tokenKey]cachedToken
}

var _ app.GitHubAPI = (*Client)(nil)

// tokenKey ties a cached token to the App credentials (by fingerprint: a rotated key never reuses old tokens), the
// installation and the one repository it is restricted to.
type tokenKey struct {
	app  [sha256.Size]byte
	inst int64
	repo string
}

type cachedToken struct {
	token domain.Secret
	until time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

func (c *Client) client() *http.Client {
	c.once.Do(func() {
		hc := http.Client{}
		if c.HTTP != nil {
			hc = *c.HTTP
		}
		rt := hc.Transport
		if rt == nil {
			rt = http.DefaultTransport
		}
		prefix := ""
		if u, err := url.Parse(c.base()); err == nil {
			prefix = u.Path
		}
		hc.Transport = Guard{Next: rt, Prefix: prefix}
		// Redirects are not followed: credentials go only to the configured API.
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		if hc.Timeout == 0 {
			hc.Timeout = 15 * time.Second
			if c.Timeout > 0 {
				hc.Timeout = c.Timeout
			}
		}
		c.hc = &hc
	})
	return c.hc
}

// AppJWT is the App's RS256 JWT: issued 60s in the past against clock drift, valid 9 minutes (GitHub allows 10).
func AppJWT(creds app.AppCredentials, now time.Time) (string, error) {
	key, err := domain.ParsePrivateKey(creds.PrivateKey)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(creds.AppID, 10),
	})
	if err != nil {
		return "", fmt.Errorf("encoding jwt claims: %w", err)
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", errors.New("signing app jwt failed")
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// failure is a call that did not give the expected answer: a stable code and a credential-free detail.
type failure struct{ code, detail string }

func (f *failure) Error() string { return f.code + ": " + f.detail }

func result(err error) domain.CheckResult {
	var f *failure
	if errors.As(err, &f) {
		return domain.CheckResult{Code: f.code, Detail: f.detail}
	}
	if errors.Is(err, domain.ErrProtectedBranch) {
		return domain.CheckResult{Code: "protected_branch"}
	}
	return domain.CheckResult{Code: "internal"}
}

// networkDetail names a network failure with a fixed phrase: raw dial errors reveal addresses and ports.
func networkDetail(err error) string {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var hdr tls.RecordHeaderError
	switch {
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return "timeout"
	case errors.As(err, &dns):
		return "DNS lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &cert) || errors.As(err, &hdr):
		return "TLS error"
	default:
		return "connection failed"
	}
}

// call sends one request with the given bearer credential and decodes a JSON answer into out (when non-nil).
// It returns the status code; statuses other than the wanted ones become failures.
func (c *Client) call(ctx context.Context, method, path, bearer string, body any, out any, want ...int) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encoding request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return 0, &failure{"bad_url", ""}
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "kindevai-control-plane")
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		if errors.Is(err, domain.ErrProtectedBranch) {
			return 0, domain.ErrProtectedBranch
		}
		return 0, &failure{"unreachable", networkDetail(err)}
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxResponseBytes)
	for _, w := range want {
		if resp.StatusCode == w {
			if out != nil && resp.StatusCode != http.StatusNotFound {
				if err := json.NewDecoder(limited).Decode(out); err != nil {
					return resp.StatusCode, &failure{"bad_response", ""}
				}
			}
			return resp.StatusCode, nil
		}
	}
	_, _ = io.Copy(io.Discard, limited)
	status := "HTTP " + strconv.Itoa(resp.StatusCode)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, &failure{"unauthorized", status}
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return resp.StatusCode, &failure{"forbidden", status}
	case resp.StatusCode == http.StatusNotFound:
		return resp.StatusCode, &failure{"not_found", status}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return resp.StatusCode, &failure{"redirected", status}
	default:
		return resp.StatusCode, &failure{"unexpected_status", status}
	}
}

// CheckApp authenticates as the App (GET /app) and reports its slug.
func (c *Client) CheckApp(ctx context.Context, creds app.AppCredentials) domain.CheckResult {
	jwt, err := AppJWT(creds, c.now())
	if err != nil {
		return domain.CheckResult{Code: "bad_key"}
	}
	var a struct {
		Slug string `json:"slug"`
	}
	if _, err := c.call(ctx, http.MethodGet, "/app", jwt, nil, &a, http.StatusOK); err != nil {
		return result(err)
	}
	return domain.CheckResult{OK: true, Detail: a.Slug}
}

func repoPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// InstallationToken returns a token of the installation restricted to the one repository. A cached token is reused
// while it has more than tokenMargin left; tokens are cached at most maxTokenTTL and only in this process' memory.
func (c *Client) InstallationToken(ctx context.Context, creds app.AppCredentials, installationID int64, repo string) (domain.Secret, error) {
	key := tokenKey{app: sha256.Sum256([]byte(strconv.FormatInt(creds.AppID, 10) + "\x00" + creds.PrivateKey.Reveal())), inst: installationID, repo: repo}
	now := c.now()
	c.mu.Lock()
	if t, ok := c.tokens[key]; ok && now.Before(t.until) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	jwt, err := AppJWT(creds, now)
	if err != nil {
		return domain.Secret{}, err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := c.call(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installationID, 10)+"/access_tokens", jwt,
		map[string]any{"repositories": []string{repo}, "permissions": TokenPermissions}, &out, http.StatusCreated); err != nil {
		return domain.Secret{}, fmt.Errorf("minting installation token: %w", err)
	}
	tok, err := domain.NewSecret(out.Token)
	if err != nil {
		return domain.Secret{}, &failure{"bad_response", "no token"}
	}
	until := out.ExpiresAt.Add(-tokenMargin)
	if limit := now.Add(maxTokenTTL); until.After(limit) {
		until = limit
	}
	if until.After(now) {
		c.mu.Lock()
		if c.tokens == nil || len(c.tokens) >= maxCachedTokens {
			c.prune(now)
		}
		c.tokens[key] = cachedToken{token: tok, until: until}
		c.mu.Unlock()
	}
	return tok, nil
}

// prune drops expired tokens; if the cache is still full it starts over (callers mint again). c.mu is held.
func (c *Client) prune(now time.Time) {
	if c.tokens == nil {
		c.tokens = map[tokenKey]cachedToken{}
		return
	}
	for k, t := range c.tokens {
		if !now.Before(t.until) {
			delete(c.tokens, k)
		}
	}
	if len(c.tokens) >= maxCachedTokens {
		clear(c.tokens)
	}
}

// ReadRepository finds the App's installation on the repository (App JWT), then, with a token restricted to it,
// reads the default branch and whether main/master exist and are protected by a ruleset or branch protection.
func (c *Client) ReadRepository(ctx context.Context, creds app.AppCredentials, owner, name string) domain.SyncResult {
	jwt, err := AppJWT(creds, c.now())
	if err != nil {
		return domain.SyncResult{CheckResult: domain.CheckResult{Code: "bad_key"}}
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	code, err := c.call(ctx, http.MethodGet, repoPath(owner, name)+"/installation", jwt, nil, &inst, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return domain.SyncResult{CheckResult: result(err)}
	}
	if code == http.StatusNotFound || inst.ID == 0 {
		return domain.SyncResult{CheckResult: domain.CheckResult{Code: "not_installed", Detail: "the App is not installed on the repository"}}
	}
	tok, err := c.InstallationToken(ctx, creds, inst.ID, name)
	if err != nil {
		return domain.SyncResult{CheckResult: result(err)}
	}
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.call(ctx, http.MethodGet, repoPath(owner, name), tok.Reveal(), nil, &repo, http.StatusOK); err != nil {
		return domain.SyncResult{CheckResult: result(err)}
	}
	state := domain.RepoState{InstallationID: inst.ID, DefaultBranch: repo.DefaultBranch}
	for _, b := range (domain.Repository{}).ProtectedBranches() {
		bp, err := c.branchProtection(ctx, tok, owner, name, b)
		if err != nil {
			return domain.SyncResult{CheckResult: result(err)}
		}
		state.Branches = append(state.Branches, bp)
	}
	st, _ := state.Protection()
	return domain.SyncResult{CheckResult: domain.CheckResult{OK: true, Detail: string(st)}, State: state}
}

// branchProtection reads one branch: GET .../branches/{b} gives existence and classic protection, GET
// .../rules/branches/{b} lists the ruleset rules that apply to it (also for a branch that does not exist yet).
func (c *Client) branchProtection(ctx context.Context, tok domain.Secret, owner, name, branch string) (domain.BranchProtection, error) {
	bp := domain.BranchProtection{Branch: branch}
	var br struct {
		Protected bool `json:"protected"`
	}
	code, err := c.call(ctx, http.MethodGet, repoPath(owner, name)+"/branches/"+url.PathEscape(branch), tok.Reveal(), nil, &br,
		http.StatusOK, http.StatusNotFound)
	if err != nil {
		return bp, err
	}
	bp.Exists, bp.Protected = code == http.StatusOK, br.Protected
	var rules []json.RawMessage
	code, err = c.call(ctx, http.MethodGet, repoPath(owner, name)+"/rules/branches/"+url.PathEscape(branch), tok.Reveal(), nil, &rules,
		http.StatusOK, http.StatusNotFound)
	if err != nil {
		return bp, err
	}
	if code == http.StatusOK && len(rules) > 0 {
		bp.Protected = true
	}
	return bp, nil
}
