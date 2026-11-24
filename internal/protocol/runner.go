package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode"
)

// Runner connection (KIN-19, KB KIN-A-17/KIN-A-18). The runner dials ConnectPath over wss:// (ws:// only on
// loopback) offering Subprotocol; the Control Plane answers with a challenge, the runner signs the canonical
// transcript with its Ed25519 key, and the connection is authorized only for an approved runner.
const (
	// ConnectPath is the WebSocket endpoint. It is outside /api/: runners are machines, not session users.
	ConnectPath = "/runner/connect"
	// Subprotocol negotiates the protocol's major version during the HTTP upgrade. A runner that does not offer
	// it gets HTTP 426 before any WebSocket frame.
	Subprotocol = "kindevai.runner.v1"
	// ProtocolVersion is carried in the challenge and in runner.authenticate.
	ProtocolVersion = 1
	// MaxMessageBytes bounds every message in both directions; a larger one closes the connection (1009).
	MaxMessageBytes = 64 << 10
	// AuthContext domain-separates the signed transcript: product, operation (runner-auth) and transcript version.
	AuthContext = "kindevai-runner-auth/1"
)

// Message types. Every frame is one JSON Envelope in a text message; KIN-20/KIN-22 add their types (heartbeat,
// capabilities, commands with seq/ACK) to the same envelope. An unknown type closes the connection with
// CloseProtocolError.
const (
	TypeChallenge    = "runner.challenge"    // server → runner, first frame
	TypeAuthenticate = "runner.authenticate" // runner → server, answer to the challenge
	TypeAuthorized   = "runner.authorized"   // server → runner, the session is open
)

// Envelope is the frame of every message. ID is optional and reserved for request/ACK correlation (KIN-22).
type Envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Challenge is the payload of runner.challenge.
type Challenge struct {
	Version      int    `json:"version"`
	Nonce        string `json:"nonce"`     // 32 random bytes, unpadded base64url; single use
	ExpiresAt    int64  `json:"expiresAt"` // unix seconds
	Audience     string `json:"audience"`  // the Control Plane origin the runner dialed, see Audience
	ConnectionID string `json:"connectionId"`
}

// Authenticate is the payload of runner.authenticate.
type Authenticate struct {
	Version     int    `json:"version"`
	RunnerID    string `json:"runnerId"`
	Fingerprint string `json:"fingerprint,omitempty"` // public key reference (OpenSSH SHA256 fingerprint)
	Signature   string `json:"signature"`             // standard base64 Ed25519 signature over AuthTranscript
}

// Authorized is the payload of runner.authorized.
type Authorized struct {
	RunnerID     string   `json:"runnerId"`
	SessionID    string   `json:"sessionId"`
	Capabilities []string `json:"capabilities"` // granted capabilities; empty until KIN-20/KIN-22
}

// WebSocket close codes (4000–4999 are application codes). The runner stops for good on CloseRevoked and
// CloseUnsupportedVersion; every other close is retried with backoff.
const (
	CloseProtocolError      = 4400 // invalid frame, unknown message type, message out of order
	CloseAuthFailed         = 4401 // bad signature, unknown runner, expired or reused challenge
	CloseNotApproved        = 4403 // pending or disabled runner; retry later
	CloseAuthTimeout        = 4408 // no runner.authenticate in time
	CloseReplaced           = 4409 // a newer connection of the same runner took over
	CloseRevoked            = 4410 // the runner was revoked; never reconnect
	CloseUnsupportedVersion = 4426 // protocol version not supported
)

// HTTP error codes of ConnectPath before the upgrade (JSON Error bodies).
const (
	ErrCodePlainRemote       = "plain_ws_remote"      // 403: ws:// from a non-loopback address; use wss://
	ErrCodeUnsupportedProto  = "unsupported_protocol" // 426: subprotocol not offered
	ErrCodeBrowserNotAllowed = "origin_not_allowed"   // 403: browsers (Origin header) may not connect
	ErrCodeBusy              = "busy"                 // 503: connection limit reached
	ErrCodeMisdirected       = "misdirected"          // 421: Host is not one of the Control Plane's public origins
)

// ErrAuthField means a transcript field is empty or could make the transcript ambiguous.
var ErrAuthField = errors.New("invalid runner-auth transcript field")

// AuthFields are the values the runner signs.
type AuthFields struct {
	RunnerID     string
	Audience     string
	Nonce        string // as sent in the challenge (unpadded base64url)
	ExpiresAt    int64
	ConnectionID string
}

// AuthTranscript is the canonical signed payload: AuthContext and the fields, one per line, in this order:
//
//	kindevai-runner-auth/1\n<runner_id>\n<audience>\n<nonce_b64>\n<expiry_unix>\n<conn_id>
//
// No field may be empty or contain control characters, so the encoding is unambiguous.
func AuthTranscript(f AuthFields) ([]byte, error) {
	for _, v := range []struct{ name, val string }{{"runnerId", f.RunnerID}, {"audience", f.Audience},
		{"nonce", f.Nonce}, {"connectionId", f.ConnectionID}} {
		if v.val == "" || strings.IndexFunc(v.val, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("%w: %s", ErrAuthField, v.name)
		}
	}
	if f.ExpiresAt <= 0 {
		return nil, fmt.Errorf("%w: expiresAt", ErrAuthField)
	}
	return []byte(strings.Join([]string{AuthContext, f.RunnerID, f.Audience, f.Nonce,
		strconv.FormatInt(f.ExpiresAt, 10), f.ConnectionID}, "\n")), nil
}

// Audience is the normalized origin of the Control Plane as the runner dialed it: scheme, lowercase host and the
// port unless it is the scheme's default. The server derives it from the request, the runner from its configured
// URL; a challenge relayed from another server carries a different audience and is refused by the runner.
func Audience(secure bool, hostport string) string {
	scheme, def := "http", "80"
	if secure {
		scheme, def = "https", "443"
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = strings.Trim(hostport, "[]"), ""
	}
	host = strings.ToLower(host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" || port == def {
		return scheme + "://" + host
	}
	return scheme + "://" + host + ":" + port
}
