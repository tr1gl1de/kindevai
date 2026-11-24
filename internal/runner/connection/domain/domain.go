// Package domain is the runner's connection context: its Ed25519 identity, the Control Plane it talks to and its
// enrollment there. Pure Go, no I/O.
package domain

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
)

// Errors of the connection context. Adapters map Control Plane responses onto them.
var (
	ErrServerURL           = errors.New("server URL must be an absolute https:// URL without credentials, query or fragment")
	ErrPlainHTTP           = errors.New("http:// is allowed only for a Control Plane on this host (localhost, 127.0.0.1, ::1); use https://")
	ErrTokenFormat         = errors.New("enrollment token is empty or malformed")
	ErrTokenInvalid        = errors.New("the Control Plane does not know this enrollment token")
	ErrTokenUsed           = errors.New("enrollment token was already used; create a new one in Dashboard → Runners")
	ErrTokenExpired        = errors.New("enrollment token expired; create a new one in Dashboard → Runners")
	ErrTokenRevoked        = errors.New("enrollment token was revoked; create a new one in Dashboard → Runners")
	ErrKeyEnrolled         = errors.New("this runner key is already enrolled with another token")
	ErrRateLimited         = errors.New("too many enrollment attempts; wait a minute and retry")
	ErrRejected            = errors.New("the Control Plane rejected the enrollment request")
	ErrFingerprintMismatch = errors.New("the Control Plane reports a different key fingerprint; the enrollment cannot be trusted")
	ErrNotEnrolled         = errors.New("runner is not enrolled; run `ai-runner enroll --server <url>` first")
	ErrInvalidKey          = errors.New("invalid Ed25519 private key")
)

// Identity is the runner's Ed25519 key pair. The private key never leaves the runner.
type Identity struct {
	key ed25519.PrivateKey
}

// NewIdentity wraps a private key.
func NewIdentity(key ed25519.PrivateKey) (Identity, error) {
	if len(key) != ed25519.PrivateKeySize {
		return Identity{}, ErrInvalidKey
	}
	return Identity{key: key}, nil
}

// PrivateKey is the key for signing (WSS challenge-response, KIN-19) and for persisting it.
func (i Identity) PrivateKey() ed25519.PrivateKey { return i.key }

// PublicKey is the raw 32-byte public key.
func (i Identity) PublicKey() ed25519.PublicKey { return i.key.Public().(ed25519.PublicKey) }

// PublicKeyBase64 is the public key as the Control Plane API expects it: standard base64 of the raw 32 bytes.
func (i Identity) PublicKeyBase64() string { return base64.StdEncoding.EncodeToString(i.PublicKey()) }

// Fingerprint is the OpenSSH fingerprint ("SHA256:" + unpadded base64 of SHA-256 over the SSH wire encoding of
// the key), the same string the Control Plane shows on the runner's card and `ssh-keygen -lf` prints.
func (i Identity) Fingerprint() string { return FingerprintOf(i.PublicKey()) }

// FingerprintOf is the OpenSSH fingerprint of a raw Ed25519 public key.
func FingerprintOf(pub ed25519.PublicKey) string {
	const algo = "ssh-ed25519"
	blob := make([]byte, 0, 4+len(algo)+4+len(pub))
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(algo)))
	blob = append(blob, algo...)
	blob = binary.BigEndian.AppendUint32(blob, uint32(len(pub))) //nolint:gosec // G115: len(pub) is 32
	blob = append(blob, pub...)
	h := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(h[:])
}

// ServerURL is the validated base URL of the Control Plane. TLS is mandatory except for a loopback host.
type ServerURL struct {
	u url.URL
}

// ParseServerURL accepts https:// with any host, and http:// only for localhost or a loopback address.
func ParseServerURL(s string) (ServerURL, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" {
		return ServerURL{}, ErrServerURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return ServerURL{}, ErrPlainHTTP
		}
	default:
		return ServerURL{}, ErrServerURL
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return ServerURL{u: *u}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// String is the canonical URL, without a trailing slash.
func (s ServerURL) String() string {
	if s.u.Host == "" {
		return ""
	}
	return s.u.String()
}

// Endpoint resolves an absolute API path (e.g. "/api/runners/enroll") against the base URL.
func (s ServerURL) Endpoint(path string) string {
	u := s.u
	u.Path += path
	return u.String()
}

// IsZero reports an unset URL.
func (s ServerURL) IsZero() bool { return s.u.Host == "" }

const maxTokenLen = 512

// Token is a one-time enrollment token. It formats as a placeholder so it never lands in logs or errors.
type Token struct {
	v string
}

// ParseToken trims surrounding whitespace and accepts one printable word.
func ParseToken(s string) (Token, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxTokenLen || strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || !unicode.IsPrint(r)
	}) >= 0 {
		return Token{}, ErrTokenFormat
	}
	return Token{v: s}, nil
}

// Reveal returns the token value for the request body; nothing else may call it.
func (t Token) Reveal() string { return t.v }

// String hides the value.
func (Token) String() string { return "[token]" }

// GoString hides the value from %#v.
func (Token) GoString() string { return "domain.Token{[token]}" }

// Format hides the value from every verb.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[token]")) }

// Enrollment is the runner's registration with one Control Plane.
type Enrollment struct {
	Server      ServerURL
	RunnerID    string
	Fingerprint string
	Status      string // as reported at enrollment time: "pending" until the Owner approves
}

// Enrolled reports whether the runner holds a registration.
func (e Enrollment) Enrolled() bool { return !e.Server.IsZero() && e.RunnerID != "" }

// Host is the runner's self-description sent at enrollment; informational, not proof of identity.
type Host struct {
	Hostname string
	OS       string
	Arch     string
	Version  string
}
