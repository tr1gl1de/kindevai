// Package domain is the integrations bounded context: the credentials vault's values, the installation's YouTrack
// connection, the binding of a project to a YouTrack project and the project's repository registry (KIN-26,
// KIN-A-19, KIN-A-21). Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotFound: no such project, repository or integration.
	ErrNotFound = errors.New("not found")
	// ErrForbidden: only the Owner manages integrations.
	ErrForbidden = errors.New("forbidden")
	// ErrSecretNotSet: the credential has not been stored.
	ErrSecretNotSet = errors.New("credential not set")
	// ErrNotConfigured: the integration has no connection yet.
	ErrNotConfigured = errors.New("integration not configured")
	// ErrRepositoryExists: owner/name is already registered (in this or another project).
	ErrRepositoryExists = errors.New("repository already registered")
	// ErrShortNameTaken: the YouTrack project is already bound to another project.
	ErrShortNameTaken = errors.New("youtrack project already bound to another project")
)

// ValidationError reports invalid input for one field with a stable code.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

func invalid(field, code string) error { return &ValidationError{Field: field, Code: code} }

// MaxSecretBytes bounds one stored credential (a PEM private key fits easily).
const MaxSecretBytes = 8 << 10

const redacted = "[redacted]"

// Secret is a credential in memory. Every way of printing it (fmt verbs, slog, JSON) yields "[redacted]"; only
// Reveal returns the value, at the point where an adapter puts it on the wire.
type Secret struct{ v string }

// NewSecret validates a credential: surrounding whitespace is trimmed (copy and paste), the rest must be non-empty,
// at most MaxSecretBytes, valid UTF-8 and without NUL.
func NewSecret(s string) (Secret, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Secret{}, invalid("secret", "required")
	case len(s) > MaxSecretBytes:
		return Secret{}, invalid("secret", "too_long")
	case !utf8.ValidString(s) || strings.ContainsRune(s, 0):
		return Secret{}, invalid("secret", "secret_format")
	}
	return Secret{v: s}, nil
}

// Reveal returns the plaintext. Call it only to hand the value to the external system.
func (s Secret) Reveal() string { return s.v }

// IsZero reports an unset secret.
func (s Secret) IsZero() bool { return s.v == "" }

func (Secret) String() string                { return redacted }
func (Secret) GoString() string              { return redacted }
func (Secret) Format(f fmt.State, _ rune)    { _, _ = f.Write([]byte(redacted)) }
func (Secret) LogValue() slog.Value          { return slog.StringValue(redacted) }
func (Secret) MarshalJSON() ([]byte, error)  { return []byte(`"` + redacted + `"`), nil }
func (Secret) MarshalText() ([]byte, error)  { return []byte(redacted), nil }
func (s *Secret) UnmarshalJSON([]byte) error { return errors.New("secrets are not decoded from JSON") }

// SecretName identifies a credential in the vault: dot-separated lower-case segments, e.g. "youtrack.token".
type SecretName string

// Well-known credentials. Adapters of later tasks add their own names in the same format (e.g. KIN-30:
// "network_profile.<id>.password").
const (
	SecretYouTrackToken         SecretName = "youtrack.token"
	SecretGitHubAppPrivateKey   SecretName = "github.app_private_key" //nolint:gosec // G101: a vault slot name, not a credential
	SecretGitHubWebhookSecret   SecretName = "github.webhook_secret"  //nolint:gosec // G101: a vault slot name, not a credential
	SecretTelegramBotToken      SecretName = "telegram.bot_token"     //nolint:gosec // G101: a vault slot name, not a credential
	SecretTelegramWebhookSecret SecretName = "telegram.webhook_secret"
)

var secretNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9][a-z0-9_-]*)+$`)

// NewSecretName validates a vault name (at most 100 bytes).
func NewSecretName(s string) (SecretName, error) {
	if len(s) > 100 || !secretNameRE.MatchString(s) {
		return "", invalid("name", "secret_name_format")
	}
	return SecretName(s), nil
}

// SecretMeta is everything about a stored credential that may be shown: whether it is set and when it changed.
type SecretMeta struct {
	Name      SecretName
	Set       bool
	UpdatedAt time.Time
	UpdatedBy string
}

// NewBaseURL validates and normalises a YouTrack base URL: http(s), a host, no credentials, query or fragment;
// scheme and host lower-cased, trailing slashes removed.
func NewBaseURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", invalid("baseUrl", "required")
	}
	if len(s) > 2048 || strings.ContainsFunc(s, unicode.IsControl) {
		return "", invalid("baseUrl", "base_url_format")
	}
	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return "", invalid("baseUrl", "base_url_format")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", invalid("baseUrl", "base_url_scheme")
	}
	if u.User != nil {
		return "", invalid("baseUrl", "base_url_credentials")
	}
	// The token travels in a header: plain HTTP only to this machine (a local YouTrack or a test double).
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return "", invalid("baseUrl", "base_url_https")
	}
	if u.Host == "" || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(s, "#") {
		return "", invalid("baseUrl", "base_url_format")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// NewYouTrackToken validates a YouTrack permanent token: printable ASCII without spaces (it goes into an
// Authorization header).
func NewYouTrackToken(s string) (Secret, error) {
	sec, err := NewSecret(s)
	if err != nil {
		return Secret{}, invalid("token", "required")
	}
	for _, r := range sec.v {
		if r <= ' ' || r > '~' {
			return Secret{}, invalid("token", "token_format")
		}
	}
	return sec, nil
}

var shortNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,49}$`)

// NewShortName validates a YouTrack project short name (upper-cased): a letter, then letters, digits or "_".
func NewShortName(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !shortNameRE.MatchString(s) {
		return "", invalid("shortName", "short_name_format")
	}
	return s, nil
}

// CheckStatus is the state of the last "test connection" run.
type CheckStatus string

const (
	CheckNone    CheckStatus = "none"
	CheckPending CheckStatus = "pending"
	CheckOK      CheckStatus = "ok"
	CheckFailed  CheckStatus = "failed"
)

// MaxCheckDetail bounds the stored detail of a check result.
const MaxCheckDetail = 200

// Check is the last connection test. ID ties an asynchronous result to the request that started it.
type Check struct {
	Status      CheckStatus
	ID          string
	Code        string // failure code, e.g. "unauthorized", "unreachable"
	Detail      string // short, credential-free detail: the account login on success, e.g. "HTTP 502" on failure
	RequestedAt *time.Time
	CheckedAt   *time.Time
}

// CheckResult is what a connection test found.
type CheckResult struct {
	OK     bool
	Code   string
	Detail string
}

// YouTrackConnection is the installation's single YouTrack connection (KIN-26 decision: one per installation in
// the MVP). The token lives in the vault under SecretYouTrackToken.
type YouTrackConnection struct {
	BaseURL   string
	UpdatedAt time.Time
	Check     Check
}

// NewYouTrackConnection is a connection to the (validated) baseURL that has not been tested yet.
func NewYouTrackConnection(baseURL string, now time.Time) YouTrackConnection {
	return YouTrackConnection{BaseURL: baseURL, UpdatedAt: now, Check: Check{Status: CheckNone}}
}

// CheckTimeout is how long a pending test may take before it is shown as failed ("timeout") and may be restarted,
// e.g. because the outbox message went dead.
const CheckTimeout = 2 * time.Minute

// CheckRunning reports a pending test younger than CheckTimeout.
func (c YouTrackConnection) CheckRunning(now time.Time) bool { return c.Check.Running(now) }

// CheckAt is the test as it should be shown at now: a pending test past CheckTimeout reads as failed with "timeout".
func (c YouTrackConnection) CheckAt(now time.Time) Check { return c.Check.At(now) }

// StartCheck marks a new connection test as pending.
func (c *YouTrackConnection) StartCheck(id string, now time.Time) { c.Check.Start(id, now) }

// FinishCheck records the result of test id; a result of a superseded or finished test is ignored (false).
func (c *YouTrackConnection) FinishCheck(id string, r CheckResult, now time.Time) bool {
	return c.Check.Finish(id, r, now)
}

// ResetCheck forgets the last test, e.g. because the URL or token changed.
func (c *YouTrackConnection) ResetCheck() { c.Check = Check{Status: CheckNone} }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// DefaultIntegrationBranch is where agent PRs go unless the Owner picks another branch.
const DefaultIntegrationBranch = "dev"

// protectedBranches are never written by automation (KIN-A-21: main/master are Owner-only).
var protectedBranches = []string{"main", "master"}

var (
	ownerRE    = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	branchRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

// Repository is a GitHub repository registered for a project.
type Repository struct {
	ID                string
	ProjectID         string
	Owner             string
	Name              string
	IntegrationBranch string
	// TestCommand is run by the runner in the worktree after an implementer's AI CLI (KIN-35): the objective test
	// evidence of the review loop. "" when none is configured (shown as a gap, never as a pass).
	TestCommand string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	GitHub      GitHubState // read from GitHub by the App (KIN-28)
}

// NewRepository validates a registration; an empty branch means DefaultIntegrationBranch.
func NewRepository(id, projectID, owner, name, branch string, now time.Time) (Repository, error) {
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	switch {
	case owner == "":
		return Repository{}, invalid("owner", "required")
	case !ownerRE.MatchString(owner):
		return Repository{}, invalid("owner", "owner_format")
	case name == "":
		return Repository{}, invalid("name", "required")
	case !repoNameRE.MatchString(name) || name == "." || name == "..":
		return Repository{}, invalid("name", "repo_name_format")
	}
	r := Repository{ID: id, ProjectID: projectID, Owner: owner, Name: name, CreatedAt: now,
		GitHub: GitHubState{Protection: ProtectionUnknown, Sync: Check{Status: CheckNone}}}
	if strings.TrimSpace(branch) == "" {
		branch = DefaultIntegrationBranch
	}
	if err := r.SetIntegrationBranch(branch, now); err != nil {
		return Repository{}, err
	}
	return r, nil
}

// FullName is "owner/name".
func (r Repository) FullName() string { return r.Owner + "/" + r.Name }

// ProtectedBranches are the branches automation never pushes to or merges into.
func (Repository) ProtectedBranches() []string { return append([]string(nil), protectedBranches...) }

// SetIntegrationBranch changes the branch agent PRs target; it must be a valid branch name and not protected.
func (r *Repository) SetIntegrationBranch(branch string, now time.Time) error {
	branch = strings.TrimSpace(branch)
	if !validBranch(branch) {
		return invalid("integrationBranch", "branch_format")
	}
	if IsProtectedBranch(branch) {
		return invalid("integrationBranch", "branch_protected")
	}
	r.IntegrationBranch, r.UpdatedAt = branch, now
	return nil
}

// MaxTestCommand bounds a repository's test command (characters).
const MaxTestCommand = 500

// SetTestCommand changes the repository's test command: one line of printable text ("" for none).
func (r *Repository) SetTestCommand(cmd string, now time.Time) error {
	cmd = strings.TrimSpace(cmd)
	if utf8.RuneCountInString(cmd) > MaxTestCommand || !utf8.ValidString(cmd) ||
		strings.ContainsFunc(cmd, func(c rune) bool { return unicode.IsControl(c) }) {
		return invalid("testCommand", "test_command_format")
	}
	r.TestCommand, r.UpdatedAt = cmd, now
	return nil
}

// validBranch is a conservative subset of git's ref name rules.
func validBranch(b string) bool {
	if !branchRE.MatchString(b) || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") ||
		strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") || strings.Contains(b, "..") || strings.Contains(b, "//") ||
		strings.Contains(b, "/.") || strings.HasPrefix(b, ".") {
		return false
	}
	return true
}
