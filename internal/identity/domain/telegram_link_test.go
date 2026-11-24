package domain_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func newLink(t *testing.T) (string, domain.LinkRequest) {
	t.Helper()
	plain, r, err := domain.NewLinkRequest(rand.Reader, "req1", "u1", []byte("web-session"), t0)
	if err != nil {
		t.Fatal(err)
	}
	return plain, r
}

func TestNewLinkRequestIsHashedAndShortLived(t *testing.T) {
	t.Parallel()
	plain, r := newLink(t)
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(plain) {
		t.Fatalf("token %q is not 256-bit base64url", plain)
	}
	if bytes.Contains(r.TokenHash, []byte(plain)) || !bytes.Equal(r.TokenHash, domain.HashLinkToken(plain)) {
		t.Fatal("only the token hash is kept")
	}
	if !r.ExpiresAt.Equal(t0.Add(domain.LinkRequestTTL)) || r.State(t0) != domain.LinkAwaitingTelegram {
		t.Fatalf("expires %v state %v", r.ExpiresAt, r.State(t0))
	}
	if len("link_"+plain) > 64 {
		t.Fatal("startapp parameter is limited to 64 characters")
	}
	other, _ := newLink(t)
	if other == plain {
		t.Fatal("tokens must be random")
	}
}

func TestLinkRequestClaimAndCode(t *testing.T) {
	t.Parallel()
	_, r := newLink(t)
	ann := domain.TelegramUser{ID: 42, Username: "ann"}
	code, err := r.Claim(rand.Reader, ann, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(code) || bytes.Contains(r.CodeHash, []byte(code)) {
		t.Fatalf("code %q must be 6 digits and stored hashed", code)
	}
	if r.State(t0.Add(time.Minute)) != domain.LinkAwaitingCode || r.Telegram.ID != 42 {
		t.Fatalf("state %v telegram %+v", r.State(t0), r.Telegram)
	}
	// The same Telegram user may reopen the Mini App: a fresh code replaces the old one.
	code2, err := r.Claim(rand.Reader, ann, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CheckCode(code2, []byte("web-session"), t0.Add(3*time.Minute)); err != nil {
		t.Fatalf("fresh code: %v", err)
	}
	if code != code2 {
		if err := r.CheckCode(code, []byte("web-session"), t0.Add(3*time.Minute)); !errors.Is(err, domain.ErrLinkCodeWrong) {
			t.Fatalf("replaced code: %v", err)
		}
	}
}

func TestLinkRequestStolenLinkIsClaimedOnce(t *testing.T) {
	t.Parallel()
	_, r := newLink(t)
	if _, err := r.Claim(rand.Reader, domain.TelegramUser{ID: 42}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Claim(rand.Reader, domain.TelegramUser{ID: 666}, t0); !errors.Is(err, domain.ErrLinkClaimed) {
		t.Fatalf("second Telegram user: %v, want ErrLinkClaimed", err)
	}
	if r.Telegram.ID != 42 {
		t.Fatal("the first claim stays")
	}
}

func TestLinkRequestCodeOnlyFromTheCreatingSession(t *testing.T) {
	t.Parallel()
	_, r := newLink(t)
	if err := r.CheckCode("123456", []byte("web-session"), t0); !errors.Is(err, domain.ErrLinkNotClaimed) {
		t.Fatalf("before claim: %v", err)
	}
	code, _ := r.Claim(rand.Reader, domain.TelegramUser{ID: 42}, t0)
	if err := r.CheckCode(code, []byte("other-session"), t0); !errors.Is(err, domain.ErrLinkInvalid) {
		t.Fatalf("other web session: %v, want ErrLinkInvalid", err)
	}
	if r.Attempts != 0 {
		t.Fatal("a foreign session does not burn attempts")
	}
}

func TestLinkRequestWrongCodesCancelIt(t *testing.T) {
	t.Parallel()
	_, r := newLink(t)
	code, _ := r.Claim(rand.Reader, domain.TelegramUser{ID: 42}, t0)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for i := 1; i < domain.LinkCodeAttempts; i++ {
		if err := r.CheckCode(wrong, []byte("web-session"), t0); !errors.Is(err, domain.ErrLinkCodeWrong) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := r.CheckCode(wrong, []byte("web-session"), t0); !errors.Is(err, domain.ErrLinkTooManyAttempts) {
		t.Fatalf("last attempt: %v", err)
	}
	if r.State(t0) != domain.LinkCancelled {
		t.Fatalf("state %v, want cancelled", r.State(t0))
	}
	if err := r.CheckCode(code, []byte("web-session"), t0); !errors.Is(err, domain.ErrLinkCancelled) {
		t.Fatalf("right code after cancel: %v", err)
	}
}

func TestLinkRequestExpiryAndReuse(t *testing.T) {
	t.Parallel()
	_, r := newLink(t)
	late := t0.Add(domain.LinkRequestTTL)
	if _, err := r.Claim(rand.Reader, domain.TelegramUser{ID: 42}, late); !errors.Is(err, domain.ErrLinkExpired) {
		t.Fatalf("claim after TTL: %v", err)
	}
	code, _ := r.Claim(rand.Reader, domain.TelegramUser{ID: 42}, t0)
	if err := r.CheckCode(code, []byte("web-session"), late); !errors.Is(err, domain.ErrLinkExpired) {
		t.Fatalf("code after TTL: %v", err)
	}
	if err := r.CheckCode(code, []byte("web-session"), t0); err != nil {
		t.Fatal(err)
	}
	r.Consume(t0)
	if r.State(t0) != domain.LinkLinked {
		t.Fatalf("state %v", r.State(t0))
	}
	if _, err := r.Claim(rand.Reader, domain.TelegramUser{ID: 42}, t0); !errors.Is(err, domain.ErrLinkUsed) {
		t.Fatalf("replayed link: %v, want ErrLinkUsed", err)
	}
	if err := r.CheckCode(code, []byte("web-session"), t0); !errors.Is(err, domain.ErrLinkUsed) {
		t.Fatalf("replayed code: %v, want ErrLinkUsed", err)
	}
}

func TestParseStartParam(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, kind, token string }{
		{"link_" + strings.Repeat("a", 43), "link", strings.Repeat("a", 43)},
		{"invite_abc-_9", "invite", "abc-_9"},
		{"", "", ""},
		{"link_", "", ""},
		{"other_x", "", ""},
		{"link_a b", "", ""},
	} {
		kind, token := domain.ParseStartParam(c.in)
		if kind != c.kind || token != c.token {
			t.Errorf("ParseStartParam(%q) = %q, %q", c.in, kind, token)
		}
	}
}
