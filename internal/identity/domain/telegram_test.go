package domain_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

// testBotToken is a fake bot token; it has the right shape but belongs to no bot.
const testBotToken = "123456789:TEST_token-for-unit-tests_only_000"

// vectorInitData was signed with testBotToken by an independent implementation (Python hmac/hashlib, following
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app), so the Go code is checked
// against the published algorithm rather than against itself. It carries the newer "signature" field, which must
// stay part of the data-check-string.
const vectorInitData = "query_id=AAHdF6IQAAAAAN0XohDhrOrc&user=%7B%22id%22%3A279058397%2C%22first_name%22%3A%22Vladislav%22%2C%22last_name%22%3A%22Kibenko%22%2C%22username%22%3A%22vdkfrost%22%2C%22language_code%22%3A%22ru%22%2C%22is_premium%22%3Atrue%2C%22allows_write_to_pm%22%3Atrue%7D&auth_date=1662771648&start_param=link_abc&signature=6fbdaab833d39f54518bd5c3eb3f511d035e68cb&hash=a42608262a410e2f66291e4b7402fb96680042cc4c1a73cb1f397fa9a0777c75"

var vectorAuthDate = time.Unix(1662771648, 0)

// sign builds initData for fields signed with token (the same algorithm, for cases the vector does not cover).
func sign(t *testing.T, token string, fields map[string]string) string {
	t.Helper()
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+fields[k])
	}
	sk := hmac.New(sha256.New, []byte("WebAppData"))
	sk.Write([]byte(token))
	m := hmac.New(sha256.New, sk.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	v := url.Values{}
	for k, val := range fields {
		v.Set(k, val)
	}
	v.Set("hash", hex.EncodeToString(m.Sum(nil)))
	return v.Encode()
}

func TestValidateInitDataAcceptsOfficialAlgorithmVector(t *testing.T) {
	t.Parallel()
	d, err := domain.ValidateInitData(vectorInitData, testBotToken, vectorAuthDate.Add(time.Minute))
	if err != nil {
		t.Fatalf("ValidateInitData: %v", err)
	}
	if d.User.ID != 279058397 || d.User.Username != "vdkfrost" || d.User.FirstName != "Vladislav" || d.User.LanguageCode != "ru" {
		t.Errorf("user = %+v", d.User)
	}
	if d.StartParam != "link_abc" || !d.AuthDate.Equal(vectorAuthDate) {
		t.Errorf("start_param %q auth_date %v", d.StartParam, d.AuthDate)
	}
}

func TestValidateInitDataRejects(t *testing.T) {
	t.Parallel()
	now := vectorAuthDate.Add(time.Minute)
	tamper := func(from, to string) string { return strings.Replace(vectorInitData, from, to, 1) }
	cases := []struct {
		name  string
		raw   string
		token string
		now   time.Time
		want  error
	}{
		{"other bot's token", vectorInitData, "987654321:TEST_token-for-unit-tests_only_000", now, domain.ErrInitDataInvalid},
		{"changed user id", tamper("279058397", "279058398"), testBotToken, now, domain.ErrInitDataInvalid},
		{"changed start_param", tamper("link_abc", "link_abd"), testBotToken, now, domain.ErrInitDataInvalid},
		{"changed signature field", tamper("signature=6fbd", "signature=7fbd"), testBotToken, now, domain.ErrInitDataInvalid},
		{"no hash", tamper("&hash=", "&nohash="), testBotToken, now, domain.ErrInitDataInvalid},
		{"hash not hex", tamper("hash=a426", "hash=z426"), testBotToken, now, domain.ErrInitDataInvalid},
		{"hash truncated", strings.TrimSuffix(vectorInitData, "75"), testBotToken, now, domain.ErrInitDataInvalid},
		{"empty", "", testBotToken, now, domain.ErrInitDataInvalid},
		{"garbage", "%zz", testBotToken, now, domain.ErrInitDataInvalid},
		{"too long", vectorInitData + "&pad=" + strings.Repeat("a", 8192), testBotToken, now, domain.ErrInitDataInvalid},
		{"no bot token", vectorInitData, "", now, domain.ErrInitDataInvalid},
		{"older than max age", vectorInitData, testBotToken, vectorAuthDate.Add(domain.InitDataMaxAge + time.Second), domain.ErrInitDataExpired},
		{"from the future", vectorInitData, testBotToken, vectorAuthDate.Add(-10 * time.Minute), domain.ErrInitDataExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := domain.ValidateInitData(c.raw, c.token, c.now); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestValidateInitDataNeedsAUser(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	auth := "1800000000"
	for name, fields := range map[string]map[string]string{
		"no user":       {"auth_date": auth},
		"user not json": {"auth_date": auth, "user": "alice"},
		"user id zero":  {"auth_date": auth, "user": `{"id":0}`},
		"no auth_date":  {"user": `{"id":42}`},
		"bad auth_date": {"auth_date": "soon", "user": `{"id":42}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := domain.ValidateInitData(sign(t, testBotToken, fields), testBotToken, now); !errors.Is(err, domain.ErrInitDataInvalid) {
				t.Fatalf("err = %v, want ErrInitDataInvalid", err)
			}
		})
	}
	d, err := domain.ValidateInitData(sign(t, testBotToken, map[string]string{"auth_date": auth, "user": `{"id":42,"first_name":"Ann"}`}), testBotToken, now)
	if err != nil || d.User.ID != 42 || d.User.FirstName != "Ann" || d.StartParam != "" {
		t.Fatalf("minimal initData: %+v, %v", d, err)
	}
}

func TestTelegramUserDisplayName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		u    domain.TelegramUser
		want string
	}{
		{domain.TelegramUser{ID: 1, Username: "ann", FirstName: "Ann"}, "@ann"},
		{domain.TelegramUser{ID: 1, FirstName: "Ann", LastName: "Lee"}, "Ann Lee"},
		{domain.TelegramUser{ID: 7}, "7"},
	} {
		if got := c.u.DisplayName(); got != c.want {
			t.Errorf("DisplayName(%+v) = %q, want %q", c.u, got, c.want)
		}
	}
}

func TestNewBotSettings(t *testing.T) {
	t.Parallel()
	if _, err := domain.NewBotToken(testBotToken); err != nil {
		t.Errorf("valid token: %v", err)
	}
	for _, bad := range []string{"", "abc", "123:short", "12345678:" + strings.Repeat("a", 30) + " x", "x123456789:" + strings.Repeat("a", 35)} {
		var ve *domain.ValidationError
		if _, err := domain.NewBotToken(bad); !errors.As(err, &ve) || ve.Field != "botToken" {
			t.Errorf("NewBotToken(%q) = %v, want botToken validation error", bad, err)
		}
	}
	if u, err := domain.NewBotUsername(" @Kindev_Bot "); err != nil || u != "Kindev_Bot" {
		t.Errorf("NewBotUsername = %q, %v", u, err)
	}
	for _, bad := range []string{"", "kindev", "kin dev_bot", "a_bot" + strings.Repeat("x", 40)} {
		if _, err := domain.NewBotUsername(bad); err == nil {
			t.Errorf("NewBotUsername(%q) accepted", bad)
		}
	}
	if n, err := domain.NewAppShortName("app"); err != nil || n != "app" {
		t.Errorf("NewAppShortName = %q, %v", n, err)
	}
	for _, bad := range []string{"", "ap", "my app", "-app", strings.Repeat("a", 65)} {
		if _, err := domain.NewAppShortName(bad); err == nil {
			t.Errorf("NewAppShortName(%q) accepted", bad)
		}
	}
	s := domain.BotSettings{BotUsername: "Kindev_Bot", AppShortName: "app"}
	if got := s.LinkURL("link_tok"); got != "https://t.me/Kindev_Bot/app?startapp=link_tok" {
		t.Errorf("LinkURL = %q", got)
	}
}
