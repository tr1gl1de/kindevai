package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/password"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
)

// fakeBotToken has a bot token's shape but belongs to no bot; nothing here talks to Telegram.
const fakeBotToken = "123456789:TEST_token-for-unit-tests_only_000"

// memVault stands in for the encrypted integrations vault.
type memVault struct {
	mu    sync.Mutex
	token string
}

func (v *memVault) BotToken(context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token == "" {
		return "", domain.ErrNotFound
	}
	return v.token, nil
}

func (v *memVault) SetBotToken(_ context.Context, t, _ string) error {
	v.mu.Lock()
	v.token = t
	v.mu.Unlock()
	return nil
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
func newClock() *clock               { return &clock{now: time.Now().Truncate(time.Second)} }
func (c *clock) unix() string        { return strconv.FormatInt(c.Now().Unix(), 10) }
func tgUserJSON(id int64, name string) string {
	return `{"id":` + strconv.FormatInt(id, 10) + `,"first_name":"` + name + `","username":"` + strings.ToLower(name) + `","language_code":"ru"}`
}

type tgEnv struct {
	h     http.Handler
	db    *sql.DB
	clock *clock
	vault *memVault
	logs  *bytes.Buffer
}

// initData signs Mini App launch data like Telegram does (HMAC-SHA256 with key HMAC("WebAppData", bot token)).
func (e *tgEnv) initData(t *testing.T, tgID int64, name, startParam string) string {
	t.Helper()
	fields := map[string]string{"auth_date": e.clock.unix(), "user": tgUserJSON(tgID, name), "query_id": "AAQ"}
	if startParam != "" {
		fields["start_param"] = startParam
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + fields[k]
	}
	sk := hmac.New(sha256.New, []byte("WebAppData"))
	sk.Write([]byte(fakeBotToken))
	m := hmac.New(sha256.New, sk.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	v := url.Values{}
	for k, val := range fields {
		v.Set(k, val)
	}
	v.Set("hash", hex.EncodeToString(m.Sum(nil)))
	return v.Encode()
}

func telegramStack(t *testing.T) *tgEnv {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), "INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', '2026-11-24T00:00:00.000000000Z')"); err != nil {
		t.Fatal(err)
	}
	hasher := password.New(password.Params{MemoryKiB: 64, Iterations: 1, Parallelism: 1, MaxConcurrent: 4})
	users := sqlite.NewUserRepository(d)
	sessions := sqlite.NewSessionRepository(d)
	hash, _ := hasher.Hash(t.Context(), "correct horse battery")
	for _, u := range []domain.User{
		{ID: "u-owner", Login: "owner", PasswordHash: hash, Role: domain.RoleOwner, CreatedAt: time.Now()},
		{ID: "u-alice", Login: "alice", PasswordHash: hash, Role: domain.RoleUser, CreatedAt: time.Now()},
		{ID: "u-bob", Login: "bob", PasswordHash: hash, Role: domain.RoleUser, CreatedAt: time.Now()},
	} {
		if err := users.Create(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	}
	e := &tgEnv{db: d, clock: newClock(), vault: &memVault{}, logs: &bytes.Buffer{}}
	logger := slog.New(slog.NewTextHandler(e.logs, nil))
	audit := &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}
	tx := db.Transactor{DB: d}
	lim := func() *ratelimit.Limiter {
		return ratelimit.New(ratelimit.Config{Burst: 1000, Every: time.Minute, MaxKeys: 100}, time.Now)
	}
	authn := &AuthHandler{
		Service: &app.AuthService{Users: users, Sessions: sessions, Passwords: hasher, Now: e.clock.Now, Rand: rand.Reader,
			Tx: tx, Audit: audit},
		IPLimiter: lim(), LoginLimiter: lim(), Logger: logger,
	}
	tg := &app.TelegramService{Repo: sqlite.NewTelegramRepository(d), Vault: e.vault, Users: users, Sessions: sessions,
		Tx: tx, Audit: audit, Now: e.clock.Now, Rand: rand.Reader, NewID: rand.Text, Logger: logger}
	invitations := &app.InvitationsService{Invitations: sqlite.NewInvitationRepository(d), Accounts: users,
		Projects: sqlProjects{d}, Tx: tx, Audit: audit, Hasher: hasher, Rand: rand.Reader, Now: e.clock.Now,
		NewID: rand.Text, Logger: logger, Telegram: tg}
	accounts := &app.AccountsService{Accounts: users, Sessions: sessions, Hasher: hasher, Rand: rand.Reader, Now: time.Now,
		NewID: rand.Text, Tx: tx, Audit: audit}
	mux := http.NewServeMux()
	authn.Register(mux)
	(&TelegramHandler{Service: tg, Limiter: lim(), Logger: logger}).Register(mux)
	(&InvitationsHandler{Service: invitations, Limiter: lim(), Logger: logger, Telegram: tg}).Register(mux)
	(&AccountsHandler{Service: accounts, Limiter: lim(), Logger: logger}).Register(mux)
	e.h = authn.Middleware(mux)
	return e
}

func (e *tgEnv) configure(t *testing.T) {
	t.Helper()
	owner, _ := login(t, e.h, "owner", "correct horse battery")
	rec := doCall(t, e.h, call{method: "PUT", target: "/api/telegram/settings", cookie: owner,
		body: `{"botUsername":"@kindev_bot","appShortName":"app","botToken":"` + fakeBotToken + `"}`})
	if rec.Code != 200 {
		t.Fatalf("configure = %d %s", rec.Code, rec.Body)
	}
}

func (e *tgEnv) post(t *testing.T, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doCall(t, e.h, call{method: "POST", target: target, body: body})
}

func initBody(initData string) string {
	b, _ := json.Marshal(map[string]string{"initData": initData})
	return string(b)
}

func tgCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == TelegramSessionCookie {
			return c
		}
	}
	return nil
}

// meWithTelegram calls /api/auth/me with only the Mini App session cookie.
func (e *tgEnv) meWithTelegram(t *testing.T, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doCall(t, e.h, call{method: "GET", target: "/api/auth/me", headers: map[string]string{"Cookie": TelegramSessionCookie + "=" + token}})
}

// startLink creates a link request from the given web session and returns its startapp parameter.
func (e *tgEnv) startLink(t *testing.T, web string) string {
	t.Helper()
	rec := doCall(t, e.h, call{method: "POST", target: "/api/telegram/link", cookie: web})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start link = %d %s", rec.Code, rec.Body)
	}
	u, _ := decode(t, rec)["url"].(string)
	const prefix = "https://t.me/kindev_bot/app?startapp="
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("link url = %q", u)
	}
	return strings.TrimPrefix(u, prefix)
}

func (e *tgEnv) confirm(t *testing.T, tgID int64, name, start string) string {
	t.Helper()
	rec := e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, tgID, name, start)))
	if rec.Code != 200 {
		t.Fatalf("confirm = %d %s", rec.Code, rec.Body)
	}
	code, _ := decode(t, rec)["code"].(string)
	return code
}

func (e *tgEnv) complete(t *testing.T, web, code string) *httptest.ResponseRecorder {
	t.Helper()
	return doCall(t, e.h, call{method: "POST", target: "/api/telegram/link/complete", cookie: web, body: `{"code":"` + code + `"}`})
}

// link runs the whole flow for user l with Telegram user tgID.
func (e *tgEnv) link(t *testing.T, l string, tgID int64, name string) string {
	t.Helper()
	web, _ := login(t, e.h, l, "correct horse battery")
	start := e.startLink(t, web)
	if rec := e.complete(t, web, e.confirm(t, tgID, name, start)); rec.Code != 200 {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body)
	}
	return web
}

func (e *tgEnv) miniAppLogin(t *testing.T, tgID int64, name string) *httptest.ResponseRecorder {
	t.Helper()
	return e.post(t, "/api/telegram/login", initBody(e.initData(t, tgID, name, "")))
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status || decode(t, rec)["error"] != code {
		t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, status, code)
	}
}

func countRows(t *testing.T, d *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// AC: the Owner stores the bot token (write-only, encrypted by the vault) and the Mini App short name.
func TestTelegramSettingsAreOwnerOnlyAndTheTokenWriteOnly(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	owner, _ := login(t, e.h, "owner", "correct horse battery")
	alice, _ := login(t, e.h, "alice", "correct horse battery")
	put := func(cookie, body string) *httptest.ResponseRecorder {
		return doCall(t, e.h, call{method: "PUT", target: "/api/telegram/settings", cookie: cookie, body: body})
	}
	wantError(t, put(alice, `{"botUsername":"kindev_bot","appShortName":"app","botToken":"`+fakeBotToken+`"}`), 403, "forbidden")
	wantError(t, doCall(t, e.h, call{method: "GET", target: "/api/telegram/settings", cookie: alice}), 403, "forbidden")
	if rec := put(owner, `{"botUsername":"kindev_bot","appShortName":"app"}`); rec.Code != 422 || decode(t, rec)["message"] != "bot_token_required" {
		t.Fatalf("no token yet = %d %s", rec.Code, rec.Body)
	}
	if rec := put(owner, `{"botUsername":"kindev_bot","appShortName":"app","botToken":"nope"}`); rec.Code != 422 || decode(t, rec)["field"] != "botToken" {
		t.Fatalf("bad token = %d %s", rec.Code, rec.Body)
	}
	rec := put(owner, `{"botUsername":"@kindev_bot","appShortName":"app","botToken":"`+fakeBotToken+`"}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "TEST_token") {
		t.Fatalf("save = %d %s", rec.Code, rec.Body)
	}
	got := decode(t, rec)
	if got["configured"] != true || got["tokenSet"] != true || got["botUsername"] != "kindev_bot" || got["appShortName"] != "app" {
		t.Fatalf("settings = %v", got)
	}
	// An empty token keeps the stored one.
	if rec := put(owner, `{"botUsername":"kindev_bot","appShortName":"app2"}`); rec.Code != 200 || decode(t, rec)["tokenSet"] != true {
		t.Fatalf("keep token = %d %s", rec.Code, rec.Body)
	}
	if tok, _ := e.vault.BotToken(t.Context()); tok != fakeBotToken {
		t.Fatal("token not kept in the vault")
	}
	if n := countRows(t, e.db, "SELECT count(*) FROM audit_events WHERE kind = 'telegram.settings_updated' AND details NOT LIKE '%TEST_token%'"); n != 2 {
		t.Fatalf("settings audit events = %d", n)
	}
	if strings.Contains(e.logs.String(), "TEST_token") {
		t.Fatal("token reached the logs")
	}
}

// AC: the backend validates raw initData (signature, auth_date); an unknown Telegram ID does not create an account.
func TestMiniAppLoginNeedsValidInitDataAndALinkedAccount(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	wantError(t, e.miniAppLogin(t, 42, "Ann"), 503, "telegram_not_configured")
	e.configure(t)
	users := countRows(t, e.db, "SELECT count(*) FROM users")
	wantError(t, e.miniAppLogin(t, 42, "Ann"), 401, "telegram_not_linked")
	if countRows(t, e.db, "SELECT count(*) FROM users") != users {
		t.Fatal("an unknown Telegram user created an account")
	}
	forged := strings.Replace(e.initData(t, 42, "Ann", ""), "%22id%22%3A42", "%22id%22%3A43", 1)
	wantError(t, e.post(t, "/api/telegram/login", initBody(forged)), 401, "telegram_auth_invalid")
	old := e.initData(t, 42, "Ann", "")
	e.clock.add(domain.InitDataMaxAge + time.Minute)
	wantError(t, e.post(t, "/api/telegram/login", initBody(old)), 401, "telegram_auth_expired")
	// initDataUnsafe-style input (no signature) is refused.
	wantError(t, e.post(t, "/api/telegram/login", `{"initData":"user=%7B%22id%22%3A42%7D&auth_date=1"}`), 401, "telegram_auth_invalid")
}

// AC: linking — web request (TTL) → t.me link → Mini App checks initData and token → code confirmed in the web
// session → identity + audit; afterwards the Mini App signs in as the same user.
func TestTelegramLinkFlow(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	web, _ := login(t, e.h, "alice", "correct horse battery")
	start := e.startLink(t, web)
	if !strings.HasPrefix(start, "link_") || len(start) > 64 {
		t.Fatalf("start param %q", start)
	}
	status := func() map[string]any {
		rec := doCall(t, e.h, call{method: "GET", target: "/api/telegram/link", cookie: web})
		if rec.Code != 200 {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		return decode(t, rec)
	}
	if req, _ := status()["request"].(map[string]any); req["state"] != "awaiting_telegram" || req["url"] != nil {
		t.Fatalf("status before Mini App = %v", req)
	}

	rec := e.post(t, "/api/telegram/link/preview", initBody(e.initData(t, 42, "Ann", start)))
	if rec.Code != 200 || decode(t, rec)["login"] != "alice" || decode(t, rec)["code"] != nil {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	code := e.confirm(t, 42, "Ann", start)
	if len(code) != 6 {
		t.Fatalf("code %q", code)
	}
	if req, _ := status()["request"].(map[string]any); req["state"] != "awaiting_code" || req["telegramName"] != "@ann" {
		t.Fatalf("status after Mini App = %v", req)
	}
	// Mini App sessions and other web sessions of the same user cannot finish the request.
	other, _ := login(t, e.h, "alice", "correct horse battery")
	wantError(t, e.complete(t, other, code), 404, "link_invalid")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	wantError(t, e.complete(t, web, wrong), 422, "link_code_wrong")
	rec = e.complete(t, web, code)
	if rec.Code != 200 || decode(t, rec)["telegramId"] != "42" {
		t.Fatalf("complete = %d %s", rec.Code, rec.Body)
	}
	if id, _ := status()["identity"].(map[string]any); id["username"] != "ann" {
		t.Fatalf("identity = %v", id)
	}

	// Replay of the link and of the code.
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", start))), 410, "link_used")
	wantError(t, e.complete(t, web, code), 410, "link_used")

	rec = e.miniAppLogin(t, 42, "Ann")
	c := tgCookie(t, rec)
	if rec.Code != 200 || c == nil || !c.HttpOnly {
		t.Fatalf("Mini App login = %d %s cookie %v", rec.Code, rec.Body, c)
	}
	if rec := e.meWithTelegram(t, c.Value); rec.Code != 200 || decode(t, rec)["login"] != "alice" {
		t.Fatalf("me via Telegram session = %d %s", rec.Code, rec.Body)
	}
	if n := countRows(t, e.db, "SELECT count(*) FROM user_sessions WHERE user_id = 'u-alice' AND via = 'telegram'"); n != 1 {
		t.Fatalf("telegram sessions = %d", n)
	}
	for _, kind := range []string{"telegram.link_requested", "telegram.linked"} {
		if countRows(t, e.db, "SELECT count(*) FROM audit_events WHERE kind = ? AND actor_id = 'u-alice'", kind) != 1 {
			t.Errorf("missing audit event %s", kind)
		}
	}
	if countRows(t, e.db, "SELECT count(*) FROM audit_events WHERE kind = 'auth.login_succeeded' AND details LIKE '%\"method\":\"telegram\"%'") != 1 {
		t.Error("Mini App sign-in not audited")
	}
	for _, secret := range []string{strings.TrimPrefix(start, "link_"), code, c.Value} {
		if strings.Contains(e.logs.String(), secret) || countRows(t, e.db, "SELECT count(*) FROM audit_events WHERE details LIKE ?", "%"+secret+"%") != 0 {
			t.Fatal("a token or code leaked into logs or audit")
		}
	}
}

// AC: a stolen link — someone else's Telegram account that opens it first cannot be linked: the code is shown only
// to them and only the creating web session can enter one; the owner's own Telegram is then refused.
func TestStolenLinkCannotLinkAStranger(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	web, _ := login(t, e.h, "alice", "correct horse battery")
	start := e.startLink(t, web)
	mallory := e.confirm(t, 666, "Mallory", start)
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", start))), 409, "link_claimed")
	// Mallory has a session of her own but not Alice's web session.
	malloryWeb, _ := login(t, e.h, "bob", "correct horse battery")
	wantError(t, e.complete(t, malloryWeb, mallory), 404, "link_invalid")
	// Guessing codes in Alice's session cancels the request after LinkCodeAttempts.
	var rec *httptest.ResponseRecorder
	for range domain.LinkCodeAttempts {
		rec = e.complete(t, web, "999999x")
	}
	wantError(t, rec, 410, "link_cancelled")
	wantError(t, e.complete(t, web, mallory), 410, "link_cancelled")
	if countRows(t, e.db, "SELECT count(*) FROM telegram_identities") != 0 {
		t.Fatal("a stolen link created an identity")
	}
	if countRows(t, e.db, "SELECT count(*) FROM audit_events WHERE kind = 'telegram.link_cancelled'") != 1 {
		t.Fatal("cancellation not audited")
	}
}

// AC: an expired link request is refused.
func TestExpiredLinkRequest(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	web, _ := login(t, e.h, "alice", "correct horse battery")
	start := e.startLink(t, web)
	e.clock.add(domain.LinkRequestTTL)
	wantError(t, e.post(t, "/api/telegram/link/preview", initBody(e.initData(t, 42, "Ann", start))), 410, "link_expired")
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", start))), 410, "link_expired")
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", "link_unknown"))), 404, "link_invalid")
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", ""))), 404, "link_invalid")
}

// AC: a new request replaces the previous one of the same user.
func TestNewLinkRequestCancelsThePrevious(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	web, _ := login(t, e.h, "alice", "correct horse battery")
	first := e.startLink(t, web)
	e.startLink(t, web)
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", first))), 410, "link_cancelled")
}

// AC: conflicts are blocked — a Telegram ID linked to another user, or a user with another Telegram ID.
func TestLinkConflictsAreBlocked(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	bobWeb := e.link(t, "bob", 42, "Ann")
	wantError(t, doCall(t, e.h, call{method: "POST", target: "/api/telegram/link", cookie: bobWeb}), 409, "telegram_already_linked")
	web, _ := login(t, e.h, "alice", "correct horse battery")
	start := e.startLink(t, web)
	wantError(t, e.post(t, "/api/telegram/link/confirm", initBody(e.initData(t, 42, "Ann", start))), 409, "telegram_taken")
	if countRows(t, e.db, "SELECT count(*) FROM telegram_identities WHERE user_id = 'u-alice'") != 0 {
		t.Fatal("conflict linked alice")
	}
}

// AC: unlinking removes the identity and ends Telegram sessions; password sessions stay.
func TestUnlinkRevokesTelegramSessions(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	web := e.link(t, "alice", 42, "Ann")
	tg := tgCookie(t, e.miniAppLogin(t, 42, "Ann")).Value
	if rec := doCall(t, e.h, call{method: "DELETE", target: "/api/telegram/link", cookie: web}); rec.Code != 204 {
		t.Fatalf("unlink = %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.meWithTelegram(t, tg), 401, "unauthenticated")
	if rec := doCall(t, e.h, call{method: "GET", target: "/api/auth/me", cookie: web}); rec.Code != 200 {
		t.Fatalf("password session = %d", rec.Code)
	}
	wantError(t, e.miniAppLogin(t, 42, "Ann"), 401, "telegram_not_linked")
	wantError(t, doCall(t, e.h, call{method: "DELETE", target: "/api/telegram/link", cookie: web}), 401, "telegram_not_linked")
	if countRows(t, e.db, "SELECT count(*) FROM audit_events WHERE kind = 'telegram.unlinked'") != 1 {
		t.Fatal("unlink not audited")
	}
	// Linking again works after unlinking.
	e.link(t, "alice", 42, "Ann")
}

// AC: disabling the user revokes Telegram sessions and blocks Mini App sign-in.
func TestDisablingUserRevokesTelegramSessions(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	e.link(t, "alice", 42, "Ann")
	tg := tgCookie(t, e.miniAppLogin(t, 42, "Ann")).Value
	owner, _ := login(t, e.h, "owner", "correct horse battery")
	if rec := doCall(t, e.h, call{method: "POST", target: "/api/users/u-alice/disable", cookie: owner}); rec.Code != 204 {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	wantError(t, e.meWithTelegram(t, tg), 401, "unauthenticated")
	wantError(t, e.miniAppLogin(t, 42, "Ann"), 403, "account_disabled")
}

// The Mini App cookie must reach the server from the Telegram web client's cross-site iframe over HTTPS.
func TestMiniAppCookieOverHTTPS(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	e.link(t, "alice", 42, "Ann")
	rec := doCall(t, e.h, call{method: "POST", target: "/api/telegram/login", body: initBody(e.initData(t, 42, "Ann", "")),
		headers: map[string]string{"X-Forwarded-Proto": "https"}})
	c := tgCookie(t, rec)
	if c == nil || !c.Secure || c.SameSite != http.SameSiteNoneMode || !c.Partitioned || !c.HttpOnly {
		t.Fatalf("cookie = %+v", c)
	}
	// Plain HTTP (trusted local profile) never gets SameSite=None.
	if c := tgCookie(t, e.miniAppLogin(t, 42, "Ann")); c.SameSite != http.SameSiteStrictMode || c.Secure {
		t.Fatalf("http cookie = %+v", c)
	}
	// Logout ends the Mini App session too.
	tg := tgCookie(t, e.miniAppLogin(t, 42, "Ann")).Value
	if rec := doCall(t, e.h, call{method: "POST", target: "/api/auth/logout", headers: map[string]string{"Cookie": TelegramSessionCookie + "=" + tg}}); rec.Code != 204 {
		t.Fatalf("logout = %d", rec.Code)
	}
	wantError(t, e.meWithTelegram(t, tg), 401, "unauthenticated")
}

// AC: KIN-11 invitations are accepted from the Mini App (startapp=invite_…) and link that Telegram account.
func TestInvitationAcceptedInMiniAppLinksTelegram(t *testing.T) {
	t.Parallel()
	e := telegramStack(t)
	e.configure(t)
	owner, _ := login(t, e.h, "owner", "correct horse battery")
	invite := func() string {
		rec := doCall(t, e.h, call{method: "POST", target: "/api/invitations", body: `{"projectId":"p1","role":"member"}`, cookie: owner})
		if rec.Code != 201 {
			t.Fatalf("invite = %d %s", rec.Code, rec.Body)
		}
		tok, _ := decode(t, rec)["token"].(string)
		return tok
	}
	accept := func(token, login string, tgID int64) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"token": token, "login": login, "password": "a long enough password",
			"initData": e.initData(t, tgID, "Dave", "invite_"+token)})
		return e.post(t, "/api/invitations/accept", string(b))
	}
	if rec := accept(invite(), "dave", 77); rec.Code != 201 {
		t.Fatalf("accept = %d %s", rec.Code, rec.Body)
	}
	if rec := e.miniAppLogin(t, 77, "Dave"); rec.Code != 200 || decode(t, rec)["user"].(map[string]any)["login"] != "dave" {
		t.Fatalf("Mini App login after invite = %d %s", rec.Code, rec.Body)
	}
	// The same Telegram account cannot accept a second invitation as another user; nothing is created.
	tok := invite()
	wantError(t, accept(tok, "erin", 77), 409, "telegram_taken")
	if countRows(t, e.db, "SELECT count(*) FROM users WHERE login = 'erin'") != 0 {
		t.Fatal("conflicting acceptance created a user")
	}
	if rec := accept(tok, "erin", 78); rec.Code != 201 {
		t.Fatalf("invitation still usable = %d %s", rec.Code, rec.Body)
	}
}
