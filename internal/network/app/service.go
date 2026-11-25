// Package app holds the network use cases (KIN-30): the Owner manages network profiles (proxy password kept in the
// credentials vault of KIN-26), assigns them along System → Runner → Project → CLI role, sees the effective
// profile, and runs Test Network on a runner. Every change re-delivers the runners' resolved configuration with
// runner.config.update through the KIN-22 command channel, in the same transaction.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Audit event kinds. Passwords never appear; a changed password is recorded as passwordChanged=true.
const (
	KindProfileCreated = "network.profile_created"
	KindProfileUpdated = "network.profile_updated"
	KindProfileDeleted = "network.profile_deleted"
	KindAssigned       = "network.profile_assigned"
	KindUnassigned     = "network.profile_unassigned"
	KindTestRequested  = "network.test_requested"
	KindTestCompleted  = "network.test_completed"
)

// Repo stores profiles, assignments, the configuration revision and tests. Every method runs on the caller's
// transaction (db.Conn).
type Repo interface {
	Profiles(ctx context.Context) ([]domain.Profile, error)
	Profile(ctx context.Context, id string) (domain.Profile, error) // domain.ErrNotFound
	CreateProfile(ctx context.Context, p domain.Profile) error      // domain.ErrNameTaken
	SaveProfile(ctx context.Context, p domain.Profile) error        // domain.ErrNameTaken
	DeleteProfile(ctx context.Context, id string) error
	Assignments(ctx context.Context) ([]domain.Assignment, error)
	SetAssignment(ctx context.Context, a domain.Assignment) error
	DeleteAssignment(ctx context.Context, s domain.Scope) error
	// NextRevision increments and returns the configuration revision; Revision reads it.
	NextRevision(ctx context.Context) (int64, error)
	Revision(ctx context.Context) (int64, error)
	CreateTest(ctx context.Context, t domain.Test) error
	Test(ctx context.Context, id string) (domain.Test, error) // domain.ErrNotFound
	SaveTest(ctx context.Context, t domain.Test) error
	// Tests lists the newest tests (of runnerID, or all when ""), newest first.
	Tests(ctx context.Context, runnerID string, limit int) ([]domain.Test, error)
}

// Secrets is the credentials vault (KIN-26) as this context needs it.
type Secrets interface {
	Put(ctx context.Context, name, value, by string) error
	Get(ctx context.Context, name string) (string, bool, error)
	IsSet(ctx context.Context, name string) (bool, error)
	Delete(ctx context.Context, name string) error
}

// Cipher seals values with the installation key (secrets.Box).
type Cipher interface {
	Seal(plaintext []byte, aad string) ([]byte, error)
	Open(ciphertext []byte, aad string) ([]byte, error)
}

// RunnerRef is a runner as this context sees it.
type RunnerRef struct {
	ID       string
	Name     string
	Projects []string
	// Active: approved or disabled — it has (or will have again) a session and keeps its configuration.
	Active bool
}

// Runners is the runners context's API.
type Runners interface {
	Runner(ctx context.Context, id string) (RunnerRef, error) // domain.ErrNotFound
	Runners(ctx context.Context) ([]RunnerRef, error)
}

// Projects is the projects context's API.
type Projects interface {
	Exists(ctx context.Context, id string) error // domain.ErrNotFound
}

// Commands queues runner commands in the caller's transaction (runners app.Channel).
type Commands interface {
	Send(ctx context.Context, runnerID, id, kind string, body any) error
}

// TxRunner runs fn in one database transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// AuditLog records audit events in the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Alert hooks (KIN-60): the outcome of a Test Network of a runner's effective profile, for the Alert Center.
const (
	AdapterAlerts          = "alerts"
	TopicEgressCheckFailed = "alerts.egress_check_failed"
	TopicEgressCheckPassed = "alerts.egress_check_passed" //nolint:gosec // G101: a topic name, not a credential
)

// Outbox stores outgoing messages in the caller's transaction.
type Outbox interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// Service implements the network use cases.
type Service struct {
	Repo     Repo
	Secrets  Secrets
	Cipher   Cipher
	Runners  Runners
	Projects Projects
	Commands Commands
	Tx       TxRunner
	Audit    AuditLog
	Outbox   Outbox // alert hooks; nil: none
	// YouTrackURL returns the configured YouTrack base URL ("" when none); it is a required endpoint.
	YouTrackURL func(ctx context.Context) string
	Now         func() time.Time
	NewID       func() string
	Logger      *slog.Logger
}

func (s *Service) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func owner(a auth.Principal) error {
	if !a.Owner {
		return domain.ErrForbidden
	}
	return nil
}

// PasswordSecret is the vault slot of a profile's proxy password.
func PasswordSecret(profileID string) string { return "network_profile." + profileID + ".password" }

// ProfileRequest is a profile as the Owner submits it. The proxy comes either as ProxyURL (credentials in it are
// taken out: the password goes to the vault) or as ProxyFields. Password nil keeps the stored one; ClearPassword
// removes it.
type ProfileRequest struct {
	Name          string
	ProxyURL      string
	ProxyFields   *ProxyFields
	Password      *string
	ClearPassword bool
	Enforcement   string
	DNS           string
	FailClosed    bool
	ExpectedIPv4  string
	ExpectedIPv6  string
	Allow         []domain.RuleInput
	DiagnosticURL string
}

// ProxyFields is the proxy entered as separate fields.
type ProxyFields struct {
	Scheme   string
	Host     string
	Port     int
	Username string
}

// ProfileView is a profile as the Owner sees it: the password only as "set".
type ProfileView struct {
	Profile     domain.Profile
	PasswordSet bool
	Assigned    []domain.Scope
}

func (r ProfileRequest) parse(id string) (domain.Profile, domain.Password, error) {
	var px *domain.Proxy
	var pw domain.Password
	switch {
	case r.ProxyURL != "" && r.ProxyFields != nil:
		return domain.Profile{}, pw, &domain.ValidationError{Field: "proxy", Code: "proxy_ambiguous"}
	case r.ProxyURL != "":
		p, pass, err := domain.ParseProxyURL(r.ProxyURL)
		if err != nil {
			return domain.Profile{}, pw, err
		}
		px, pw = &p, pass
	case r.ProxyFields != nil:
		p, err := domain.NewProxy(r.ProxyFields.Scheme, r.ProxyFields.Host, r.ProxyFields.Port, r.ProxyFields.Username)
		if err != nil {
			return domain.Profile{}, pw, err
		}
		px = &p
	}
	if r.Password != nil && *r.Password != "" {
		p, err := domain.NewPassword(*r.Password)
		if err != nil {
			return domain.Profile{}, pw, err
		}
		pw = p
	}
	p, err := domain.NewProfile(id, domain.ProfileInput{Name: r.Name, Proxy: px, Enforcement: domain.Enforcement(r.Enforcement),
		DNS: domain.DNSPolicy(r.DNS), FailClosed: r.FailClosed, ExpectedIPv4: r.ExpectedIPv4, ExpectedIPv6: r.ExpectedIPv6,
		Allow: r.Allow, DiagnosticURL: r.DiagnosticURL})
	return p, pw, err
}

func profileDetails(p domain.Profile, passwordChanged bool) map[string]string {
	d := map[string]string{"name": p.Name, "enforcement": string(p.Enforcement), "dns": string(p.DNS),
		"failClosed": strconv.FormatBool(p.FailClosed), "allowRules": strconv.Itoa(len(p.Allow))}
	if p.Proxy != nil {
		d["proxy"] = p.Proxy.URL()
	}
	if p.ExpectedIPv4 != "" {
		d["expectedIpv4"] = p.ExpectedIPv4
	}
	if p.ExpectedIPv6 != "" {
		d["expectedIpv6"] = p.ExpectedIPv6
	}
	if passwordChanged {
		d["passwordChanged"] = "true"
	}
	return d
}

// storePassword applies the password part of a request inside the transaction; it reports whether it changed.
func (s *Service) storePassword(ctx context.Context, a auth.Principal, p domain.Profile, pw domain.Password, clear bool) (bool, error) {
	name := PasswordSecret(p.ID)
	if p.Proxy == nil || clear {
		set, err := s.Secrets.IsSet(ctx, name)
		if err != nil || !set {
			return false, err
		}
		return true, s.Secrets.Delete(ctx, name)
	}
	if pw.IsZero() {
		return false, nil
	}
	return true, s.Secrets.Put(ctx, name, pw.Reveal(), a.UserID)
}

// CreateProfile adds a profile.
func (s *Service) CreateProfile(ctx context.Context, a auth.Principal, r ProfileRequest) (ProfileView, error) {
	if err := owner(a); err != nil {
		return ProfileView{}, err
	}
	p, pw, err := r.parse(s.NewID())
	if err != nil {
		return ProfileView{}, err
	}
	p.CreatedAt, p.UpdatedAt = s.Now().UTC(), s.Now().UTC()
	var view ProfileView
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.CreateProfile(ctx, p); err != nil {
			return err
		}
		changed, err := s.storePassword(ctx, a, p, pw, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(a), Kind: KindProfileCreated,
			Subject: "network_profile:" + p.ID, Details: profileDetails(p, changed)}); err != nil {
			return err
		}
		view = ProfileView{Profile: p, PasswordSet: changed}
		return nil
	})
	return view, err
}

// UpdateProfile replaces a profile's settings and re-delivers the configuration of every runner.
func (s *Service) UpdateProfile(ctx context.Context, a auth.Principal, id string, r ProfileRequest) (ProfileView, error) {
	if err := owner(a); err != nil {
		return ProfileView{}, err
	}
	p, pw, err := r.parse(id)
	if err != nil {
		return ProfileView{}, err
	}
	var view ProfileView
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		old, err := s.Repo.Profile(ctx, id)
		if err != nil {
			return err
		}
		p.CreatedAt, p.UpdatedAt = old.CreatedAt, s.Now().UTC()
		if err := s.Repo.SaveProfile(ctx, p); err != nil {
			return err
		}
		changed, err := s.storePassword(ctx, a, p, pw, r.ClearPassword)
		if err != nil {
			return err
		}
		if err := s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(a), Kind: KindProfileUpdated,
			Subject: "network_profile:" + p.ID, Details: profileDetails(p, changed)}); err != nil {
			return err
		}
		if err := s.pushAll(ctx); err != nil {
			return err
		}
		view, err = s.view(ctx, p)
		return err
	})
	return view, err
}

// DeleteProfile removes an unassigned profile and its password.
func (s *Service) DeleteProfile(ctx context.Context, a auth.Principal, id string) error {
	if err := owner(a); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		p, err := s.Repo.Profile(ctx, id)
		if err != nil {
			return err
		}
		as, err := s.Repo.Assignments(ctx)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(as, func(x domain.Assignment) bool { return x.ProfileID == id }) {
			return domain.ErrInUse
		}
		if err := s.Repo.DeleteProfile(ctx, id); err != nil {
			return err
		}
		if err := s.Secrets.Delete(ctx, PasswordSecret(id)); err != nil {
			return err
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(a), Kind: KindProfileDeleted,
			Subject: "network_profile:" + id, Details: map[string]string{"name": p.Name}})
	})
}

func (s *Service) view(ctx context.Context, p domain.Profile) (ProfileView, error) {
	set, err := s.Secrets.IsSet(ctx, PasswordSecret(p.ID))
	if err != nil {
		return ProfileView{}, err
	}
	as, err := s.Repo.Assignments(ctx)
	if err != nil {
		return ProfileView{}, err
	}
	v := ProfileView{Profile: p, PasswordSet: set, Assigned: []domain.Scope{}}
	for _, x := range as {
		if x.ProfileID == p.ID {
			v.Assigned = append(v.Assigned, x.Scope)
		}
	}
	return v, nil
}

// Profiles lists the profiles.
func (s *Service) Profiles(ctx context.Context, a auth.Principal) ([]ProfileView, error) {
	if err := owner(a); err != nil {
		return nil, err
	}
	ps, err := s.Repo.Profiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProfileView, 0, len(ps))
	for _, p := range ps {
		v, err := s.view(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Assignments lists where profiles are assigned.
func (s *Service) Assignments(ctx context.Context, a auth.Principal) ([]domain.Assignment, error) {
	if err := owner(a); err != nil {
		return nil, err
	}
	return s.Repo.Assignments(ctx)
}

// Assign assigns profileID at scope ("" removes the assignment) and re-delivers the runners' configuration.
func (s *Service) Assign(ctx context.Context, a auth.Principal, level, target, profileID string) error {
	if err := owner(a); err != nil {
		return err
	}
	sc, err := domain.NewScope(level, target)
	if err != nil {
		return err
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		switch sc.Level {
		case domain.LevelRunner:
			if _, err := s.Runners.Runner(ctx, sc.Target); err != nil {
				return err
			}
		case domain.LevelProject:
			if err := s.Projects.Exists(ctx, sc.Target); err != nil {
				return err
			}
		}
		details := map[string]string{"level": string(sc.Level), "target": sc.Target}
		kind := KindUnassigned
		if profileID == "" {
			if err := s.Repo.DeleteAssignment(ctx, sc); err != nil {
				return err
			}
		} else {
			p, err := s.Repo.Profile(ctx, profileID)
			if err != nil {
				return err
			}
			if err := s.Repo.SetAssignment(ctx, domain.Assignment{Scope: sc, ProfileID: p.ID, UpdatedAt: s.Now().UTC()}); err != nil {
				return err
			}
			kind, details["profile"], details["profileName"] = KindAssigned, p.ID, p.Name
		}
		if err := s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(a), Kind: kind,
			Subject: "network_scope:" + string(sc.Level) + ":" + sc.Target, Details: details}); err != nil {
			return err
		}
		return s.pushAll(ctx)
	})
}

// state is everything resolution needs, loaded once.
type state struct {
	profiles  map[string]domain.Profile
	assigned  map[domain.Scope]string
	passwords map[string]*string // read once per use case: profile id → password (nil: none)
}

func (s *Service) load(ctx context.Context) (state, error) {
	ps, err := s.Repo.Profiles(ctx)
	if err != nil {
		return state{}, err
	}
	as, err := s.Repo.Assignments(ctx)
	if err != nil {
		return state{}, err
	}
	st := state{profiles: map[string]domain.Profile{}, assigned: map[domain.Scope]string{}, passwords: map[string]*string{}}
	for _, p := range ps {
		st.profiles[p.ID] = p
	}
	for _, x := range as {
		st.assigned[x.Scope] = x.ProfileID
	}
	return st, nil
}

func (st state) at(level domain.Level, target string) *domain.Profile {
	id, ok := st.assigned[domain.Scope{Level: level, Target: target}]
	if !ok {
		return nil
	}
	p, ok := st.profiles[id]
	if !ok {
		return nil
	}
	return &p
}

func (st state) resolve(runnerID, projectID, role string) domain.Effective {
	layers := []domain.Layer{{Level: domain.LevelSystem, Profile: st.at(domain.LevelSystem, "")}}
	if runnerID != "" {
		layers = append(layers, domain.Layer{Level: domain.LevelRunner, Profile: st.at(domain.LevelRunner, runnerID)})
	}
	if projectID != "" {
		layers = append(layers, domain.Layer{Level: domain.LevelProject, Profile: st.at(domain.LevelProject, projectID)})
	}
	if role != "" {
		layers = append(layers, domain.Layer{Level: domain.LevelRole, Profile: st.at(domain.LevelRole, role)})
	}
	return domain.Resolve(layers)
}

// Effective is the profile that applies to a step of projectID run by the CLI role on runnerID (any may be "").
func (s *Service) Effective(ctx context.Context, a auth.Principal, runnerID, projectID, role string) (domain.Effective, error) {
	if err := owner(a); err != nil {
		return domain.Effective{}, err
	}
	st, err := s.load(ctx)
	if err != nil {
		return domain.Effective{}, err
	}
	return st.resolve(runnerID, projectID, role), nil
}

// StrictFor returns, for steps of projectID, whether a runner's effective profile is strict with any CLI role
// (KIN-31): such steps go only to runners that verified their strict egress sandbox. Internal (the scheduler), no
// principal.
func (s *Service) StrictFor(ctx context.Context, projectID string) (func(runnerID string) bool, error) {
	st, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	roles := []string{""}
	for sc := range st.assigned {
		if sc.Level == domain.LevelRole {
			roles = append(roles, sc.Target)
		}
	}
	return func(runnerID string) bool {
		for _, role := range roles {
			if st.resolve(runnerID, projectID, role).Enforcement == domain.EnforcementStrict {
				return true
			}
		}
		return false
	}, nil
}

func (s *Service) endpoints(ctx context.Context) []protocol.NetworkEndpoint {
	yt := ""
	if s.YouTrackURL != nil {
		yt = s.YouTrackURL(ctx)
	}
	var out []protocol.NetworkEndpoint
	for _, e := range domain.RequiredEndpoints(yt) {
		out = append(out, protocol.NetworkEndpoint(e))
	}
	return out
}

func commandAAD(runnerID, commandID string) string {
	return "runner_command:" + runnerID + ":" + commandID
}

// wire converts an effective profile, sealing its proxy password for runnerID's command commandID.
func (s *Service) wire(ctx context.Context, st state, e domain.Effective, runnerID, commandID string) (protocol.NetworkProfile, error) {
	np := protocol.NetworkProfile{ID: e.ID, Name: e.Name, Enforcement: string(e.Enforcement), DNS: string(e.DNS),
		FailClosed: e.FailClosed, ExpectedIPv4: e.ExpectedIPv4, ExpectedIPv6: e.ExpectedIPv6, Restricted: e.Restricted,
		DiagnosticURL: e.DiagnosticURL}
	for _, r := range e.Allow {
		np.Allow = append(np.Allow, protocol.NetworkRule(r))
	}
	if e.Proxy != nil {
		np.Proxy = &protocol.NetworkProxy{Scheme: string(e.Proxy.Scheme), Host: e.Proxy.Host, Port: e.Proxy.Port,
			Username: e.Proxy.Username}
		pw, cached := st.passwords[e.ProxyProfileID]
		if !cached {
			v, ok, err := s.Secrets.Get(ctx, PasswordSecret(e.ProxyProfileID))
			if err != nil {
				return np, fmt.Errorf("reading proxy password: %w", err)
			}
			if ok {
				pw = &v
			}
			st.passwords[e.ProxyProfileID] = pw
		}
		if pw != nil {
			sealed, err := s.Cipher.Seal([]byte(*pw), commandAAD(runnerID, commandID))
			if err != nil {
				return np, fmt.Errorf("sealing proxy password: %w", err)
			}
			np.Proxy.PasswordSealed = sealed
		}
	}
	return np, nil
}

// config builds runner r's configuration for command commandID.
func (s *Service) config(ctx context.Context, st state, r RunnerRef, commandID string, rev int64) (protocol.ConfigUpdate, error) {
	defEff := st.resolve(r.ID, "", "")
	def, err := s.wire(ctx, st, defEff, r.ID, commandID)
	if err != nil {
		return protocol.ConfigUpdate{}, err
	}
	cu := protocol.ConfigUpdate{Revision: rev, Network: protocol.NetworkConfig{Default: def, Endpoints: s.endpoints(ctx)}}
	var projects, roles []string
	for _, p := range r.Projects {
		if st.at(domain.LevelProject, p) != nil {
			projects = append(projects, p)
		}
	}
	for sc := range st.assigned {
		if sc.Level == domain.LevelRole && st.at(sc.Level, sc.Target) != nil {
			roles = append(roles, sc.Target)
		}
	}
	slices.Sort(roles)
	add := func(project, role string) error {
		e := st.resolve(r.ID, project, role)
		if sameEffect(e, defEff) {
			return nil // the runner's default applies anyway
		}
		if len(cu.Network.Scoped) == protocol.MaxNetworkScoped {
			// Never fail the Owner's change over it; the runner falls back to the default for the rest (KIN-31
			// enforces the strictest of the two before running a step).
			s.logger().Warn("too many scoped network profiles for one runner; the rest use the default", "runner", r.ID)
			return nil
		}
		np, err := s.wire(ctx, st, e, r.ID, commandID)
		cu.Network.Scoped = append(cu.Network.Scoped, protocol.ScopedNetworkProfile{Project: project, Role: role, Profile: np})
		return err
	}
	for _, p := range projects {
		if err := add(p, ""); err != nil {
			return cu, err
		}
		for _, role := range roles {
			if err := add(p, role); err != nil {
				return cu, err
			}
		}
	}
	for _, role := range roles {
		if err := add("", role); err != nil {
			return cu, err
		}
	}
	return cu, nil
}

// sameEffect reports whether two effective profiles configure the network identically.
func sameEffect(a, b domain.Effective) bool {
	a.ID, a.Name, a.Sources, a.CreatedAt, a.UpdatedAt = "", "", nil, time.Time{}, time.Time{}
	b.ID, b.Name, b.Sources, b.CreatedAt, b.UpdatedAt = "", "", nil, time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func configCommandID(rev int64) string { return "network-config/" + strconv.FormatInt(rev, 10) }

// pushAll bumps the revision and queues the new configuration for every active runner (caller's transaction).
func (s *Service) pushAll(ctx context.Context) error {
	rev, err := s.Repo.NextRevision(ctx)
	if err != nil {
		return err
	}
	rs, err := s.Runners.Runners(ctx)
	if err != nil {
		return err
	}
	st, err := s.load(ctx)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if !r.Active {
			continue
		}
		if err := s.push(ctx, st, r, rev); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) push(ctx context.Context, st state, r RunnerRef, rev int64) error {
	id := configCommandID(rev)
	cu, err := s.config(ctx, st, r, id, rev)
	if err != nil {
		return err
	}
	return s.Commands.Send(ctx, r.ID, id, protocol.CommandConfigUpdate, cu)
}

// PushConfig queues the current configuration for a runner whose session just opened (a runner approved after
// the last change has none yet). The command id is the revision, so repeated sessions queue nothing new.
func (s *Service) PushConfig(ctx context.Context, runnerID string) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		r, err := s.Runners.Runner(ctx, runnerID)
		if err != nil || !r.Active {
			return err
		}
		rev, err := s.Repo.Revision(ctx)
		if err != nil {
			return err
		}
		st, err := s.load(ctx)
		if err != nil {
			return err
		}
		return s.push(ctx, st, r, rev)
	})
}

// errPasswordSealed: a sealed proxy password could not be opened (wrong runner, rotated key). The channel then
// sends the stored body, whose password stays sealed: the runner reports it as undeliverable and fails closed on
// proxy authentication; the command stream is never blocked.
var errPasswordSealed = errors.New("proxy password of the runner command could not be opened")

// PrepareCommand opens the sealed proxy passwords of a network command right before it is sent to runnerID over
// its authenticated session. Other kinds pass through.
func (s *Service) PrepareCommand(_ context.Context, runnerID, commandID, kind string, body json.RawMessage) (json.RawMessage, error) {
	var failed bool
	open := func(px *protocol.NetworkProxy) {
		if px == nil || px.PasswordSealed == nil {
			return
		}
		p, err := s.Cipher.Open(px.PasswordSealed, commandAAD(runnerID, commandID))
		if err != nil {
			failed = true
			return
		}
		px.Password, px.PasswordSealed = string(p), nil
	}
	switch kind {
	case protocol.CommandConfigUpdate:
		var cu protocol.ConfigUpdate
		if err := json.Unmarshal(body, &cu); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", kind, err)
		}
		open(cu.Network.Default.Proxy)
		for i := range cu.Network.Scoped {
			open(cu.Network.Scoped[i].Profile.Proxy)
		}
		if failed {
			return nil, errPasswordSealed
		}
		return json.Marshal(cu)
	case protocol.CommandNetworkTest:
		var nt protocol.NetworkTest
		if err := json.Unmarshal(body, &nt); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", kind, err)
		}
		open(nt.Profile.Proxy)
		if failed {
			return nil, errPasswordSealed
		}
		return json.Marshal(nt)
	}
	return body, nil
}

// RequestTest asks runnerID to run Test Network for profileID ("" = the runner's effective profile).
func (s *Service) RequestTest(ctx context.Context, a auth.Principal, runnerID, profileID string) (domain.Test, error) {
	if err := owner(a); err != nil {
		return domain.Test{}, err
	}
	var t domain.Test
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		r, err := s.Runners.Runner(ctx, runnerID)
		if err != nil {
			return err
		}
		if !r.Active {
			return &domain.ValidationError{Field: "runnerId", Code: "runner_inactive"}
		}
		recent, err := s.Repo.Tests(ctx, r.ID, 1)
		if err != nil {
			return err
		}
		if len(recent) > 0 && recent[0].StatusAt(s.Now()) == domain.TestPending {
			return &domain.ValidationError{Field: "runnerId", Code: "test_pending"}
		}
		st, err := s.load(ctx)
		if err != nil {
			return err
		}
		e := st.resolve(r.ID, "", "")
		if profileID != "" {
			p, err := s.Repo.Profile(ctx, profileID)
			if err != nil {
				return err
			}
			e = domain.Resolve([]domain.Layer{{Level: domain.LevelProject, Profile: &p}})
		}
		t = domain.NewTest(s.NewID(), r.ID, profileID, a.UserID, domain.Expected{IPv4: e.ExpectedIPv4, IPv6: e.ExpectedIPv6}, s.Now())
		if err := s.Repo.CreateTest(ctx, t); err != nil {
			return err
		}
		cmdID := "network-test/" + t.ID
		np, err := s.wire(ctx, st, e, r.ID, cmdID)
		if err != nil {
			return err
		}
		if err := s.Commands.Send(ctx, r.ID, cmdID, protocol.CommandNetworkTest,
			protocol.NetworkTest{TestID: t.ID, Profile: np, Endpoints: s.endpoints(ctx)}); err != nil {
			return err
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(a), Kind: KindTestRequested,
			Subject: "network_test:" + t.ID, Details: map[string]string{"runner": r.ID, "profile": profileID}})
	})
	return t, err
}

// Tests lists recent tests (of runnerID, or all when "").
func (s *Service) Tests(ctx context.Context, a auth.Principal, runnerID string, limit int) ([]domain.Test, error) {
	if err := owner(a); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	return s.Repo.Tests(ctx, runnerID, limit)
}

// Test returns one test.
func (s *Service) Test(ctx context.Context, a auth.Principal, id string) (domain.Test, error) {
	if err := owner(a); err != nil {
		return domain.Test{}, err
	}
	return s.Repo.Test(ctx, id)
}

// RecordResult stores a runner's Test Network result. runnerID comes from the authenticated session: a result for
// a test of another runner, an unknown or a finished test is ignored. The result is untrusted input (sanitized and
// re-judged by domain.Test.Complete).
func (s *Service) RecordResult(ctx context.Context, runnerID string, r protocol.NetworkTestResult) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		t, err := s.Repo.Test(ctx, r.TestID)
		if errors.Is(err, domain.ErrNotFound) || (err == nil && t.RunnerID != runnerID) {
			s.logger().Warn("network test result for an unknown test ignored", "runner", runnerID)
			return nil
		}
		if err != nil {
			return err
		}
		res := domain.Result{OK: r.OK, Code: r.Code, Detail: r.Detail, ObservedIPv4: r.ObservedIPv4,
			ObservedIPv6: r.ObservedIPv6, DurationMs: r.DurationMs}
		for _, c := range r.Checks {
			res.Checks = append(res.Checks, domain.Check(c))
		}
		if err := t.Complete(res, s.Now()); errors.Is(err, domain.ErrTestFinished) {
			return nil
		}
		if err := s.Repo.SaveTest(ctx, t); err != nil {
			return err
		}
		if err := s.egressAlert(ctx, t); err != nil {
			return err
		}
		ref, err := s.Runners.Runner(ctx, runnerID)
		if err != nil {
			return err
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.Runner(runnerID, ref.Name), Kind: KindTestCompleted,
			Subject: "network_test:" + t.ID, Details: map[string]string{"status": string(t.Status), "code": t.Code,
				"observedIpv4": t.ObservedIPv4, "observedIpv6": t.ObservedIPv6}})
	})
}

// egressAlert tells the Alert Center how a completed test of the runner's effective profile went (a test of another
// profile says nothing about the runner's egress). Keyed by the test, so a redelivery is not counted twice.
func (s *Service) egressAlert(ctx context.Context, t domain.Test) error {
	if s.Outbox == nil || t.ProfileID != "" {
		return nil
	}
	topic, body := TopicEgressCheckPassed, map[string]string{"runnerId": t.RunnerID, "testId": t.ID}
	if t.Status != domain.TestOK {
		topic, body["code"] = TopicEgressCheckFailed, t.Code
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic,
		Key: "network-test/" + t.ID, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}
