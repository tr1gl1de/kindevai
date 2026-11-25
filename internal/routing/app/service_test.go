package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
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
		if c.Active(now) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *memRepo) Cooldown(_ context.Context, runnerID, agent string) (Cooldown, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cooldowns[[2]string{runnerID, agent}]
	return c, ok, nil
}

func (r *memRepo) DeleteCooldown(_ context.Context, runnerID, agent string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.cooldowns[[2]string{runnerID, agent}]
	delete(r.cooldowns, [2]string{runnerID, agent})
	return ok, nil
}

type memOutbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (o *memOutbox) Enqueue(_ context.Context, m messaging.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = append(o.msgs, m)
	return nil
}

// events returns the enqueued alert events as "<topic> <cause>/<strikes>/<escalated>/<parked>".
func (o *memOutbox) events(t *testing.T) []string {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, m := range o.msgs {
		var ev CLIHeldEvent
		if err := json.Unmarshal(m.Payload, &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s/%s %s/%d/%t/%t", m.Topic, ev.RunnerID, ev.Agent, ev.Cause, ev.Strikes, ev.Escalated, ev.Parked))
	}
	return out
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
	out   *memOutbox
	now   *time.Time
}

func newEnv() env {
	repo, au, st, out := newMemRepo(), &memAudit{}, &fakeStopper{}, &memOutbox{}
	now := t0
	svc := &Service{Repo: repo, Tx: memTx{}, Audit: au, Workflow: st, Outbox: out, Now: func() time.Time { return now },
		Access: fakeAccess{
			"owner": {"p1": "owner", "p2": "owner"},
			"admin": {"p1": "project_admin"},
			"dev":   {"p1": "developer"},
		}}
	return env{svc: svc, repo: repo, audit: au, stop: st, out: out, now: &now}
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
	if _, err := e.svc.CoolDown(ctx, "r1", "claude", domain.CauseRateLimit, t0.Add(time.Hour), "rate limit"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CoolDown(ctx, "r1", "codex", domain.CauseRateLimit, t0.Add(-time.Minute), "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CoolDown(ctx, "r1", "zcode", domain.CauseRateLimit, t0.Add(time.Hour), ""); err == nil {
		t.Fatal("cool-down of an unknown CLI accepted")
	}
	*e.now = t0.Add(45 * time.Minute) // codex: a reset in the past waited the default 30 min
	r, _ := e.svc.Route(ctx, "p1", "implementer", "")
	if h, ok := r.Cooling("r1", "claude"); !ok || !h.Until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("claude cooling = %v %v", h, ok)
	}
	if _, ok := r.Cooling("r1", "codex"); ok {
		t.Fatal("an expired cool-down still counts")
	}
	if ev := e.out.events(t); len(ev) != 0 {
		t.Fatalf("a plain rate limit raised alerts: %v", ev)
	}
	s, _ := e.svc.Settings(ctx, owner)
	if len(s.Cooldowns) != 1 || s.Cooldowns[0].Reason != "rate limit" {
		t.Fatalf("cooldowns %+v", s.Cooldowns)
	}
}

// Review finding: the runner's cool-down reason is untrusted text: control characters are dropped; an unknown cause
// counts as a rate limit.
func TestCooldownSanitizesTheRunnersText(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	if _, err := e.svc.CoolDown(ctx, "r1", "claude", "", t0.Add(time.Hour), "limit\n\x1b[31mreset\x00 soon "); err != nil {
		t.Fatal(err)
	}
	cs, _ := e.repo.Cooldowns(ctx, t0)
	if len(cs) != 1 || cs[0].Reason != "limit [31mreset soon" || cs[0].Cause != domain.CauseRateLimit {
		t.Fatalf("cooldowns %+v", cs)
	}
}

// Acceptance (KIN-83): a logged-out CLI backs off 30 min, 2 h, 6 h and is then parked until the Owner marks it
// ready; its alert names the runner and CLI from the first hold and escalates at the third hold in a row.
func TestLoggedOutCLIBacksOffEscalatesAndParks(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	for _, d := range []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour} {
		h, err := e.svc.CoolDown(ctx, "r1", "claude", domain.CauseAuth, time.Time{}, "not logged in")
		if err != nil || h.Parked || !h.Until.Equal(e.now.Add(d)) {
			t.Fatalf("hold = %+v, %v; want +%v", h, err, d)
		}
		// An attempt that was already running fails the same way: not a new strike, no new alert.
		if again, _ := e.svc.CoolDown(ctx, "r1", "claude", domain.CauseAuth, time.Time{}, "not logged in"); again != h {
			t.Fatalf("concurrent hold %+v, want %+v", again, h)
		}
		*e.now = h.Until.Add(time.Second)
	}
	h, _ := e.svc.CoolDown(ctx, "r1", "claude", domain.CauseAuth, time.Time{}, "not logged in")
	if !h.Parked || h.Strikes != 4 {
		t.Fatalf("4th hold %+v, want parked", h)
	}
	*e.now = e.now.Add(30 * 24 * time.Hour)
	r, _ := e.svc.Route(ctx, "p1", "implementer", "")
	if c, ok := r.Cooling("r1", "claude"); !ok || !c.NeedsOwner() {
		t.Fatalf("parked CLI routed again: %+v %v", c, ok)
	}
	if got, exp := e.out.events(t), []string{
		"alerts.cli_held r1/claude auth/1/false/false",
		"alerts.cli_held r1/claude auth/2/false/false",
		"alerts.cli_held r1/claude auth/3/true/false",
		"alerts.cli_held r1/claude auth/4/true/true",
	}; !slices.Equal(got, exp) {
		t.Fatalf("alerts %v, want %v", got, exp)
	}

	if err := e.svc.MarkReady(ctx, member, "r1", "claude"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("a member marked the CLI ready: %v", err)
	}
	if err := e.svc.MarkReady(ctx, owner, "r1", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.MarkReady(ctx, owner, "r1", "claude"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second MarkReady = %v, want not found", err)
	}
	r, _ = e.svc.Route(ctx, "p1", "implementer", "")
	if _, ok := r.Cooling("r1", "claude"); ok {
		t.Fatal("the CLI is still held after the Owner marked it ready")
	}
	if ev := e.out.events(t); ev[len(ev)-1] != "alerts.cli_ready r1/claude /0/false/false" {
		t.Fatalf("last alert event %v", ev)
	}
	if got := e.audit.kinds(); !slices.Equal(got, []string{KindCLIReady}) {
		t.Fatalf("audit %v", got)
	}
	// The streak starts over.
	if h, _ := e.svc.CoolDown(ctx, "r1", "claude", domain.CauseAuth, time.Time{}, ""); h.Strikes != 1 {
		t.Fatalf("hold after MarkReady %+v", h)
	}
}

// Acceptance (KIN-83): a rate limit repeating EscalateAfter times in a row raises a critical alert; a successful run
// on the pair forgets the streak and resolves the alert.
func TestRepeatedRateLimitsEscalateAndRecover(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	for range 3 {
		h, err := e.svc.CoolDown(ctx, "r2", "codex", domain.CauseRateLimit, e.now.Add(time.Hour), "usage limit")
		if err != nil {
			t.Fatal(err)
		}
		*e.now = h.Until.Add(time.Second)
	}
	if got := e.out.events(t); !slices.Equal(got, []string{"alerts.cli_held r2/codex rate_limit/3/true/false"}) {
		t.Fatalf("alerts %v", got)
	}
	if err := e.svc.Recovered(ctx, "r2", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Recovered(ctx, "r2", "codex"); err != nil { // nothing held: no event
		t.Fatal(err)
	}
	if got := e.out.events(t); len(got) != 2 || got[1] != "alerts.cli_ready r2/codex /0/false/false" {
		t.Fatalf("alerts %v", got)
	}
	if h, _ := e.svc.CoolDown(ctx, "r2", "codex", domain.CauseRateLimit, time.Time{}, ""); h.Strikes != 1 {
		t.Fatalf("streak survived a success: %+v", h)
	}
}
