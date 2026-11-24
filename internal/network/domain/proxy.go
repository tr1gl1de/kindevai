// Package domain is the network bounded context (KIN-30, KB KIN-A-22): network profiles (upstream proxy, expected
// egress IP, enforcement mode, DNS policy, fail-closed, destination allowlist), their assignment along the hierarchy
// System → Runner → Project → CLI role, the resolution of the effective profile and Test Network results. Pure Go,
// no I/O. Enforcement itself (namespaces, nftables) is the runner's job (KIN-31); here profiles are data.
package domain

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	// ErrNotFound: no such profile, runner, project or test.
	ErrNotFound = errors.New("not found")
	// ErrForbidden: only the Owner manages network profiles.
	ErrForbidden = errors.New("forbidden")
	// ErrInUse: the profile is still assigned somewhere.
	ErrInUse = errors.New("profile in use")
	// ErrNameTaken: another profile has this name.
	ErrNameTaken = errors.New("profile name taken")
	// ErrTestFinished: a result arrived for a test that is no longer pending.
	ErrTestFinished = errors.New("test already finished")
)

// ValidationError reports invalid input for one field with a stable code. It never carries the input itself, so
// a proxy URL with credentials cannot leak through an error.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

func invalid(field, code string) error { return &ValidationError{Field: field, Code: code} }

// Scheme is the proxy protocol.
type Scheme string

const (
	SchemeHTTP   Scheme = "http"   // HTTP CONNECT over plain TCP to the proxy
	SchemeHTTPS  Scheme = "https"  // HTTP CONNECT over TLS to the proxy
	SchemeSOCKS5 Scheme = "socks5" // SOCKS5; host names are resolved by the proxy (remote DNS)
)

var defaultPorts = map[Scheme]int{SchemeHTTP: 80, SchemeHTTPS: 443, SchemeSOCKS5: 1080}

func parseScheme(s string) (Scheme, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "http":
		return SchemeHTTP, true
	case "https":
		return SchemeHTTPS, true
	case "socks5", "socks5h":
		return SchemeSOCKS5, true
	}
	return "", false
}

// Proxy is an upstream proxy without its password (the password lives in the credentials vault, KIN-26).
type Proxy struct {
	Scheme   Scheme
	Host     string // lower-case host name or IP literal (IPv6 without brackets)
	Port     int
	Username string
}

// Limits of proxy fields.
const (
	MaxUsernameBytes = 255
	MaxPasswordBytes = 1024
	maxProxyURLBytes = 2048
)

// URL is the normalized proxy URL: scheme://[user@]host:port, the username percent-encoded, never the password.
func (p Proxy) URL() string {
	u := url.URL{Scheme: string(p.Scheme), Host: net.JoinHostPort(p.Host, strconv.Itoa(p.Port))}
	if p.Username != "" {
		u.User = url.User(p.Username)
	}
	return u.String()
}

var hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// normalizeHost validates a host name (ASCII labels) or an IP literal without a zone; it returns it lower-cased.
func normalizeHost(h string) (string, bool) {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "" || len(h) > 253 {
		return "", false
	}
	if a, err := netip.ParseAddr(h); err == nil {
		if a.Zone() != "" {
			return "", false
		}
		return a.Unmap().String(), true
	}
	if strings.ContainsAny(h, ":%") {
		return "", false
	}
	for _, l := range strings.Split(h, ".") {
		if !hostLabelRE.MatchString(l) {
			return "", false
		}
	}
	return h, true
}

func validUsername(u string) bool {
	return len(u) <= MaxUsernameBytes && utf8.ValidString(u) && !strings.ContainsFunc(u, unicode.IsControl)
}

// NewProxy builds a proxy from separate fields (type/host/port/username).
func NewProxy(scheme, host string, port int, username string) (Proxy, error) {
	sc, ok := parseScheme(scheme)
	if !ok {
		return Proxy{}, invalid("proxy", "proxy_scheme")
	}
	h, ok := normalizeHost(host)
	if !ok {
		return Proxy{}, invalid("proxy", "proxy_host")
	}
	if port < 1 || port > 65535 {
		return Proxy{}, invalid("proxy", "proxy_port")
	}
	if !validUsername(username) {
		return Proxy{}, invalid("proxy", "proxy_username")
	}
	return Proxy{Scheme: sc, Host: h, Port: port, Username: username}, nil
}

// ParseProxyURL parses http://, https://, socks5:// (and socks5h://) proxy URLs with optional percent-encoded
// credentials and IPv6 literals; a missing port takes the scheme's default. It returns the proxy without the
// password and the password separately (zero when absent). Errors carry only a code, never the input.
func ParseProxyURL(s string) (Proxy, Password, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Proxy{}, Password{}, invalid("proxy", "required")
	}
	if len(s) > maxProxyURLBytes || strings.ContainsFunc(s, unicode.IsControl) {
		return Proxy{}, Password{}, invalid("proxy", "proxy_format")
	}
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return Proxy{}, Password{}, invalid("proxy", "proxy_scheme")
	}
	sc, ok := parseScheme(scheme)
	if !ok {
		return Proxy{}, Password{}, invalid("proxy", "proxy_scheme")
	}
	u, err := url.Parse(string(sc) + "://" + rest)
	if err != nil {
		// url.Parse errors quote the input (and with it the password): map them to a code.
		if strings.Contains(err.Error(), "invalid port") {
			return Proxy{}, Password{}, invalid("proxy", "proxy_port")
		}
		return Proxy{}, Password{}, invalid("proxy", "proxy_format")
	}
	if u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Proxy{}, Password{}, invalid("proxy", "proxy_format")
	}
	port := defaultPorts[sc]
	if ps := u.Port(); ps != "" {
		n, err := strconv.Atoi(ps)
		if err != nil || n < 1 || n > 65535 {
			return Proxy{}, Password{}, invalid("proxy", "proxy_port")
		}
		port = n
	}
	var user string
	var pass Password
	if u.User != nil {
		user = u.User.Username()
		if p, set := u.User.Password(); set && p != "" {
			if pass, err = NewPassword(p); err != nil {
				return Proxy{}, Password{}, err
			}
		}
	}
	p, err := NewProxy(string(sc), u.Hostname(), port, user)
	if err != nil {
		return Proxy{}, Password{}, err
	}
	return p, pass, nil
}

const redacted = "[redacted]"

// Password is a proxy password in memory. Every way of printing it (fmt verbs, slog, JSON) yields "[redacted]";
// only Reveal returns the value, for the vault and the wire to the runner.
type Password struct{ v string }

// NewPassword validates a password: non-empty, at most MaxPasswordBytes, no control characters.
func NewPassword(s string) (Password, error) {
	if s == "" {
		return Password{}, invalid("password", "required")
	}
	if len(s) > MaxPasswordBytes || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return Password{}, invalid("password", "password_format")
	}
	return Password{v: s}, nil
}

// Reveal returns the plaintext.
func (p Password) Reveal() string { return p.v }

// IsZero reports an absent password.
func (p Password) IsZero() bool { return p.v == "" }

func (Password) String() string               { return redacted }
func (Password) GoString() string             { return redacted }
func (Password) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (Password) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Password) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Password) MarshalText() ([]byte, error) { return []byte(redacted), nil }
