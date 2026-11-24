// Package domain is the runners bounded context: enrollment tokens and the identity, status and project
// permissions of enrolled runner machines (KIN-A-17). Pure Go, no I/O.
package domain

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotFound: no such runner or enrollment token.
	ErrNotFound = errors.New("not found")
	// ErrForbidden: only the Owner manages runners.
	ErrForbidden = errors.New("forbidden")
	// ErrInvalidTransition: the runner's status does not allow the action.
	ErrInvalidTransition = errors.New("runner status does not allow this action")
	// ErrKeyEnrolled: the public key already belongs to another runner (a key enrolls once; re-enrollment uses a
	// new key).
	ErrKeyEnrolled = errors.New("public key already enrolled")

	// Token errors are specific on purpose: whoever presents the token already holds it. An unknown token is
	// ErrTokenInvalid.
	ErrTokenInvalid = errors.New("invalid enrollment token")
	ErrTokenUsed    = errors.New("enrollment token already used")
	ErrTokenRevoked = errors.New("enrollment token revoked")
	ErrTokenExpired = errors.New("enrollment token expired")
)

// ValidationError reports invalid input for one field with a stable code.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

// EnrollmentTTL is how long an enrollment token stays usable (KIN-A-17 recommendation, KIN-17 decision).
const EnrollmentTTL = 10 * time.Minute

// TokenState is derived from the timestamps; nothing stores it.
type TokenState string

const (
	TokenPending TokenState = "pending"
	TokenUsed    TokenState = "used"
	TokenRevoked TokenState = "revoked"
	TokenExpired TokenState = "expired"
)

// EnrollmentToken is a one-time permission to register one runner. Only the SHA-256 of the 256-bit token is
// stored; it never approves anything by itself.
type EnrollmentToken struct {
	ID         string
	TokenHash  []byte
	CreatedBy  string // the Owner who issued it; becomes the runner's CreatedBy
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	RunnerID   string // the runner it registered, once consumed
	RevokedAt  *time.Time
}

// NewEnrollmentToken issues a token valid for EnrollmentTTL and returns it with the plaintext (shown once).
func NewEnrollmentToken(rnd io.Reader, id, createdBy string, now time.Time) (EnrollmentToken, string, error) {
	var b [32]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return EnrollmentToken{}, "", fmt.Errorf("generating enrollment token: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b[:])
	return EnrollmentToken{ID: id, TokenHash: HashEnrollmentToken(plain), CreatedBy: createdBy, CreatedAt: now,
		ExpiresAt: now.Add(EnrollmentTTL)}, plain, nil
}

// HashEnrollmentToken is the lookup key for a presented token; a fast hash suffices for 256 random bits.
func HashEnrollmentToken(plain string) []byte {
	h := sha256.Sum256([]byte(plain))
	return h[:]
}

// State reports the token's state at now. Used and revoked win over expired.
func (t EnrollmentToken) State(now time.Time) TokenState {
	switch {
	case t.ConsumedAt != nil:
		return TokenUsed
	case t.RevokedAt != nil:
		return TokenRevoked
	case !now.Before(t.ExpiresAt):
		return TokenExpired
	}
	return TokenPending
}

// Usable returns nil for a pending token and the matching error otherwise.
func (t EnrollmentToken) Usable(now time.Time) error {
	switch t.State(now) {
	case TokenUsed:
		return ErrTokenUsed
	case TokenRevoked:
		return ErrTokenRevoked
	case TokenExpired:
		return ErrTokenExpired
	}
	return nil
}

// PublicKey is a runner's raw 32-byte Ed25519 public key. The private key never leaves the runner.
type PublicKey []byte

// ParsePublicKey decodes standard base64 of the raw 32-byte Ed25519 public key.
func ParsePublicKey(s string) (PublicKey, error) {
	bad := &ValidationError{Field: "publicKey", Code: "public_key_format"}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return nil, bad
	}
	if !slices.ContainsFunc(b, func(c byte) bool { return c != 0 }) {
		return nil, bad
	}
	return PublicKey(b), nil
}

// String is the key as standard base64, the form the API accepts and returns.
func (k PublicKey) String() string { return base64.StdEncoding.EncodeToString(k) }

// Fingerprint is the OpenSSH fingerprint ("SHA256:" + unpadded base64 of SHA-256 over the SSH wire encoding of
// the key), identical to `ssh-keygen -lf` for the same key, so the Owner can compare it with the runner machine.
func (k PublicKey) Fingerprint() string {
	const algo = "ssh-ed25519"
	blob := make([]byte, 0, 4+len(algo)+4+len(k))
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(algo)))
	blob = append(blob, algo...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(k))) //nolint:gosec // G115: len(k) is 32
	blob = append(blob, k...)
	h := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// Metadata is what the runner reports about itself at enrollment. It is informational, not proof of identity.
type Metadata struct {
	Hostname string
	OS       string // GOOS, e.g. linux, darwin
	Arch     string // GOARCH, e.g. amd64, arm64
	Version  string // ai-runner version, optional
}

var platformRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

func printable(s string, maxLen int) bool {
	return utf8.ValidString(s) && len(s) <= maxLen && strings.IndexFunc(s, unicode.IsControl) < 0
}

// NewMetadata validates the runner's self-description.
func NewMetadata(hostname, os, arch, version string) (Metadata, error) {
	m := Metadata{Hostname: strings.TrimSpace(hostname), OS: os, Arch: arch, Version: strings.TrimSpace(version)}
	switch {
	case m.Hostname == "" || !printable(m.Hostname, 253):
		return Metadata{}, &ValidationError{Field: "hostname", Code: "hostname_format"}
	case !platformRe.MatchString(m.OS):
		return Metadata{}, &ValidationError{Field: "os", Code: "os_format"}
	case !platformRe.MatchString(m.Arch):
		return Metadata{}, &ValidationError{Field: "arch", Code: "arch_format"}
	case !printable(m.Version, 64):
		return Metadata{}, &ValidationError{Field: "version", Code: "version_format"}
	}
	return m, nil
}

// Status is a runner's lifecycle state.
type Status string

const (
	StatusPending  Status = "pending"  // enrolled, waiting for the Owner; may not connect
	StatusApproved Status = "approved" // may connect and run tasks of its projects
	StatusDisabled Status = "disabled" // temporarily blocked by the Owner; can be enabled again
	StatusRevoked  Status = "revoked"  // terminal; the key is never accepted again
)

// Runner is the aggregate for one enrolled machine: its identity (key, fingerprint), lifecycle status and the
// projects it may execute. ProjectIDs belong to the projects context and are opaque here.
type Runner struct {
	ID                string
	PublicKey         PublicKey
	Fingerprint       string
	Metadata          Metadata
	Status            Status
	CreatedBy         string // the Owner who issued the enrollment token
	EnrollmentTokenID string
	EnrolledAt        time.Time
	ApprovedBy        string
	ApprovedAt        *time.Time
	DisabledAt        *time.Time
	RevokedBy         string
	RevokedAt         *time.Time
	LastSeenAt        *time.Time // set by the gateway (KIN-19/20)
	ProjectIDs        []string   // sorted, unique
}

// Enroll registers a pending runner for a usable token (the caller checks Usable and consumes the token).
func Enroll(id string, tok EnrollmentToken, key PublicKey, m Metadata, now time.Time) Runner {
	return Runner{ID: id, PublicKey: key, Fingerprint: key.Fingerprint(), Metadata: m, Status: StatusPending,
		CreatedBy: tok.CreatedBy, EnrollmentTokenID: tok.ID, EnrolledAt: now}
}

func projectSet(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) == 0 {
		return nil, &ValidationError{Field: "projectIds", Code: "projects_required"}
	}
	return out, nil
}

// Approve lets a pending runner connect, with an explicit, non-empty list of projects.
func (r *Runner) Approve(projectIDs []string, by string, now time.Time) error {
	if r.Status != StatusPending {
		return ErrInvalidTransition
	}
	ps, err := projectSet(projectIDs)
	if err != nil {
		return err
	}
	r.Status, r.ApprovedBy, r.ApprovedAt, r.ProjectIDs = StatusApproved, by, &now, ps
	return nil
}

// SetProjects replaces the projects of an approved or disabled runner (a pending one gets them on approval).
func (r *Runner) SetProjects(projectIDs []string) error {
	if r.Status != StatusApproved && r.Status != StatusDisabled {
		return ErrInvalidTransition
	}
	ps, err := projectSet(projectIDs)
	if err != nil {
		return err
	}
	r.ProjectIDs = ps
	return nil
}

// Disable blocks an approved runner until it is enabled again.
func (r *Runner) Disable(now time.Time) error {
	if r.Status != StatusApproved {
		return ErrInvalidTransition
	}
	r.Status, r.DisabledAt = StatusDisabled, &now
	return nil
}

// Enable lets a disabled runner connect again.
func (r *Runner) Enable(_ time.Time) error {
	if r.Status != StatusDisabled {
		return ErrInvalidTransition
	}
	r.Status, r.DisabledAt = StatusApproved, nil
	return nil
}

// Revoke permanently rejects the runner's key; for a pending runner it is the rejection of the enrollment.
func (r *Runner) Revoke(by string, now time.Time) error {
	if r.Status == StatusRevoked {
		return ErrInvalidTransition
	}
	r.Status, r.RevokedBy, r.RevokedAt = StatusRevoked, by, &now
	return nil
}

// MayConnect: only an approved runner passes the gateway, even with a valid signature (KIN-A-17).
func (r Runner) MayConnect() bool { return r.Status == StatusApproved }

// MayRun: an approved runner executes tasks only of the projects the Owner granted it.
func (r Runner) MayRun(projectID string) bool {
	return r.MayConnect() && slices.Contains(r.ProjectIDs, projectID)
}
