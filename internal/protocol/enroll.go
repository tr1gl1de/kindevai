// Package protocol holds the wire messages shared by the Control Plane and the runner (KB KIN-A-17, KIN-A-18).
// It contains only data shapes and constants; neither side's domain imports it, adapters translate.
package protocol

// EnrollPath is the public, rate-limited endpoint a runner calls once with its one-time enrollment token.
const EnrollPath = "/api/runners/enroll"

// EnrollRequest is the body of POST EnrollPath. The token travels only in the body, never in a URL.
type EnrollRequest struct {
	Token     string `json:"token"`
	PublicKey string `json:"publicKey"` // standard base64 of the raw 32-byte Ed25519 public key
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Version   string `json:"version"`
}

// EnrollResult is the runner's registration: 201 when created, 200 for a retry of the same enrollment.
type EnrollResult struct {
	RunnerID    string `json:"runnerId"`
	Status      string `json:"status"`
	Fingerprint string `json:"fingerprint"`
}

// Error is the JSON error body of every Control Plane API response with a 4xx/5xx status.
type Error struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}

// Error codes of POST EnrollPath.
const (
	ErrCodeTokenInvalid = "token_invalid" // 401
	ErrCodeTokenUsed    = "token_used"    // 410
	ErrCodeTokenExpired = "token_expired" // 410
	ErrCodeTokenRevoked = "token_revoked" // 410
	ErrCodeKeyEnrolled  = "key_enrolled"  // 409
	ErrCodeValidation   = "validation"    // 422, with Field
	ErrCodeRateLimited  = "rate_limited"  // 429
)
