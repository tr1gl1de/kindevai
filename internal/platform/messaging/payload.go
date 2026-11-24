package messaging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Size limits of stored messages. Adapters store a small projection of a webhook body (ids, refs, event type)
// and fetch the authoritative state when processing, so payloads stay far below MaxPayloadBytes.
const (
	MaxPayloadBytes  = 64 << 10
	maxEndpointBytes = 64
	maxTopicBytes    = 128
	maxKeyBytes      = 256
)

const redacted = "[redacted]"

func checkField(name, v string, maxLen int) error {
	if v == "" || len(v) > maxLen {
		return fmt.Errorf("%w: %s must be 1..%d bytes", ErrInvalidMessage, name, maxLen)
	}
	return nil
}

// secretField reports a JSON object key that names a credential (password, token, secret, API/private key,
// authorization or cookie header), ignoring case and separators.
func secretField(key string) bool {
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

func decodePayload(p []byte) (any, error) {
	if len(p) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes, limit %d", ErrInvalidMessage, len(p), MaxPayloadBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber() // keep large integers (ids) exact when re-encoding
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, fmt.Errorf("%w: payload is not a JSON value", ErrInvalidMessage)
	}
	return v, nil
}

// findSecret returns the path of the first secret-looking field in v, or "".
func findSecret(v any, path string) string {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if secretField(k) {
				return path + "." + k
			}
			if p := findSecret(child, path+"."+k); p != "" {
				return p
			}
		}
	case []any:
		for i, child := range t {
			if p := findSecret(child, fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	}
	return ""
}

// redact replaces the values of secret-looking fields in place.
func redact(v any) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if secretField(k) {
				t[k] = redacted
				continue
			}
			redact(child)
		}
	case []any:
		for _, child := range t {
			redact(child)
		}
	}
}

// outboxPayload validates an outgoing payload: secrets in it are a programming error, since they would be
// stored in SQLite and shown in the dead-letter view. Handlers read credentials from the vault when delivering.
func outboxPayload(p []byte) ([]byte, error) {
	v, err := decodePayload(p)
	if err != nil {
		return nil, err
	}
	if f := findSecret(v, "$"); f != "" {
		// The field name, never its value, goes into the error.
		return nil, fmt.Errorf("%w: payload field %s looks like a secret", ErrInvalidMessage, f)
	}
	return p, nil
}

// inboxPayload validates an incoming payload and redacts secret-looking fields: the sender controls the body,
// so it is cleaned rather than rejected (rejecting would make the sender retry forever).
func inboxPayload(p []byte) ([]byte, error) {
	v, err := decodePayload(p)
	if err != nil {
		return nil, err
	}
	if findSecret(v, "$") == "" {
		return p, nil
	}
	redact(v)
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-encoding redacted payload: %w", err)
	}
	return out, nil
}
