package domain

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// LinkRequestTTL is how long a Link Telegram request stays usable.
	LinkRequestTTL = 10 * time.Minute
	// LinkCodeAttempts wrong confirmation codes cancel the request.
	LinkCodeAttempts = 5
)

// Link request errors. An unknown token and a code from another web session are both ErrLinkInvalid.
var (
	ErrLinkInvalid         = errors.New("invalid link request")
	ErrLinkExpired         = errors.New("link request expired")
	ErrLinkUsed            = errors.New("link request already used")
	ErrLinkCancelled       = errors.New("link request cancelled")
	ErrLinkClaimed         = errors.New("link request was opened by another Telegram account")
	ErrLinkNotClaimed      = errors.New("link request has not been confirmed in Telegram yet")
	ErrLinkCodeWrong       = errors.New("wrong confirmation code")
	ErrLinkTooManyAttempts = errors.New("too many wrong confirmation codes")
)

// LinkState is where a link request is in the two-sided confirmation.
type LinkState string

const (
	LinkAwaitingTelegram LinkState = "awaiting_telegram" // link shown on the web, not opened in the Mini App yet
	LinkAwaitingCode     LinkState = "awaiting_code"     // confirmed in the Mini App; the web session must enter the code
	LinkLinked           LinkState = "linked"
	LinkCancelled        LinkState = "cancelled"
	LinkExpired          LinkState = "expired"
)

// LinkRequest is a single-use request to link a Telegram account to UserID, bound to the web session that created
// it. The Mini App side proves the Telegram user (signed initData) and receives a short code; only the creating web
// session can finish by entering that code, so a stolen link alone cannot attach a stranger's Telegram account.
type LinkRequest struct {
	ID          string
	TokenHash   []byte
	UserID      string
	SessionHash []byte // token hash of the creating web session
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Telegram    TelegramUser // zero until confirmed in the Mini App
	CodeHash    []byte
	Attempts    int
	ConsumedAt  *time.Time
	CancelledAt *time.Time
}

// NewLinkRequest creates a request for userID from the web session with token hash sessionHash; the plaintext
// token goes into the startapp link once and only its hash is stored.
func NewLinkRequest(rnd io.Reader, id, userID string, sessionHash []byte, now time.Time) (string, LinkRequest, error) {
	var b [32]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return "", LinkRequest{}, fmt.Errorf("generating link token: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b[:])
	return plain, LinkRequest{ID: id, TokenHash: HashLinkToken(plain), UserID: userID, SessionHash: sessionHash,
		CreatedAt: now, ExpiresAt: now.Add(LinkRequestTTL)}, nil
}

// HashLinkToken is the lookup key for a presented link token.
func HashLinkToken(plain string) []byte {
	h := sha256.Sum256([]byte("link:" + plain))
	return h[:]
}

func hashLinkCode(requestID, code string) []byte {
	h := sha256.Sum256([]byte("code:" + requestID + ":" + code))
	return h[:]
}

// State reports the request's state at now.
func (r LinkRequest) State(now time.Time) LinkState {
	switch {
	case r.ConsumedAt != nil:
		return LinkLinked
	case r.CancelledAt != nil:
		return LinkCancelled
	case !now.Before(r.ExpiresAt):
		return LinkExpired
	case r.Telegram.ID != 0:
		return LinkAwaitingCode
	default:
		return LinkAwaitingTelegram
	}
}

// Usable reports why the request can no longer be used, or nil.
func (r LinkRequest) Usable(now time.Time) error {
	switch r.State(now) {
	case LinkLinked:
		return ErrLinkUsed
	case LinkCancelled:
		return ErrLinkCancelled
	case LinkExpired:
		return ErrLinkExpired
	}
	return nil
}

// Claim records the Telegram user who confirmed the request in the Mini App and returns a fresh 6-digit code for
// the web session. Only the first Telegram user may claim; the same user reopening the Mini App gets a new code.
func (r *LinkRequest) Claim(rnd io.Reader, tg TelegramUser, now time.Time) (string, error) {
	if err := r.Usable(now); err != nil {
		return "", err
	}
	if r.Telegram.ID != 0 && r.Telegram.ID != tg.ID {
		return "", ErrLinkClaimed
	}
	code, err := sixDigits(rnd)
	if err != nil {
		return "", err
	}
	r.Telegram, r.CodeHash = tg, hashLinkCode(r.ID, code)
	return code, nil
}

// CheckCode verifies the code entered in the web session with token hash sessionHash. A wrong code uses up an
// attempt; the last one cancels the request. A different web session is ErrLinkInvalid and costs nothing.
func (r *LinkRequest) CheckCode(code string, sessionHash []byte, now time.Time) error {
	if err := r.Usable(now); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(r.SessionHash, sessionHash) != 1 {
		return ErrLinkInvalid
	}
	if r.Telegram.ID == 0 {
		return ErrLinkNotClaimed
	}
	if subtle.ConstantTimeCompare(r.CodeHash, hashLinkCode(r.ID, code)) == 1 {
		return nil
	}
	r.Attempts++
	if r.Attempts >= LinkCodeAttempts {
		r.Cancel(now)
		return ErrLinkTooManyAttempts
	}
	return ErrLinkCodeWrong
}

// Consume marks the request used.
func (r *LinkRequest) Consume(now time.Time) { r.ConsumedAt = &now }

// Cancel withdraws the request.
func (r *LinkRequest) Cancel(now time.Time) {
	if r.CancelledAt == nil && r.ConsumedAt == nil {
		r.CancelledAt = &now
	}
}

// sixDigits draws a uniform code in 000000–999999 (rejection sampling avoids modulo bias).
func sixDigits(rnd io.Reader) (string, error) {
	const limit = 4_294_000_000 // largest multiple of 1e6 below 2^32
	var b [4]byte
	for {
		if _, err := io.ReadFull(rnd, b[:]); err != nil {
			return "", fmt.Errorf("generating link code: %w", err)
		}
		if v := binary.BigEndian.Uint32(b[:]); v < limit {
			return fmt.Sprintf("%06d", v%1_000_000), nil
		}
	}
}
