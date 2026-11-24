package app_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/network/app"
	"github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
	"github.com/tr1gl1de/kindevai/internal/protocol"
)

var t0 = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

var ownerP = auth.Principal{UserID: "u-owner", Login: "owner", Owner: true}

type repo struct {
	mu       sync.Mutex
	profiles map[string]domain.Profile
	assigned map[domain.Scope]domain.Assignment
	rev      int64
	tests    map[string]domain.Test
}

func (r *repo) Profiles(context.Context) ([]domain.Profile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Profile
	for _, p := range r.profiles {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b domain.Profile) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (r *repo) Profile(_ context.Context, id string) (domain.Profile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.profiles[id]
	if !ok {
		return p, domain.ErrNotFound
	}
	return p, nil
}

func (r *repo) CreateProfile(_ context.Context, p domain.Profile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.profiles {
		if x.Name == p.Name {
			return domain.ErrNameTaken
		}
	}
	r.profiles[p.ID] = p
	return nil
}

func (r *repo) SaveProfile(_ context.Context, p domain.Profile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles[p.ID] = p
	return nil
}

func (r *repo) DeleteProfile(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.profiles, id)
	return nil
}

func (r *repo) Assignments(context.Context) ([]domain.Assignment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Assignment
	for _, a := range r.assigned {
		out = append(out, a)
	}
	return out, nil
}

func (r *repo) SetAssignment(_ context.Context, a domain.Assignment) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assigned[a.Scope] = a
	return nil
}

func (r *repo) DeleteAssignment(_ context.Context, s domain.Scope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.assigned, s)
	return nil
}

func (r *repo) NextRevision(context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rev++
	return r.rev, nil
}

func (r *repo) Revision(context.Context) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rev, nil
}

func (r *repo) CreateTest(_ context.Context, t domain.Test) error {
	return r.SaveTest(context.Background(), t)
}

func (r *repo) Test(_ context.Context, id string) (domain.Test, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tests[id]
	if !ok {
		return t, domain.ErrNotFound
	}
	return t, nil
}

func (r *repo) SaveTest(_ context.Context, t domain.Test) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tests[t.ID] = t
	return nil
}

func (r *repo) Tests(context.Context, string, int) ([]domain.Test, error) { return nil, nil }

type vault struct {
	mu sync.Mutex
	m  map[string]string
}

func (v *vault) Put(_ context.Context, name, value, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.m[name] = value
	return nil
}

func (v *vault) Get(_ context.Context, name string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.m[name]
	return s, ok, nil
}

func (v *vault) IsSet(ctx context.Context, name string) (bool, error) {
	_, ok, err := v.Get(ctx, name)
	return ok, err
}

func (v *vault) Delete(_ context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.m, name)
	return nil
}

type runners map[string]app.RunnerRef

func (r runners) Runner(_ context.Context, id string) (app.RunnerRef, error) {
	x, ok := r[id]
	if !ok {
		return x, domain.ErrNotFound
	}
	return x, nil
}

func (r runners) Runners(context.Context) ([]app.RunnerRef, error) {
	var out []app.RunnerRef
	for _, x := range r {
		out = append(out, x)
	}
	return out, nil
}

type projects map[string]bool

func (p projects) Exists(_ context.Context, id string) error {
	if !p[id] {
		return domain.ErrNotFound
	}
	return nil
}

type sent struct {
	runner, id, kind string
	body             json.RawMessage
}

type commands struct {
	mu   sync.Mutex
	sent []sent
}

func (c *commands) Send(_ context.Context, runnerID, id, kind string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sent {
		if s.runner == runnerID && s.id == id {
			return nil // the command log is idempotent by id
		}
	}
	c.sent = append(c.sent, sent{runnerID, id, kind, raw})
	return nil
}

func (c *commands) of(runner, kind string) []sent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sent
	for _, s := range c.sent {
		if s.runner == runner && s.kind == kind {
			out = append(out, s)
		}
	}
	return out
}

type auditLog struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (a *auditLog) Record(_ context.Context, e auditapp.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditLog) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Kind)
	}
	return out
}

type tx struct{}

func (tx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

type env struct {
	svc   *app.Service
	repo  *repo
	vault *vault
	cmds  *commands
	audit *auditLog
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var k secrets.Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(k)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{repo: &repo{profiles: map[string]domain.Profile{}, assigned: map[domain.Scope]domain.Assignment{}, tests: map[string]domain.Test{}},
		vault: &vault{m: map[string]string{}}, cmds: &commands{}, audit: &auditLog{}}
	var n int
	var mu sync.Mutex
	e.svc = &app.Service{Repo: e.repo, Secrets: e.vault, Cipher: box, Commands: e.cmds, Tx: tx{}, Audit: e.audit,
		Runners: runners{
			"r1": {ID: "r1", Name: "box-1", Projects: []string{"p1", "p2"}, Active: true},
			"r2": {ID: "r2", Name: "box-2", Active: true},
			"r3": {ID: "r3", Name: "pending", Active: false},
		},
		Projects:    projects{"p1": true, "p2": true},
		YouTrackURL: func(context.Context) string { return "https://yt.example" },
		Now:         func() time.Time { return t0 },
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("id%d", n)
		}}
	return e
}

func (e *env) create(t *testing.T, r app.ProfileRequest) app.ProfileView {
	t.Helper()
	v, err := e.svc.CreateProfile(t.Context(), ownerP, r)
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	return v
}

func TestProxyPasswordGoesToTheVaultNeverOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.create(t, app.ProfileRequest{Name: "Corp", ProxyURL: "socks5://alice:s3cr%40t@proxy.example:1080",
		Enforcement: "strict"})
	if !v.PasswordSet || v.Profile.Proxy.URL() != "socks5://alice@proxy.example:1080" {
		t.Fatalf("view = %+v", v)
	}
	if got := e.vault.m[app.PasswordSecret(v.Profile.ID)]; got != "s3cr@t" {
		t.Fatalf("vault holds %q", got)
	}
	if s := fmt.Sprintf("%+v", e.audit.entries); strings.Contains(s, "s3cr") || !strings.Contains(s, "passwordChanged") {
		t.Fatalf("audit = %s", s)
	}

	// Updating without a password keeps it; ClearPassword removes it.
	if _, err := e.svc.UpdateProfile(t.Context(), ownerP, v.Profile.ID, app.ProfileRequest{Name: "Corp",
		ProxyURL: "socks5://alice@proxy.example:1080", Enforcement: "strict"}); err != nil {
		t.Fatal(err)
	}
	if e.vault.m[app.PasswordSecret(v.Profile.ID)] != "s3cr@t" {
		t.Fatal("password lost on update without one")
	}
	v, err := e.svc.UpdateProfile(t.Context(), ownerP, v.Profile.ID, app.ProfileRequest{Name: "Corp",
		ProxyFields:   &app.ProxyFields{Scheme: "socks5", Host: "proxy.example", Port: 1080, Username: "alice"},
		ClearPassword: true, Enforcement: "strict"})
	if err != nil || v.PasswordSet {
		t.Fatalf("after clear: %+v %v", v, err)
	}
}

func TestOnlyTheOwnerManagesProfiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	member := auth.Principal{UserID: "u2", Login: "bob"}
	if _, err := e.svc.CreateProfile(t.Context(), member, app.ProfileRequest{Name: "x"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("create: %v", err)
	}
	if err := e.svc.Assign(t.Context(), member, "system", "", "x"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("assign: %v", err)
	}
	if _, err := e.svc.RequestTest(t.Context(), member, "r1", ""); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("test: %v", err)
	}
}

func TestAssignDeliversConfigWithSealedPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sys := e.create(t, app.ProfileRequest{Name: "Strict", ProxyURL: "http://u:hunter2@proxy.example:3128", Enforcement: "strict"})
	loose := e.create(t, app.ProfileRequest{Name: "Loose", Enforcement: "off"})
	if err := e.svc.Assign(t.Context(), ownerP, "system", "", sys.Profile.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Assign(t.Context(), ownerP, "project", "p1", loose.Profile.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Assign(t.Context(), ownerP, "role", "codex", loose.Profile.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.cmds.of("r3", protocol.CommandConfigUpdate)) != 0 {
		t.Fatal("inactive runner got a configuration")
	}
	cmds := e.cmds.of("r1", protocol.CommandConfigUpdate)
	if len(cmds) != 3 {
		t.Fatalf("r1 got %d config updates, want one per change", len(cmds))
	}
	last := cmds[len(cmds)-1]
	if strings.Contains(string(last.body), "hunter2") {
		t.Fatal("the queued command holds the plaintext password")
	}
	// Opened for the runner it was sealed for, right before sending.
	out, err := e.svc.PrepareCommand(t.Context(), "r1", last.id, last.kind, last.body)
	if err != nil {
		t.Fatal(err)
	}
	var cu protocol.ConfigUpdate
	if err := json.Unmarshal(out, &cu); err != nil {
		t.Fatal(err)
	}
	if cu.Revision != 3 || cu.Network.Default.Proxy == nil || cu.Network.Default.Proxy.Password != "hunter2" ||
		cu.Network.Default.Proxy.PasswordSealed != nil {
		t.Fatalf("config = %+v", cu.Network.Default)
	}
	// Strict from the system level holds for the project and the role that assigned an "off" profile.
	var scopes []string
	for _, s := range cu.Network.Scoped {
		scopes = append(scopes, s.Project+"/"+s.Role)
		if s.Profile.Enforcement != "strict" || !s.Profile.FailClosed || s.Profile.Proxy.Password != "hunter2" {
			t.Fatalf("scoped %s/%s = %+v", s.Project, s.Role, s.Profile)
		}
	}
	if !slices.Equal(scopes, []string{"p1/", "p1/codex", "/codex"}) {
		t.Fatalf("scopes = %v", scopes)
	}
	if len(cu.Network.Endpoints) != 5 || cu.Network.Endpoints[4].URL != "https://yt.example" {
		t.Fatalf("endpoints = %+v", cu.Network.Endpoints)
	}
	// Another runner's session cannot open it (sealed to runner + command id).
	out, _ = e.svc.PrepareCommand(t.Context(), "r2", last.id, last.kind, last.body)
	if strings.Contains(string(out), "hunter2") {
		t.Fatal("a command sealed for r1 opened for r2")
	}
	if !slices.Contains(e.audit.kinds(), app.KindAssigned) {
		t.Fatalf("audit kinds = %v", e.audit.kinds())
	}
}

func TestEffectiveKeepsSystemStrict(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sys := e.create(t, app.ProfileRequest{Name: "Strict", ProxyURL: "socks5://proxy.example:1080", Enforcement: "strict"})
	off := e.create(t, app.ProfileRequest{Name: "Off"})
	for _, a := range [][2]string{{"system", ""}, {"runner", "r1"}, {"project", "p1"}, {"role", "claude"}} {
		id := off.Profile.ID
		if a[0] == "system" {
			id = sys.Profile.ID
		}
		if err := e.svc.Assign(t.Context(), ownerP, a[0], a[1], id); err != nil {
			t.Fatal(err)
		}
	}
	eff, err := e.svc.Effective(t.Context(), ownerP, "r1", "p1", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Enforcement != domain.EnforcementStrict || eff.StrictFrom != domain.LevelSystem {
		t.Fatalf("effective = %+v", eff)
	}
}

func TestAssignValidatesTargets(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.create(t, app.ProfileRequest{Name: "P"})
	if err := e.svc.Assign(t.Context(), ownerP, "runner", "nope", p.Profile.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown runner: %v", err)
	}
	if err := e.svc.Assign(t.Context(), ownerP, "project", "nope", p.Profile.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if err := e.svc.Assign(t.Context(), ownerP, "system", "", "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown profile: %v", err)
	}
	if err := e.svc.Assign(t.Context(), ownerP, "system", "", p.Profile.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteProfile(t.Context(), ownerP, p.Profile.ID); !errors.Is(err, domain.ErrInUse) {
		t.Fatalf("delete assigned: %v", err)
	}
	if err := e.svc.Assign(t.Context(), ownerP, "system", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteProfile(t.Context(), ownerP, p.Profile.ID); err != nil {
		t.Fatalf("delete unassigned: %v", err)
	}
}

func TestPushConfigOnSessionIsIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for range 2 {
		if err := e.svc.PushConfig(t.Context(), "r2"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.cmds.of("r2", protocol.CommandConfigUpdate)); n != 1 {
		t.Fatalf("queued %d configs, want 1", n)
	}
	if err := e.svc.PushConfig(t.Context(), "r3"); err != nil || len(e.cmds.of("r3", protocol.CommandConfigUpdate)) != 0 {
		t.Fatalf("inactive runner: %v", err)
	}
}

func TestTestNetworkRunsOnTheRunner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.create(t, app.ProfileRequest{Name: "P", ProxyURL: "http://u:pw@proxy.example:3128", ExpectedIPv4: "203.0.113.7"})
	tt, err := e.svc.RequestTest(t.Context(), ownerP, "r1", p.Profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tt.Status != domain.TestPending || tt.Expected.IPv4 != "203.0.113.7" {
		t.Fatalf("test = %+v", tt)
	}
	cmds := e.cmds.of("r1", protocol.CommandNetworkTest)
	if len(cmds) != 1 || strings.Contains(string(cmds[0].body), `"pw"`) {
		t.Fatalf("commands = %+v", cmds)
	}
	out, _ := e.svc.PrepareCommand(t.Context(), "r1", cmds[0].id, cmds[0].kind, cmds[0].body)
	var nt protocol.NetworkTest
	_ = json.Unmarshal(out, &nt)
	if nt.TestID != tt.ID || nt.Profile.Proxy.Password != "pw" || len(nt.Endpoints) == 0 {
		t.Fatalf("network.test = %+v", nt)
	}

	// Another runner cannot answer it.
	res := protocol.NetworkTestResult{TestID: tt.ID, OK: true, ObservedIPv4: "203.0.113.7", DurationMs: 900}
	if err := e.svc.RecordResult(t.Context(), "r2", res); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.repo.Test(t.Context(), tt.ID); got.Status != domain.TestPending {
		t.Fatalf("result of another runner applied: %+v", got)
	}
	if err := e.svc.RecordResult(t.Context(), "r1", res); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Test(t.Context(), ownerP, tt.ID)
	if got.Status != domain.TestOK || got.ObservedIPv4 != "203.0.113.7" || got.CompletedAt == nil {
		t.Fatalf("test = %+v", got)
	}
	if k := e.audit.kinds(); !slices.Contains(k, app.KindTestRequested) || !slices.Contains(k, app.KindTestCompleted) {
		t.Fatalf("audit kinds = %v", k)
	}
	if _, err := e.svc.RequestTest(t.Context(), ownerP, "r3", ""); err == nil {
		t.Fatal("test on an inactive runner accepted")
	}
}
