// Package redact recognises credentials by field name or by shape so they can be kept out of stored records
// (audit events, inbox payloads) and API responses. It is a heuristic safety net: callers still must not pass
// secrets on purpose.
package redact

import (
	"math"
	"net/url"
	"regexp"
	"strings"
)

// Placeholder replaces a redacted value.
const Placeholder = "[redacted]"

// SecretKey reports a field name that names a credential (password, token, secret, API/private key,
// authorization or cookie header), ignoring case and separators.
func SecretKey(key string) bool {
	k := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(key))
	switch k {
	case "authorization", "proxyauthorization", "cookie", "setcookie", "passwd", "credentials", "credential",
		"jwt", "bearer", "dsn", "session", "sessionid", "otp", "pin":
		return true
	}
	for _, s := range []string{"password", "passphrase", "secret", "token", "apikey", "privatekey"} {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

var (
	jwtRe      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	telegramRe = regexp.MustCompile(`^[0-9]{6,12}:[A-Za-z0-9_-]{30,}$`)
	awsKeyRe   = regexp.MustCompile(`^(AKIA|ASIA)[A-Z0-9]{16}$`)
	// Well-known token prefixes: GitHub, GitLab, Slack, OpenAI/Anthropic-style API keys, YouTrack permanent tokens.
	tokenPrefixes = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "xoxb-", "xoxp-",
		"xoxa-", "xoxr-", "xoxs-", "sk-", "perm:", "perm-"}
)

// minRandomLen is the shortest value judged by its randomness alone.
const minRandomLen = 24

// SecretValue reports a value shaped like a credential: an Authorization header value, a JWT, a well-known API
// token, a Telegram bot token, a PEM private key, a URL with an embedded password, or a long random-looking
// string mixing lower case, upper case and digits (ids, hex hashes and commit SHAs are single-case and pass).
func SecretValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "bearer ") {
		return true
	}
	// "Basic <base64>" / "token <value>" headers; prose such as "token expired" or "Token Service" passes.
	for _, scheme := range []string{"basic ", "token "} {
		if len(v) > len(scheme) && strings.EqualFold(v[:len(scheme)], scheme) && credentialWord(strings.TrimSpace(v[len(scheme):])) {
			return true
		}
	}
	if strings.Contains(v, "PRIVATE KEY-----") || jwtRe.MatchString(v) {
		return true
	}
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(lower, p) && len(v) >= len(p)+8 {
			return true
		}
	}
	if telegramRe.MatchString(v) || awsKeyRe.MatchString(v) {
		return true
	}
	if strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil && u.User != nil {
			if _, ok := u.User.Password(); ok {
				return true
			}
		}
	}
	return randomLooking(v)
}

func randomLooking(v string) bool {
	if len(v) < minRandomLen {
		return false
	}
	var lower, upper, digit bool
	counts := map[rune]int{}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		case strings.ContainsRune("+/=_-.~", r):
		default:
			return false // spaces, punctuation: prose or a path, not a token
		}
		counts[r]++
	}
	if !lower || !upper || !digit || wordSeparated(v) {
		return false
	}
	var h float64
	n := float64(len(v))
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h >= 4.0
}

// credentialWord reports a single word long enough to be a credential after an auth scheme.
func credentialWord(w string) bool {
	if w == "" || strings.ContainsAny(w, " \t\n") {
		return false
	}
	if len(w) >= minCredentialLen || SecretValue(w) {
		return true
	}
	// Short base64 ("dXNlcjpwYXNz") has upper case inside the word or digits/padding; words do not.
	return len(w) >= 8 && strings.ContainsFunc(w[1:], func(r rune) bool {
		return (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '=' || r == '+'
	})
}

// minCredentialLen is the shortest value after "Basic"/"token" that is treated as a credential.
const minCredentialLen = 16

// wordSeparated reports a slug or path made of plain words and numbers ("Feature-Add-Login-2024",
// "feature/KIN-16-audit"): every part between separators is all letters or all digits.
func wordSeparated(v string) bool {
	parts := strings.FieldsFunc(v, func(r rune) bool { return strings.ContainsRune("-_/.", r) })
	if len(parts) < 3 {
		return false
	}
	for _, p := range parts {
		letters := strings.IndexFunc(p, func(r rune) bool { return r < 'A' || (r > 'Z' && r < 'a') || r > 'z' }) < 0
		digits := strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }) < 0
		if !letters && !digits {
			return false
		}
	}
	return true
}

// Text redacts credentials inside free text word by word: secret-shaped words, values of key=value or key: value
// pairs whose key names a secret, and the word after "Bearer"/"Basic".
func Text(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r > ' ' }) {
		return s
	}
	words := strings.Fields(s)
	changed := false
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch lw := strings.ToLower(w); {
		case lw == "bearer" || lw == "basic" || lw == "token":
			if i+1 < len(words) && (lw == "bearer" || credentialWord(words[i+1])) {
				words[i+1] = Placeholder
				i++
				changed = true
			}
		case SecretValue(w):
			words[i] = Placeholder
			changed = true
		default:
			if k, _, ok := cutPair(w); ok && SecretKey(k) {
				words[i] = w[:len(k)+1] + Placeholder
				changed = true
			}
		}
	}
	if !changed {
		return s
	}
	return strings.Join(words, " ")
}

func cutPair(w string) (key, value string, ok bool) {
	i := strings.IndexAny(w, "=:")
	if i <= 0 || i == len(w)-1 {
		return "", "", false
	}
	return w[:i], w[i+1:], true
}

// Fields returns a copy of m with secret-named fields and secret-shaped values redacted.
func Fields(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch {
		case SecretKey(k), SecretValue(v):
			out[k] = Placeholder
		default:
			out[k] = Text(v)
		}
	}
	return out
}
