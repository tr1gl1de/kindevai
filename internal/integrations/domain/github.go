package domain

import (
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// GitHub App connection, webhook verification and the main/master guard (KIN-28, KIN-A-19, KIN-A-20).

var (
	// ErrProtectedBranch: main/master are Owner-only; automation never pushes to, opens PRs against or merges into
	// them (KIN-A-20). Every GitHub write path checks GuardBranch.
	ErrProtectedBranch = errors.New("main/master are Owner-only: automation never writes to them")
	// ErrNotInstalled: the GitHub App is not installed on the repository (or it was not read from GitHub yet).
	ErrNotInstalled = errors.New("github app not installed on the repository")
	// ErrBadSignature: a webhook delivery is not signed with the configured webhook secret.
	ErrBadSignature = errors.New("webhook signature invalid")
)

// IsProtectedBranch reports main/master in any case, also as a full ref ("refs/heads/main").
func IsProtectedBranch(branch string) bool {
	b := strings.TrimSpace(branch)
	if len(b) > len("refs/heads/") && strings.EqualFold(b[:len("refs/heads/")], "refs/heads/") {
		b = b[len("refs/heads/"):]
	}
	return slices.ContainsFunc(protectedBranches, func(p string) bool { return strings.EqualFold(b, p) })
}

// GuardBranch is the application-level main/master rule: ErrProtectedBranch for a protected target branch. GitHub
// rulesets do not always stop a PR from being opened, so the platform enforces it in code before every write.
func GuardBranch(branch string) error {
	if IsProtectedBranch(branch) {
		return ErrProtectedBranch
	}
	return nil
}

// NewAppID validates a GitHub App ID: a positive decimal number.
func NewAppID(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, invalid("appId", "required")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 || id > 1<<53 {
		return 0, invalid("appId", "app_id_format")
	}
	return id, nil
}

// NewPrivateKey validates a GitHub App private key: one PEM block holding an RSA key (PKCS#1, as GitHub issues it,
// or PKCS#8).
func NewPrivateKey(s string) (Secret, error) {
	sec, err := NewSecret(s)
	if err != nil {
		return Secret{}, invalid("privateKey", "required")
	}
	if _, err := ParsePrivateKey(sec); err != nil {
		return Secret{}, invalid("privateKey", "private_key_format")
	}
	return sec, nil
}

// ParsePrivateKey decodes a private key for signing. Errors never contain key material.
func ParsePrivateKey(s Secret) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode([]byte(s.v))
	if block == nil || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("private key: not a single PEM block")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("private key: invalid PKCS#1 RSA key")
		}
		return k, nil
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("private key: invalid PKCS#8 key")
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key: not an RSA key")
		}
		return rk, nil
	}
	return nil, errors.New("private key: unexpected PEM type")
}

// MinWebhookSecretLen is the shortest webhook secret accepted (it is the only proof a delivery comes from GitHub).
const MinWebhookSecretLen = 16

// NewWebhookSecret validates a webhook secret: 16..256 printable ASCII characters without spaces.
func NewWebhookSecret(s string) (Secret, error) {
	sec, err := NewSecret(s)
	if err != nil {
		return Secret{}, invalid("webhookSecret", "required")
	}
	if len(sec.v) < MinWebhookSecretLen && utf8OnlyASCII(sec.v) {
		return Secret{}, invalid("webhookSecret", "webhook_secret_short")
	}
	if len(sec.v) > 256 || !utf8OnlyASCII(sec.v) {
		return Secret{}, invalid("webhookSecret", "webhook_secret_format")
	}
	return sec, nil
}

func utf8OnlyASCII(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' })
}

// VerifySignature checks an X-Hub-Signature-256 header ("sha256=<hex HMAC-SHA256 of the body>") in constant time.
func VerifySignature(secret Secret, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || secret.IsZero() {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret.v))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// GitHubApp is the installation's GitHub App (one per installation in the MVP). Its private key and webhook secret
// live in the vault under SecretGitHubAppPrivateKey and SecretGitHubWebhookSecret.
type GitHubApp struct {
	AppID          int64
	UpdatedAt      time.Time
	Check          Check
	LastDeliveryAt *time.Time // last webhook delivery processed
	LastEvent      string     // its event name
}

// NewGitHubApp is an App connection that has not been tested yet.
func NewGitHubApp(appID int64, now time.Time) GitHubApp {
	return GitHubApp{AppID: appID, UpdatedAt: now, Check: Check{Status: CheckNone}}
}

// Delivered records a processed webhook delivery.
func (a *GitHubApp) Delivered(event string, at time.Time) {
	a.LastDeliveryAt, a.LastEvent = &at, truncate(event, 50)
}

// Running reports a pending check younger than CheckTimeout.
func (c Check) Running(now time.Time) bool {
	return c.Status == CheckPending && c.RequestedAt != nil && now.Sub(*c.RequestedAt) <= CheckTimeout
}

// At is the check as it should be shown at now: a pending check past CheckTimeout reads as failed with "timeout".
func (c Check) At(now time.Time) Check {
	if c.Status == CheckPending && !c.Running(now) {
		c.Status, c.Code = CheckFailed, "timeout"
	}
	return c
}

// Start marks a new check as pending.
func (c *Check) Start(id string, now time.Time) {
	*c = Check{Status: CheckPending, ID: id, RequestedAt: &now}
}

// Finish records the result of check id; a result of a superseded or finished check is ignored (false).
func (c *Check) Finish(id string, r CheckResult, now time.Time) bool {
	if c.Status != CheckPending || c.ID != id {
		return false
	}
	c.Status, c.Code = CheckFailed, r.Code
	if r.OK {
		c.Status, c.Code = CheckOK, ""
	}
	c.Detail = truncate(r.Detail, MaxCheckDetail)
	c.CheckedAt = &now
	return true
}

// ProtectionStatus summarises how main/master of a repository are protected on GitHub.
type ProtectionStatus string

const (
	ProtectionUnknown     ProtectionStatus = "unknown"     // not read yet, or the read failed
	ProtectionProtected   ProtectionStatus = "protected"   // every existing main/master has a ruleset or branch protection
	ProtectionUnprotected ProtectionStatus = "unprotected" // an existing main/master has neither: warn the Owner
)

// BranchProtection is what GitHub reports for one protected-name branch.
type BranchProtection struct {
	Branch    string
	Exists    bool
	Protected bool // a ruleset applies to it or classic branch protection is on
}

// RepoState is a repository as read from GitHub with the App's credentials.
type RepoState struct {
	InstallationID int64
	DefaultBranch  string
	Branches       []BranchProtection
}

// Protection derives the status and the unprotected branches. A protected name that does not exist needs no
// protection yet: the code guard still refuses to write to it.
func (s RepoState) Protection() (ProtectionStatus, []string) {
	var unprotected []string
	for _, b := range s.Branches {
		if b.Exists && !b.Protected {
			unprotected = append(unprotected, b.Branch)
		}
	}
	if len(unprotected) > 0 {
		return ProtectionUnprotected, unprotected
	}
	return ProtectionProtected, nil
}

// SyncResult is the outcome of reading a repository from GitHub; State is meaningful only when OK.
type SyncResult struct {
	CheckResult
	State RepoState
}

// GitHubState is the part of a registered repository that is read from GitHub.
type GitHubState struct {
	InstallationID int64
	DefaultBranch  string
	Protection     ProtectionStatus
	Unprotected    []string
	Sync           Check
}

// StartSync marks a new read from GitHub as pending.
func (r *Repository) StartSync(id string, now time.Time) { r.GitHub.Sync.Start(id, now) }

// FinishSync applies the result of read id; a superseded or finished read is ignored (false). A failed read keeps
// the last known state, except "not_installed", which forgets the installation.
func (r *Repository) FinishSync(id string, res SyncResult, now time.Time) bool {
	if !r.GitHub.Sync.Finish(id, res.CheckResult, now) {
		return false
	}
	switch {
	case res.OK:
		r.GitHub.InstallationID = res.State.InstallationID
		r.GitHub.DefaultBranch = truncate(res.State.DefaultBranch, 200)
		r.GitHub.Protection, r.GitHub.Unprotected = res.State.Protection()
	case res.Code == "not_installed":
		r.GitHub.InstallationID, r.GitHub.Protection, r.GitHub.Unprotected = 0, ProtectionUnknown, nil
	}
	return true
}

// Webhook events the platform subscribes to (the App's minimal event set) and "ping", sent when a hook is created.
const (
	EventPing        = "ping"
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventCheckSuite  = "check_suite"
	EventCheckRun    = "check_run"
)

// SupportedEvent reports an event the platform processes; others are acknowledged and dropped.
func SupportedEvent(event string) bool {
	switch event {
	case EventPing, EventPush, EventPullRequest, EventCheckSuite, EventCheckRun:
		return true
	}
	return false
}

// GitHubEvent is the compact form of a webhook delivery kept in the inbox: only what workflows need, no secrets and
// bounded in size, whatever GitHub sent.
type GitHubEvent struct {
	Event          string `json:"event"`
	Action         string `json:"action,omitempty"`
	Repository     string `json:"repository,omitempty"` // owner/name
	InstallationID int64  `json:"installationId,omitempty"`
	Branch         string `json:"branch,omitempty"`  // push: the pushed branch; pull_request/check_suite: the head branch
	Base           string `json:"base,omitempty"`    // pull_request: the target branch
	HeadSHA        string `json:"headSha,omitempty"` // push: "after"; pull_request/checks: the head commit
	PullRequest    int    `json:"pullRequest,omitempty"`
	Merged         bool   `json:"merged,omitempty"`
	Status         string `json:"status,omitempty"`     // checks: queued | in_progress | completed
	Conclusion     string `json:"conclusion,omitempty"` // checks: success | failure | ...
	Name           string `json:"name,omitempty"`       // check_run name
}

type ghRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type ghPayload struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	PullRequest *struct {
		Number int   `json:"number"`
		Merged bool  `json:"merged"`
		Head   ghRef `json:"head"`
		Base   ghRef `json:"base"`
	} `json:"pull_request"`
	CheckSuite *struct {
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_suite"`
	CheckRun *struct {
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_run"`
}

// ParseGitHubEvent reduces a verified webhook body of a supported event to a GitHubEvent. Every event but ping
// must name its repository.
func ParseGitHubEvent(event string, body []byte) (GitHubEvent, error) {
	if !SupportedEvent(event) {
		return GitHubEvent{}, invalid("event", "event_unsupported")
	}
	var p ghPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return GitHubEvent{}, invalid("payload", "payload_format")
	}
	e := GitHubEvent{Event: event, Action: short(p.Action)}
	if p.Repository != nil {
		e.Repository = short(p.Repository.FullName)
	}
	if p.Installation != nil {
		e.InstallationID = p.Installation.ID
	}
	switch event {
	case EventPush:
		e.Branch, e.HeadSHA = short(strings.TrimPrefix(p.Ref, "refs/heads/")), short(p.After)
	case EventPullRequest:
		if p.PullRequest != nil {
			e.PullRequest, e.Merged = p.PullRequest.Number, p.PullRequest.Merged
			e.Branch, e.Base, e.HeadSHA = short(p.PullRequest.Head.Ref), short(p.PullRequest.Base.Ref), short(p.PullRequest.Head.SHA)
		}
	case EventCheckSuite:
		if p.CheckSuite != nil {
			e.Branch, e.HeadSHA = short(p.CheckSuite.HeadBranch), short(p.CheckSuite.HeadSHA)
			e.Status, e.Conclusion = short(p.CheckSuite.Status), short(p.CheckSuite.Conclusion)
		}
	case EventCheckRun:
		if p.CheckRun != nil {
			e.Name, e.HeadSHA = short(p.CheckRun.Name), short(p.CheckRun.HeadSHA)
			e.Status, e.Conclusion = short(p.CheckRun.Status), short(p.CheckRun.Conclusion)
		}
	}
	if event != EventPing && e.Repository == "" {
		return GitHubEvent{}, invalid("payload", "repository_required")
	}
	return e, nil
}

func short(s string) string { return truncate(strings.TrimSpace(s), 200) }
