package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/routing/domain"
)

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

type memRepo struct {
	mu        sync.Mutex
	switches  map[[2]string]Switch        // project, agent
	bindings  map[[2]string]StoredBinding // project, role
	cooldowns map[[2]string]Cooldown      // runner, agent
}

func newMemRepo() *memRepo {
	return &memRepo{switches: map[[2]string]Switch{}, bindings: map[[2]string]StoredBinding{}, cooldowns: map[[2]string]Cooldown{}}
}

func (r *memRepo) Switches(_ context.Context, projectID string) ([]Switch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Switch
	for k, v := range r.switches {
		if k[0] == projectID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (r *memRepo) SetSwitch(_ context.Context, s Switch) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.switches[[2]string{s.ProjectID, s.Agent}] = s
	return nil
}

func (r *memRepo) Bindings(_ context.Context, projectID string) ([]StoredBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []StoredBinding
	for k, v := range r.bindings {
		if k[0] == projectID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (r *memRepo) SetBinding(_ context.Context, b StoredBinding) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[[2]string{b.ProjectID, string(b.Binding.Role)}] = b
	return nil
}

func (r *memRepo) DeleteBinding(_ context.Context, projectID string, role domain.Role) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, [2]string{projectID, string(role)})
	return nil
}

func (r *memRepo) SetCooldown(_ context.Context, c Cooldown) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooldowns[[2]string{c.RunnerID, c.Agent}] = c
	return nil
}

func (r *memRepo) Cooldowns(_ context.Context, now time.Time) ([]Cooldown, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Cooldown
	for _, c := range r.cooldowns {
		if c.Until.After(now) {
			out = append(out, c)
		}
	}
	return out, nil
}

type memTx struct{}

func (memTx) InTx(ctx context.Context, fn func(ctx context.Context) error) error { return fn(ctx) }

type memAudit struct {
	mu      sync.Mutex
	entries []auditapp.Entry
}

func (a *memAudit) Record(_ context.Context, e auditapp.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
	return nil
}

func (a *memAudit) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.entries {
		out = append(out, e.Kind)
	}
	return out
}

type fakeAccess map[string]map[string]string // user → project → role

func (f fakeAccess) Visible(_ context.Context, p auth.Principal) (map[string]string, error) {
	return f[p.UserID], nil
}

type fakeStopper struct{ stopped []string }

func (f *fakeStopper) StopAgent(_ context.Context, agent string) (int, error) {
	f.stopped = append(f.stopped, agent)
	return 2, nil
}

var (
	owner  = auth.Principal{UserID: "owner", Login: "owner", Owner: true}
	admin  = auth.Principal{UserID: "admin", Login: "admin"}
	member = auth.Principal{UserID: "dev", Login: "dev"}
)

type env struct {
	svc   *Service
	repo  *memRepo
	audit *memAudit
	stop  *fakeStopper
}

func newEnv() env {
	repo, au, st := newMemRepo(), &memAudit{}, &fakeStopper{}
	svc := &Service{Repo: repo, Tx: memTx{}, Audit: au, Workflow: st, Now: func() time.Time { return t0 },
		Access: fakeAccess{
			"owner": {"p1": "owner", "p2": "owner"},
			"admin": {"p1": "project_admin"},
			"dev":   {"p1": "developer"},
		}}
	return env{svc: svc, repo: repo, audit: au, stop: st}
}

func chain(r Route) []string {
	var out []string
	for _, c := range r.Chain {
		out = append(out, c.Agent+":"+c.Model)
	}
	return out
}

// Acceptance (KIN-33): a project switch cannot enable a CLI the Owner disabled globally, and routing never offers
// a CLI disabled at either level.
func TestGlobalSwitchWinsOverTheProject(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if err := e.svc.SetGlobalSwitch(ctx, owner, domain.AgentCodex, false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetProjectSwitch(ctx, admin, "p1", domain.AgentCodex, true); !errors.Is(err, domain.ErrDisabledGlobally) {
		t.Fatalf("enabling codex in p1: %v, want ErrDisabledGlobally", err)
	}
	r, err := e.svc.Route(ctx, "p1", "implementer", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := chain(r); !slices.Equal(got, []string{"claude:"}) {
		t.Fatalf("chain %v", got)
	}
	if err := e.svc.SetProjectSwitch(ctx, admin, "p1", domain.AgentClaude, false); err != nil {
		t.Fatal(err)
	}
	r, _ = e.svc.Route(ctx, "p1", "implementer", "")
	if len(r.Chain) != 0 || !strings.Contains(r.Blocked, "claude (disabled in the project)") ||
		!strings.Contains(r.Blocked, "codex (disabled globally)") {
		t.Fatalf("route %+v", r)
	}
	if r2, _ := e.svc.Route(ctx, "p2", "implementer", ""); !slices.Equal(chain(r2), []string{"claude:"}) {
		t.Fatalf("p2 affected by p1's switch: %v", chain(r2))
	}
	if got := e.audit.kinds(); !slices.Equal(got, []string{KindSwitched, KindSwitched}) {
		t.Fatalf("audit %v", got)
	}
}

func TestOnlyOwnerAndProjectAdminManage(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if err := e.svc.SetGlobalSwitch(ctx, admin, domain.AgentCodex, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin global switch: %v", err)
	}
	if _, err := e.svc.EmergencyStop(ctx, admin, domain.AgentCodex); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin emergency stop: %v", err)
	}
	if err := e.svc.SetProjectSwitch(ctx, member, "p1", domain.AgentCodex, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("developer project switch: %v", err)
	}
	if err := e.svc.SetProjectSwitch(ctx, admin, "p2", domain.AgentCodex, false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("admin of another project: %v", err)
	}
	if _, err := e.svc.Settings(ctx, admin); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("admin global settings: %v", err)
	}
	if _, err := e.svc.ProjectSettings(ctx, admin, "p1"); err != nil {
		t.Fatalf("admin project settings: %v", err)
	}
}

// Acceptance: role binding = primary + ordered fallback with models; a project override wins over the global
// binding, and resetting it inherits the global one again.
func TestRoleBindings(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	r, _ := e.svc.Route(ctx, "p1", "implementer", "")
	if got := chain(r); !slices.Equal(got, []string{"claude:", "codex:"}) {
		t.Fatalf("default chain %v", got)
	}
	g := domain.Binding{Role: domain.RoleImplementer, Primary: domain.Choice{Agent: "codex", Model: "gpt-5"},
		Fallback: []domain.Choice{{Agent: "claude", Model: "opus"}}}
	if err := e.svc.SetGlobalBinding(ctx, owner, g); err != nil {
		t.Fatal(err)
	}
	pb := domain.Binding{Role: domain.RoleImplementer, Primary: domain.Choice{Agent: "claude", Model: "sonnet"}}
	if err := e.svc.SetProjectBinding(ctx, admin, "p1", pb); err != nil {
		t.Fatal(err)
	}
	r, _ = e.svc.Route(ctx, "p1", "implementer", "")
	if got := chain(r); !slices.Equal(got, []string{"claude:sonnet"}) {
		t.Fatalf("project chain %v", got)
	}
	r, _ = e.svc.Route(ctx, "p2", "implementer", "")
	if got := chain(r); !slices.Equal(got, []string{"codex:gpt-5", "claude:opus"}) {
		t.Fatalf("global chain %v", got)
	}
	ps, err := e.svc.ProjectSettings(ctx, admin, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if ps.Bindings[1].Source != SourceProject || ps.Bindings[0].Source != SourceDefault {
		t.Fatalf("binding sources %+v", ps.Bindings)
	}
	if err := e.svc.ResetProjectBinding(ctx, admin, "p1", domain.RoleImplementer); err != nil {
		t.Fatal(err)
	}
	r, _ = e.svc.Route(ctx, "p1", "implementer", "")
	if got := chain(r); !slices.Equal(got, []string{"codex:gpt-5", "claude:opus"}) {
		t.Fatalf("after reset %v", got)
	}
	bad := domain.Binding{Role: domain.RoleImplementer, Primary: domain.Choice{Agent: "zcode"}}
	var ve *domain.ValidationError
	if err := e.svc.SetProjectBinding(ctx, admin, "p1", bad); !errors.As(err, &ve) {
		t.Fatalf("invalid binding: %v", err)
	}
	if got := e.audit.kinds(); !slices.Equal(got, []string{KindBound, KindBound, KindBindingReset}) {
		t.Fatalf("audit %v", got)
	}
}

// Acceptance: the reviewer prefers a CLI other than the author's.
func TestReviewerRouteDiffersFromTheAuthor(t *testing.T) {
	t.Parallel()
	e := newEnv()
	r, _ := e.svc.Route(t.Context(), "p1", "reviewer", "codex")
	if got := chain(r); !slices.Equal(got, []string{"claude:", "codex:"}) {
		t.Fatalf("reviewer after codex: %v", got)
	}
	r, _ = e.svc.Route(t.Context(), "p1", "reviewer", "claude")
	if got := chain(r); !slices.Equal(got, []string{"codex:", "claude:"}) {
		t.Fatalf("reviewer after claude: %v", got)
	}
}

// Acceptance: the emergency stop disables the CLI globally and stops its running attempts, audited.
func TestEmergencyStop(t *testing.T) {
	t.Parallel()
	e := newEnv()
	n, err := e.svc.EmergencyStop(t.Context(), owner, domain.AgentClaude)
	if err != nil || n != 2 {
		t.Fatalf("EmergencyStop = %d, %v", n, err)
	}
	if !slices.Equal(e.stop.stopped, []string{"claude"}) {
		t.Fatalf("stopped %v", e.stop.stopped)
	}
	s, _ := e.svc.Settings(t.Context(), owner)
	if s.Switches[0].Agent != "claude" || s.Switches[0].Enabled {
		t.Fatalf("claude still enabled: %+v", s.Switches)
	}
	if got := e.audit.kinds(); !slices.Equal(got, []string{KindEmergencyStop}) {
		t.Fatalf("audit %v", got)
	}
}

// cli_limit (KIN-79 reports it): a runner's CLI cools down until the reset; routing reports it, the settings list it.
func TestCooldowns(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if err := e.svc.CoolDown(ctx, "r1", "claude", t0.Add(time.Hour), "rate limit"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CoolDown(ctx, "r1", "codex", t0.Add(-time.Minute), "old"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CoolDown(ctx, "r1", "zcode", t0.Add(time.Hour), ""); err == nil {
		t.Fatal("cool-down of an unknown CLI accepted")
	}
	r, _ := e.svc.Route(ctx, "p1", "implementer", "")
	if u, ok := r.Cooling("r1", "claude"); !ok || !u.Equal(t0.Add(time.Hour)) {
		t.Fatalf("claude cooling = %v %v", u, ok)
	}
	if _, ok := r.Cooling("r1", "codex"); ok {
		t.Fatal("an expired cool-down still counts")
	}
	s, _ := e.svc.Settings(ctx, owner)
	if len(s.Cooldowns) != 1 || s.Cooldowns[0].Reason != "rate limit" {
		t.Fatalf("cooldowns %+v", s.Cooldowns)
	}
}

// Review finding: the runner's cool-down reason is untrusted text: control characters are dropped, and a reset in
// the past stores nothing.
func TestCooldownSanitizesTheRunnersText(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if err := e.svc.CoolDown(ctx, "r1", "claude", t0.Add(time.Hour), "limit\n\x1b[31mreset\x00 soon "); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CoolDown(ctx, "r1", "codex", t0.Add(-time.Hour), "past"); err != nil {
		t.Fatal(err)
	}
	cs, _ := e.repo.Cooldowns(ctx, t0.Add(-2*time.Hour))
	if len(cs) != 1 || cs[0].Reason != "limit [31mreset soon" {
		t.Fatalf("cooldowns %+v", cs)
	}
}
