package domain

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Telegram Mini App authentication (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app).
// The Mini App sends the raw initData string; only its HMAC, computed with the bot token, proves who the Telegram
// user is. initDataUnsafe, usernames and the startapp parameter alone are never trusted.

const (
	// InitDataMaxAge bounds how old a signed initData may be. Telegram signs it when the Mini App opens; a captured
	// string must not work as a long-lived credential.
	InitDataMaxAge = time.Hour
	// initDataClockSkew tolerates a server clock slightly behind Telegram's.
	initDataClockSkew = 5 * time.Minute
	// maxInitDataLen caps the input before any parsing or hashing.
	maxInitDataLen = 4096
)

var (
	// ErrInitDataInvalid covers malformed initData and a wrong signature alike.
	ErrInitDataInvalid = errors.New("invalid Telegram init data")
	// ErrInitDataExpired means the signature is fine but auth_date is outside the accepted window.
	ErrInitDataExpired = errors.New("telegram init data expired")
	// ErrTelegramNotConfigured means the Owner has not set up the bot yet.
	ErrTelegramNotConfigured = errors.New("telegram bot is not configured")
	// ErrTelegramNotLinked means no account is linked to the Telegram user (accounts are never created from it).
	ErrTelegramNotLinked = errors.New("telegram account is not linked")
	// ErrTelegramTaken means the Telegram user is already linked to another account.
	ErrTelegramTaken = errors.New("this Telegram account is linked to another user")
	// ErrUserHasTelegram means the account is already linked to a Telegram user.
	ErrUserHasTelegram = errors.New("account already has a linked Telegram account")
)

// TelegramUser is the signed Telegram user from initData.
type TelegramUser struct {
	ID           int64  `json:"id"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	Username     string `json:"username"`
	LanguageCode string `json:"language_code"`
}

// DisplayName is for showing the user what is being linked; it is never used to match accounts.
func (u TelegramUser) DisplayName() string {
	if u.Username != "" {
		return "@" + u.Username
	}
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	return strconv.FormatInt(u.ID, 10)
}

// InitData is validated Mini App launch data.
type InitData struct {
	User       TelegramUser
	AuthDate   time.Time
	StartParam string // signed: part of the data-check-string
}

// ValidateInitData checks the signature of raw initData with the bot token (constant-time) and that auth_date is
// within InitDataMaxAge of now. Every structural problem is ErrInitDataInvalid.
func ValidateInitData(raw, botToken string, now time.Time) (InitData, error) {
	if raw == "" || len(raw) > maxInitDataLen || botToken == "" {
		return InitData{}, ErrInitDataInvalid
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return InitData{}, ErrInitDataInvalid
	}
	hashes := vals["hash"]
	if len(hashes) != 1 {
		return InitData{}, ErrInitDataInvalid
	}
	got, err := hex.DecodeString(hashes[0])
	if err != nil || len(got) != sha256.Size {
		return InitData{}, ErrInitDataInvalid
	}
	lines := make([]string, 0, len(vals))
	for k, vs := range vals {
		if k == "hash" {
			continue
		}
		if len(vs) != 1 { // duplicate keys have no defined data-check-string
			return InitData{}, ErrInitDataInvalid
		}
		lines = append(lines, k+"="+vs[0])
	}
	sort.Strings(lines)
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	if !hmac.Equal(mac.Sum(nil), got) {
		return InitData{}, ErrInitDataInvalid
	}
	// Only signed data is interpreted below.
	sec, err := strconv.ParseInt(vals.Get("auth_date"), 10, 64)
	if err != nil || sec <= 0 {
		return InitData{}, ErrInitDataInvalid
	}
	var u TelegramUser
	if err := json.Unmarshal([]byte(vals.Get("user")), &u); err != nil || u.ID <= 0 {
		return InitData{}, ErrInitDataInvalid
	}
	auth := time.Unix(sec, 0)
	if now.Sub(auth) > InitDataMaxAge || auth.Sub(now) > initDataClockSkew {
		return InitData{}, ErrInitDataExpired
	}
	return InitData{User: u, AuthDate: auth, StartParam: vals.Get("start_param")}, nil
}

// Start parameter kinds of t.me/<bot>/<app>?startapp=<kind>_<token> links.
const (
	StartLink   = "link"
	StartInvite = "invite"
)

var startParamRe = regexp.MustCompile(`^(link|invite)_([A-Za-z0-9_-]{1,57})$`)

// ParseStartParam splits a startapp value into its kind and opaque token; unknown values give "", "".
func ParseStartParam(s string) (kind, token string) {
	m := startParamRe.FindStringSubmatch(s)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

// TelegramIdentity links one Telegram user to one account (both sides unique).
type TelegramIdentity struct {
	UserID     string
	TelegramID int64
	Username   string // display only, as signed at link time
	LinkedAt   time.Time
}

// BotSettings configure the Mini App; the token itself is kept in the encrypted vault.
type BotSettings struct {
	BotUsername  string
	AppShortName string
}

// LinkURL is the Mini App direct link carrying a start parameter.
func (s BotSettings) LinkURL(startParam string) string {
	return "https://t.me/" + s.BotUsername + "/" + s.AppShortName + "?startapp=" + url.QueryEscape(startParam)
}

var (
	botTokenRe    = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{30,64}$`)
	botUsernameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,28}[Bb][Oo][Tt]$`)
	appShortRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{2,63}$`)
)

// NewBotToken validates the shape of a BotFather token ("<bot id>:<secret>").
func NewBotToken(s string) (string, error) {
	t := strings.TrimSpace(s)
	if !botTokenRe.MatchString(t) {
		return "", &ValidationError{Field: "botToken", Code: "bot_token_format"}
	}
	return t, nil
}

// NewBotUsername validates a bot username (an optional leading @ is dropped); Telegram requires the "bot" suffix.
func NewBotUsername(s string) (string, error) {
	u := strings.TrimPrefix(strings.TrimSpace(s), "@")
	if !botUsernameRe.MatchString(u) {
		return "", &ValidationError{Field: "botUsername", Code: "bot_username_format"}
	}
	return u, nil
}

// NewAppShortName validates the Mini App short name set in BotFather (/newapp).
func NewAppShortName(s string) (string, error) {
	n := strings.TrimSpace(s)
	if !appShortRe.MatchString(n) {
		return "", &ValidationError{Field: "appShortName", Code: "app_short_name_format"}
	}
	return n, nil
}
