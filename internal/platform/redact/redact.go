// Package redact recognises credentials by field name or by shape so they can be kept out of stored records
// (audit events, inbox payloads) and API responses. It is a heuristic safety net: callers still must not pass
// secrets on purpose.
package redact

import (
	"bytes"
	"math"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Placeholder replaces a redacted value.
const Placeholder = "[redacted]"

// SecretKey reports a field name that names a credential (password, token, secret, API/private key,
// authorization or cookie header), ignoring case and separators.
func SecretKey(key string) bool {
	var buf [64]byte
	k := normalizeKey(buf[:0], key)
	switch string(k) { // a conversion in a switch does not allocate
	case "authorization", "proxyauthorization", "cookie", "setcookie", "passwd", "credentials", "credential",
		"jwt", "bearer", "dsn", "session", "sessionid", "otp", "pin":
		return true
	}
	for _, s := range []string{"password", "passphrase", "secret", "token", "apikey", "privatekey"} {
		if bytes.HasSuffix(k, []byte(s)) {
			return true
		}
	}
	return false
}

// normalizeKey appends key to dst in lower case without the separators "_", "-", "." and " ". ASCII keys are
// handled byte by byte without allocating, because scrub calls SecretKey for every pair in a large text.
func normalizeKey(dst []byte, key string) []byte {
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= utf8.RuneSelf:
			return append(dst[:0], keySeparators.Replace(strings.ToLower(key))...)
		case c == '_' || c == '-' || c == '.' || c == ' ':
		case c >= 'A' && c <= 'Z':
			dst = append(dst, c+'a'-'A')
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")

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
	return knownSecretValue(v) || randomLooking(strings.TrimSpace(v))
}

// knownSecretValue is SecretValue without the randomness guess: only shapes that are credentials by construction.
func knownSecretValue(v string) bool {
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
		if len(v) > len(scheme) && strings.EqualFold(v[:len(scheme)], scheme) && credentialWord(strings.TrimSpace(v[len(scheme):]), false) {
			return true
		}
	}
	if strings.Contains(v, "PRIVATE KEY-----") || jwtRe.MatchString(v) {
		return true
	}
	for _, p := range tokenPrefixes {
		// Generated tokens carry digits or mixed case after the prefix; "sk-learn-pipeline" or "perm-admin-role" do not.
		if strings.HasPrefix(lower, p) && len(v) >= len(p)+8 && strings.ContainsFunc(v[len(p):], func(r rune) bool {
			return (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		}) {
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
	return false
}

func randomLooking(v string) bool {
	if len(v) < minRandomLen {
		return false
	}
	var lower, upper, digit bool
	var counts [128]int // every accepted rune is ASCII; an array keeps this allocation-free (KIN-77: output)
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
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h >= 4.0
}

// credentialWord reports a single word long enough to be a credential after an auth scheme. In strict mode
// length alone is not enough ("token authentication-failed" is prose): the word must look generated.
func credentialWord(w string, strict bool) bool {
	if w == "" || strings.ContainsAny(w, " \t\n") {
		return false
	}
	if strict {
		if knownSecretValue(w) {
			return true
		}
	} else if len(w) >= minCredentialLen || SecretValue(w) {
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

// Text redacts credentials inside free text word by word: secret-shaped words (including long random-looking
// ones), values of key=value, key: value, URL query and quoted JSON pairs whose key names a secret, and the word
// after "Bearer"/"Basic". Use it for error texts and logs, where masking an id by mistake is harmless.
func Text(s string) string { return scrub(s, SecretValue, false) }

// Credentials is Text limited to unambiguous credential shapes: auth headers, well-known token prefixes, JWTs,
// PEM keys, URLs with a password and secret-named parameters. It does not guess by randomness, so opaque ids
// (GitHub node ids, base64 cursors) in webhook payloads survive.
func Credentials(s string) string { return scrub(s, knownSecretValue, true) }

var (
	// paramRe matches key=value in query strings and free text; the value stops at a separator or a quote.
	paramRe = regexp.MustCompile(`([A-Za-z0-9_.-]+)=([^\s&;#"',)]+)`)
	// pemRe matches a PEM private key block, possibly cut off by truncation.
	pemRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----(?s:.*?)(?:-----END [A-Z ]*PRIVATE KEY-----|\z)`)
	// jsonPairRe matches a quoted JSON string pair, as found in upstream error bodies.
	jsonPairRe = regexp.MustCompile(`"([A-Za-z0-9_.-]+)"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

// maskPairs replaces PEM private keys and the values of secret-named key=value and "key":"value" pairs.
func maskPairs(s string) string {
	if strings.Contains(s, "PRIVATE KEY-----") {
		s = pemRe.ReplaceAllString(s, Placeholder)
	}
	// The regexps only run on text that can hold their pairs: they dominate the cost on process output (KIN-77).
	if strings.Contains(s, "=") {
		s = paramRe.ReplaceAllStringFunc(s, func(m string) string {
			k, _, _ := strings.Cut(m, "=")
			if SecretKey(k) {
				return k + "=" + Placeholder
			}
			return m
		})
	}
	if !strings.Contains(s, ":") || strings.Count(s, `"`) < 4 {
		return s
	}
	return jsonPairRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := jsonPairRe.FindStringSubmatch(m)
		if SecretKey(sub[1]) {
			return `"` + sub[1] + `":"` + Placeholder + `"`
		}
		return m
	})
}

// scrub masks secrets in s. strict (Credentials) needs a generated-looking word after an auth scheme and does not
// treat "key: value" with a space as a pair, so prose in payloads ("Reset password: steps below") passes.
func scrub(s string, secret func(string) bool, strict bool) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r > ' ' }) {
		return s
	}
	masked := maskPairs(s)
	words := strings.Fields(masked)
	changed := masked != s
	// maskNext masks the word after i (and the one after it when that word is an auth scheme).
	var maskNext func(i int, needCredential bool) int
	maskNext = func(i int, needCredential bool) int {
		if i+1 >= len(words) || strings.HasPrefix(words[i+1], Placeholder) {
			return i
		}
		if isScheme(words[i+1]) {
			return maskNext(i+1, needCredential)
		}
		if needCredential && !credentialWord(words[i+1], strict) {
			return i
		}
		words[i+1] = Placeholder
		changed = true
		return i + 1
	}
	for i := 0; i < len(words); i++ {
		w := words[i]
		core := strings.TrimRight(strings.TrimLeft(w, "\"'(<[{"), "\"')>]},;.")
		switch {
		case isScheme(w):
			i = maskNext(i, strict || !strings.EqualFold(w, "bearer"))
		case core != "" && secret(core):
			words[i] = strings.Replace(w, core, Placeholder, 1)
			changed = true
		default:
			k, v, ok := cutPair(w)
			key := strings.Trim(k, "\"'{")
			switch {
			case ok && SecretKey(key) && isScheme(v): // Authorization:Bearer <token>
				words[i] = w[:len(k)+1] + Placeholder
				changed = true
				i = maskNext(i, strict)
			case ok && SecretKey(key) && !strings.HasPrefix(strings.TrimLeft(v, "\""), Placeholder):
				words[i] = w[:len(k)+1] + Placeholder
				changed = true
			case !strict && len(w) > 1 && strings.ContainsAny(w[len(w)-1:], ":=") &&
				SecretKey(strings.Trim(w[:len(w)-1], "\"'{")): // "password: hunter2", "password": 12345
				i = maskNext(i, false)
			}
		}
	}
	if !changed {
		return s
	}
	return strings.Join(words, " ")
}

func isScheme(w string) bool {
	return strings.EqualFold(w, "bearer") || strings.EqualFold(w, "basic") || strings.EqualFold(w, "token")
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
