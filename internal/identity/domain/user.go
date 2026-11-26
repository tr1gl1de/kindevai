// Package domain holds the identity bounded context model: users, credentials rules and the first-run setup.
// It is pure Go: no I/O, no database, no HTTP.
package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ValidationError reports invalid user input for one field. Code is a stable, translatable identifier.
type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Code) }

// Login is a normalised (lower-case) account name.
type Login string

var loginRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

// NewLogin trims, lower-cases and validates a login: 3–64 of a-z 0-9 . _ - starting with a letter or digit.
func NewLogin(s string) (Login, error) {
	l := strings.ToLower(strings.TrimSpace(s))
	if !loginRe.MatchString(l) {
		return "", &ValidationError{Field: "login", Code: "login_format"}
	}
	return Login(l), nil
}

const (
	MinPasswordLen = 12
	MaxPasswordLen = 256
)

// ValidatePassword enforces length bounds (in characters) and forbids using the login as the password.
func ValidatePassword(pw string, login Login) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < MinPasswordLen:
		return &ValidationError{Field: "password", Code: "password_too_short"}
	case n > MaxPasswordLen:
		return &ValidationError{Field: "password", Code: "password_too_long"}
	case strings.EqualFold(pw, string(login)):
		return &ValidationError{Field: "password", Code: "password_equals_login"}
	}
	return nil
}

// PlatformName is the display name of the installation.
type PlatformName string

// NewPlatformName trims and validates a name of 1–80 printable characters.
func NewPlatformName(s string) (PlatformName, error) {
	n := strings.TrimSpace(s)
	if n == "" || utf8.RuneCountInString(n) > 80 || strings.IndexFunc(n, unicode.IsControl) >= 0 {
		return "", &ValidationError{Field: "platformName", Code: "platform_name_format"}
	}
	return PlatformName(n), nil
}

// PlatformRole is a user's installation-wide role. Project roles live in the projects context.
type PlatformRole string

const (
	RoleOwner PlatformRole = "owner"
	RoleUser  PlatformRole = "user"
)

// User is an account. PasswordHash is an opaque encoded hash (never the password).
type User struct {
	ID           string
	Login        Login
	PasswordHash string
	Role         PlatformRole
	CreatedAt    time.Time
	Disabled     bool
	// MustChangePassword is set for temporary passwords (new or reset accounts) until the user picks their own.
	MustChangePassword bool
}

// ErrNotFound is returned by repositories when an entity does not exist.
var ErrNotFound = errors.New("not found")
