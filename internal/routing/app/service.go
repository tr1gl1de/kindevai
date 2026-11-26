// Package app holds the agent router's use cases (KIN-33): the Owner's global AI CLI switches and role bindings,
// a project's allowlist and binding overrides (Owner or the project's admin), the emergency stop of a CLI, CLI
// cool-downs after a usage limit, and Route — the permitted CLI chain the scheduler uses for one step. Every
// change is audited in its transaction.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/routing/domain"
)

// Audit event kinds.
const (
	KindSwitched      = "routing.cli_switched"
	KindEmergencyStop = "routing.cli_emergency_stop"
	KindBound         = "routing.role_bound"
	KindBindingReset  = "routing.role_binding_reset"
	KindCLIReady      = "routing.cli_ready" // KIN-83: the Owner lifted a runner's CLI hold (logged in again)
)

// GlobalScope is the project id of the System level.
const GlobalScope = ""

// Switch is a stored enable/disable switch of a CLI at one level.
type Switch struct {
	ProjectID string // GlobalScope for the System switch
	Agent     string
	Enabled   bool
	UpdatedBy string
	UpdatedAt time.Time
}

// StoredBinding is a role binding set at one level.
type StoredBinding struct {
	ProjectID string // GlobalScope for the global binding
	Binding   domain.Binding
	UpdatedBy string
	UpdatedAt time.Time
}

// Cooldown: a runner's CLI hit its usage limit or is logged out (cli_limit); routing skips the pair while its hold
// is active (until Until, or until the Owner marks it ready when the hold is parked, KIN-83).
type Cooldown struct {
	RunnerID string
	Agent    string
	Reason   string
	domain.Hold
}

// Repo stores switches, bindings and cool-downs; every method runs on the caller's transaction (db.Conn).
type Repo interface {
	Switches(ctx context.Context, projectID string) ([]Switch, error)
	SetSwitch(ctx context.Context, s Switch) error
	Bindings(ctx context.Context, projectID string) ([]StoredBinding, error)
	SetBinding(ctx context.Context, b StoredBinding) error
	DeleteBinding(ctx context.Context, projectID string, role domain.Role) error
	SetCooldown(ctx context.Context, c Cooldown) error
	// Cooldowns lists the cool-downs still running at now (parked ones whatever their Until).
	Cooldowns(ctx context.Context, now time.Time) ([]Cooldown, error)
	// Cooldown returns the pair's last cool-down, expired or not (the streak of holds, KIN-83).
	Cooldown(ctx context.Context, runnerID, agent string) (Cooldown, bool, error)
	// DeleteCooldown forgets the pair's cool-down and streak; it reports whether there was one.
	DeleteCooldown(ctx context.Context, runnerID, agent string) (bool, error)
}

// Outbox stores outgoing messages in the caller's transaction.
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// TxRunner runs fn in one database transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// AuditLog records audit events in the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Access is the projects context: the projects p may see, with p's role there.
type Access interface {
	Visible(ctx context.Context, p auth.Principal) (map[string]string, error)
}

// Workflow is the workflow context as the emergency stop needs it (joins the caller's transaction).
type Workflow interface {
	StopAgent(ctx context.Context, agent string) (int, error)
}

// Project roles that may manage a project's agents.
const (
	RoleOwner        = "owner"
	RoleProjectAdmin = "project_admin"
)

// MaxCooldown bounds a cool-down (a reported reset far in the future is capped).
const MaxCooldown = domain.MaxHold

const maxCooldownReason = 500

// Service implements the agent router.
type Service struct {
	Repo     Repo
	Tx       TxRunner
	Audit    AuditLog
	Access   Access
	Workflow Workflow // emergency stop; required for EmergencyStop
	// Outbox tells the Alert Center about held and recovered CLIs (KIN-83); nil: no alerts.
	Outbox Outbox
	Now    func() time.Time
}

func (s *Service) owner(p auth.Principal) error {
	if !p.Owner {
		return domain.ErrForbidden
	}
	return nil
}

// MayManage reports whether p may manage the project's agents (the HTTP layer checks it before reading a body).
func (s *Service) MayManage(ctx context.Context, p auth.Principal, projectID string) error {
	return s.mayManage(ctx, p, projectID)
}

// mayManage: the Owner or the project's admin; a project p cannot see is not found.
func (s *Service) mayManage(ctx context.Context, p auth.Principal, projectID string) error {
	v, err := s.Access.Visible(ctx, p)
	if err != nil {
		return err
	}
	role, ok := v[projectID]
	switch {
	case !ok:
		return domain.ErrNotFound
	case p.Owner || role == RoleOwner || role == RoleProjectAdmin:
		return nil
	}
	return domain.ErrForbidden
}

func switches(ss []Switch) domain.Switches {
	out := domain.Switches{}
	for _, s := range ss {
		out[s.Agent] = s.Enabled
	}
	return out
}

// SwitchView is a CLI's switch at a level; Set is false while it has the default (enabled).
type SwitchView struct {
	Agent     string
	Enabled   bool
	Set       bool
	UpdatedBy string
	UpdatedAt *time.Time
}

func switchViews(ss []Switch, on func(agent string) bool) []SwitchView {
	out := make([]SwitchView, 0, len(domain.Agents))
	for _, a := range domain.Agents {
		v := SwitchView{Agent: a, Enabled: on(a)}
		if i := slices.IndexFunc(ss, func(s Switch) bool { return s.Agent == a }); i >= 0 {
			at := ss[i].UpdatedAt
			v.Set, v.UpdatedBy, v.UpdatedAt = true, ss[i].UpdatedBy, &at
		}
		out = append(out, v)
	}
	return out
}

// Binding sources.
const (
	SourceDefault = "default"
	SourceGlobal  = "global"
	SourceProject = "project"
)

// BindingView is a role's effective binding at a level and where it comes from.
type BindingView struct {
	Binding   domain.Binding
	Source    string
	UpdatedBy string
	UpdatedAt *time.Time
}

func bindingFor(role domain.Role, levels ...[]StoredBinding) (domain.Binding, BindingView) {
	sources := []string{SourceGlobal, SourceProject}
	v := BindingView{Binding: domain.DefaultBinding(role), Source: SourceDefault}
	for i, bs := range levels {
		if j := slices.IndexFunc(bs, func(b StoredBinding) bool { return b.Binding.Role == role }); j >= 0 {
			at := bs[j].UpdatedAt
			v = BindingView{Binding: bs[j].Binding, Source: sources[i], UpdatedBy: bs[j].UpdatedBy, UpdatedAt: &at}
		}
	}
	return v.Binding, v
}

// Settings is the System level as the Owner sees it.
type Settings struct {
	Switches  []SwitchView
	Bindings  []BindingView
	Cooldowns []Cooldown
}

// Settings returns the global switches, bindings and running cool-downs. Owner only.
func (s *Service) Settings(ctx context.Context, p auth.Principal) (Settings, error) {
	if err := s.owner(p); err != nil {
		return Settings{}, err
	}
	var out Settings
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		gs, err := s.Repo.Switches(ctx, GlobalScope)
		if err != nil {
			return err
		}
		out.Switches = switchViews(gs, switches(gs).Enabled)
		gb, err := s.Repo.Bindings(ctx, GlobalScope)
		if err != nil {
			return err
		}
		for _, r := range domain.Roles {
			_, v := bindingFor(r, gb)
			out.Bindings = append(out.Bindings, v)
		}
		cs, err := s.Repo.Cooldowns(ctx, s.Now())
		if err != nil {
			return err
		}
		slices.SortFunc(cs, func(a, b Cooldown) int {
			return strings.Compare(a.RunnerID+"/"+a.Agent, b.RunnerID+"/"+b.Agent)
		})
		out.Cooldowns = cs
		return nil
	})
	return out, err
}

// AgentState is a CLI in a project: the System switch, the project's switch and the result.
type AgentState struct {
	Agent     string
	Global    bool
	Project   SwitchView
	Effective bool
}

// ProjectSettings is a project's agent configuration.
type ProjectSettings struct {
	ProjectID string
	Agents    []AgentState
	Bindings  []BindingView // effective per role, with the level it comes from
}

// ProjectSettings returns the project's switches and effective bindings. Owner or the project's admin.
func (s *Service) ProjectSettings(ctx context.Context, p auth.Principal, projectID string) (ProjectSettings, error) {
	if err := s.mayManage(ctx, p, projectID); err != nil {
		return ProjectSettings{}, err
	}
	out := ProjectSettings{ProjectID: projectID}
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		gs, err := s.Repo.Switches(ctx, GlobalScope)
		if err != nil {
			return err
		}
		ps, err := s.Repo.Switches(ctx, projectID)
		if err != nil {
			return err
		}
		g, pr := switches(gs), switches(ps)
		for _, v := range switchViews(ps, pr.Enabled) {
			out.Agents = append(out.Agents, AgentState{Agent: v.Agent, Global: g.Enabled(v.Agent), Project: v,
				Effective: g.Enabled(v.Agent) && v.Enabled})
		}
		gb, err := s.Repo.Bindings(ctx, GlobalScope)
		if err != nil {
			return err
		}
		pb, err := s.Repo.Bindings(ctx, projectID)
		if err != nil {
			return err
		}
		for _, r := range domain.Roles {
			_, v := bindingFor(r, gb, pb)
			out.Bindings = append(out.Bindings, v)
		}
		return nil
	})
	return out, err
}

func (s *Service) setSwitch(ctx context.Context, p auth.Principal, projectID, agent string, enabled bool, kind string,
	extra map[string]string) error {
	if err := s.Repo.SetSwitch(ctx, Switch{ProjectID: projectID, Agent: agent, Enabled: enabled, UpdatedBy: p.Login,
		UpdatedAt: s.Now().UTC()}); err != nil {
		return err
	}
	scope := "global"
	if projectID != GlobalScope {
		scope = "project"
	}
	details := map[string]string{"agent": agent, "enabled": fmt.Sprint(enabled), "scope": scope}
	for k, v := range extra {
		details[k] = v
	}
	return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: kind, ProjectID: projectID,
		Subject: "cli:" + agent, Details: details})
}

// SetGlobalSwitch enables or disables a CLI for the whole installation. Disabling assigns no new sessions; running
// attempts finish (EmergencyStop stops them). Owner only.
func (s *Service) SetGlobalSwitch(ctx context.Context, p auth.Principal, agent string, enabled bool) error {
	if err := s.owner(p); err != nil {
		return err
	}
	if !domain.KnownAgent(agent) {
		return &domain.ValidationError{Field: "agent", Code: "agent_unknown"}
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		return s.setSwitch(ctx, p, GlobalScope, agent, enabled, KindSwitched, nil)
	})
}

// EmergencyStop disables a CLI globally and stops every attempt running with it at once, in one transaction.
// Owner only. It returns the number of attempts stopped.
func (s *Service) EmergencyStop(ctx context.Context, p auth.Principal, agent string) (int, error) {
	if err := s.owner(p); err != nil {
		return 0, err
	}
	if !domain.KnownAgent(agent) {
		return 0, &domain.ValidationError{Field: "agent", Code: "agent_unknown"}
	}
	n := 0
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if n, err = s.Workflow.StopAgent(ctx, agent); err != nil {
			return err
		}
		return s.setSwitch(ctx, p, GlobalScope, agent, false, KindEmergencyStop, map[string]string{"stopped": fmt.Sprint(n)})
	})
	return n, err
}

// SetProjectSwitch sets a CLI in the project's allowlist; enabling a CLI disabled globally is refused
// (domain.ErrDisabledGlobally). Owner or the project's admin.
func (s *Service) SetProjectSwitch(ctx context.Context, p auth.Principal, projectID, agent string, enabled bool) error {
	if err := s.mayManage(ctx, p, projectID); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		gs, err := s.Repo.Switches(ctx, GlobalScope)
		if err != nil {
			return err
		}
		if err := domain.CheckProjectSwitch(switches(gs), agent, enabled); err != nil {
			return err
		}
		return s.setSwitch(ctx, p, projectID, agent, enabled, KindSwitched, nil)
	})
}

func (s *Service) setBinding(ctx context.Context, p auth.Principal, projectID string, b domain.Binding) error {
	b = b.Normalize()
	if err := b.Check(); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.SetBinding(ctx, StoredBinding{ProjectID: projectID, Binding: b, UpdatedBy: p.Login,
			UpdatedAt: s.Now().UTC()}); err != nil {
			return err
		}
		chain := make([]string, 0, len(b.Fallback)+1)
		for _, c := range b.Choices() {
			chain = append(chain, choiceLabel(c))
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindBound, ProjectID: projectID,
			Subject: "role:" + string(b.Role), Details: map[string]string{"role": string(b.Role),
				"chain": strings.Join(chain, " → "), "strategy": string(b.Strategy)}})
	})
}

func choiceLabel(c domain.Choice) string {
	if c.Model == "" {
		return c.Agent
	}
	return c.Agent + "/" + c.Model
}

// SetGlobalBinding sets a role's binding for every project without its own. Owner only.
func (s *Service) SetGlobalBinding(ctx context.Context, p auth.Principal, b domain.Binding) error {
	if err := s.owner(p); err != nil {
		return err
	}
	return s.setBinding(ctx, p, GlobalScope, b)
}

// SetProjectBinding overrides a role's binding in the project. Owner or the project's admin.
func (s *Service) SetProjectBinding(ctx context.Context, p auth.Principal, projectID string, b domain.Binding) error {
	if err := s.mayManage(ctx, p, projectID); err != nil {
		return err
	}
	return s.setBinding(ctx, p, projectID, b)
}

func (s *Service) resetBinding(ctx context.Context, p auth.Principal, projectID string, role domain.Role) error {
	if !role.Valid() {
		return &domain.ValidationError{Field: "role", Code: "role_unknown"}
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.DeleteBinding(ctx, projectID, role); err != nil {
			return err
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindBindingReset,
			ProjectID: projectID, Subject: "role:" + string(role), Details: map[string]string{"role": string(role)}})
	})
}

// ResetGlobalBinding returns a role to the default binding. Owner only.
func (s *Service) ResetGlobalBinding(ctx context.Context, p auth.Principal, role domain.Role) error {
	if err := s.owner(p); err != nil {
		return err
	}
	return s.resetBinding(ctx, p, GlobalScope, role)
}

// ResetProjectBinding makes the project inherit the global binding of a role again. Owner or the project's admin.
func (s *Service) ResetProjectBinding(ctx context.Context, p auth.Principal, projectID string, role domain.Role) error {
	if err := s.mayManage(ctx, p, projectID); err != nil {
		return err
	}
	return s.resetBinding(ctx, p, projectID, role)
}

// CoolDown records that a runner's CLI reported cli_limit with cause (rate_limit | auth | billing; anything else
// counts as a rate limit) and returns the pair's hold: routing skips that CLI on that runner meanwhile. A rate
// limit waits for resetsAt (zero: 30 min; capped at MaxCooldown); a logged-out or billing-refused CLI backs off
// 30 min, 2 h, 6 h and is then parked until the Owner marks it ready (MarkReady). Every new hold of a human cause,
// and any streak of EscalateAfter holds, raises the CLI's alert (critical once escalated). Internal: the runner
// protocol reports it (KIN-79). reason is the runner's untrusted text: it is trimmed and bounded, and only shown.
func (s *Service) CoolDown(ctx context.Context, runnerID, agent, cause string, resetsAt time.Time, reason string) (domain.Hold, error) {
	if !domain.KnownAgent(agent) {
		return domain.Hold{}, &domain.ValidationError{Field: "agent", Code: "agent_unknown"}
	}
	if runnerID == "" {
		return domain.Hold{}, &domain.ValidationError{Field: "runnerId", Code: "runner_required"}
	}
	reason = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(reason, ""))), " ") // one line, no control characters
	if utf8.RuneCountInString(reason) > maxCooldownReason {
		reason = string([]rune(reason)[:maxCooldownReason])
	}
	var h domain.Hold
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		now := s.Now()
		prev, found, err := s.Repo.Cooldown(ctx, runnerID, agent)
		if err != nil {
			return err
		}
		var last *domain.Hold
		if found {
			last = &prev.Hold
		}
		h = domain.NextHold(last, cause, resetsAt, now, MaxCooldown)
		if err := s.Repo.SetCooldown(ctx, Cooldown{RunnerID: runnerID, Agent: agent, Reason: reason, Hold: h}); err != nil {
			return err
		}
		if found && prev.Cause == h.Cause && prev.Strikes == h.Strikes || !domain.NeedsHuman(h.Cause) && !h.Escalated() {
			return nil // the same hold again (attempts already running), or a plain rate limit: no new alert
		}
		return s.alert(ctx, TopicCLIHeld, runnerID, agent, &h, now)
	})
	return h, err
}

// Recovered forgets a runner's CLI hold and streak after an attempt with it succeeded (KIN-83) and resolves its
// alert. A success while the hold is still active (an attempt that started before it) changes nothing: only a
// retry after the wait proves the CLI works, and a parked hold is lifted only by the Owner. Internal (the workflow).
func (s *Service) Recovered(ctx context.Context, runnerID, agent string) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		c, found, err := s.Repo.Cooldown(ctx, runnerID, agent)
		if err != nil || !found || c.Active(s.Now()) {
			return err
		}
		if _, err := s.Repo.DeleteCooldown(ctx, runnerID, agent); err != nil {
			return err
		}
		return s.alert(ctx, TopicCLIReady, runnerID, agent, nil, s.Now())
	})
}

// MarkReady is the Owner's "CLI ready again" (KIN-83): after logging the runner's CLI in again or fixing its
// billing, the hold is lifted at once (a parked one too), its streak is forgotten and its alert resolved; held
// steps are routed to the pair on the next scheduler pass. Audited.
func (s *Service) MarkReady(ctx context.Context, p auth.Principal, runnerID, agent string) error {
	if err := s.owner(p); err != nil {
		return err
	}
	if !domain.KnownAgent(agent) {
		return &domain.ValidationError{Field: "agent", Code: "agent_unknown"}
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		had, err := s.Repo.DeleteCooldown(ctx, runnerID, agent)
		if err != nil {
			return err
		}
		if !had {
			return domain.ErrNotFound
		}
		if err := s.alert(ctx, TopicCLIReady, runnerID, agent, nil, s.Now()); err != nil {
			return err
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindCLIReady,
			Subject: "runner:" + runnerID, Details: map[string]string{"agent": agent}})
	})
}

// Alert Center topics (KIN-83): a runner's CLI is held for a human cause or a long streak, and it works again.
const (
	AdapterAlerts = "alerts"
	TopicCLIHeld  = "alerts.cli_held"
	TopicCLIReady = "alerts.cli_ready"
)

// CLIHeldEvent is the payload of TopicCLIHeld (TopicCLIReady carries only runnerId, agent and at). At is when
// routing decided (RFC 3339, UTC): the Alert Center ignores a cli_ready older than the alert's last cli_held, as
// outbox deliveries of different keys may reorder (KIN-88).
type CLIHeldEvent struct {
	RunnerID  string `json:"runnerId"`
	Agent     string `json:"agent"`
	Cause     string `json:"cause,omitempty"`
	Strikes   int    `json:"strikes,omitempty"`
	Escalated bool   `json:"escalated,omitempty"`
	Parked    bool   `json:"parked,omitempty"`
	Until     string `json:"until,omitempty"`
	At        string `json:"at"`
}

func (s *Service) alert(ctx context.Context, topic, runnerID, agent string, h *domain.Hold, now time.Time) error {
	if s.Outbox == nil {
		return nil
	}
	ev := CLIHeldEvent{RunnerID: runnerID, Agent: agent, At: now.UTC().Format(time.RFC3339Nano)}
	if h != nil {
		ev.Cause, ev.Strikes, ev.Escalated, ev.Parked = h.Cause, h.Strikes, h.Escalated(), h.Parked
		ev.Until = h.Until.UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	key := fmt.Sprintf("cli/%s/%s/%s/%d", runnerID, agent, topic, now.UnixNano())
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}

// Route is the permitted CLI chain for one step (KIN-A-15: System → Project → Role binding); the scheduler then
// checks runner capabilities, cool-downs and the network policy.
type Route struct {
	Chain []domain.Choice
	// Blocked explains an empty Chain: which CLIs of the binding are disabled and where.
	Blocked string
	// Cooling reports a runner's CLI held after cli_limit, with its hold.
	Cooling func(runnerID, agent string) (domain.Hold, bool)
}

// Route returns the chain for a step of role in the project; author is the CLI that did the work under review.
// Internal (the scheduler), no principal.
func (s *Service) Route(ctx context.Context, projectID, role, author string) (Route, error) {
	r := domain.Role(role)
	if !r.Valid() {
		return Route{}, &domain.ValidationError{Field: "role", Code: "role_unknown"}
	}
	var out Route
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		gs, err := s.Repo.Switches(ctx, GlobalScope)
		if err != nil {
			return err
		}
		ps, err := s.Repo.Switches(ctx, projectID)
		if err != nil {
			return err
		}
		gb, err := s.Repo.Bindings(ctx, GlobalScope)
		if err != nil {
			return err
		}
		pb, err := s.Repo.Bindings(ctx, projectID)
		if err != nil {
			return err
		}
		b, _ := bindingFor(r, gb, pb)
		pol := domain.Policy{Global: switches(gs), Project: switches(ps), Binding: b}
		chain, skipped := pol.Chain(author)
		out.Chain = chain
		if len(chain) == 0 {
			why := make([]string, 0, len(skipped))
			for _, a := range skipped {
				where := "disabled in the project"
				if !pol.Global.Enabled(a) {
					where = "disabled globally"
				}
				why = append(why, a+" ("+where+")")
			}
			out.Blocked = fmt.Sprintf("waiting for an AI CLI: no CLI bound to role %s is enabled: %s", role,
				strings.Join(why, ", "))
		}
		cs, err := s.Repo.Cooldowns(ctx, s.Now())
		if err != nil {
			return err
		}
		cool := make(map[[2]string]domain.Hold, len(cs))
		for _, c := range cs {
			cool[[2]string{c.RunnerID, c.Agent}] = c.Hold
		}
		out.Cooling = func(runnerID, agent string) (domain.Hold, bool) {
			u, ok := cool[[2]string{runnerID, agent}]
			return u, ok
		}
		return nil
	})
	return out, err
}
