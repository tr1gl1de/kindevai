package app

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

type memInvitations struct {
	mu        sync.Mutex
	byID      map[string]domain.Invitation
	createErr error
}

func (m *memInvitations) Create(_ context.Context, inv domain.Invitation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.createErr != nil {
		return m.createErr
	}
	m.byID[inv.ID] = inv
	return nil
}

func (m *memInvitations) List(_ context.Context, limit int, _ time.Time) ([]domain.Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Invitation, 0, len(m.byID))
	for _, inv := range m.byID {
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out[:min(limit, len(out))], nil
}

func (m *memInvitations) ByID(_ context.Context, id string) (domain.Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.byID[id]
	if !ok {
		return domain.Invitation{}, domain.ErrNotFound
	}
	return inv, nil
}

func (m *memInvitations) ByTokenHash(_ context.Context, h []byte) (domain.Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inv := range m.byID {
		if string(inv.TokenHash) == string(h) {
			return inv, nil
		}
	}
	return domain.Invitation{}, domain.ErrNotFound
}

func (m *memInvitations) update(id string, now time.Time, f func(*domain.Invitation)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.byID[id]
	if !ok || inv.State(now) != domain.InvitationPending {
		return false
	}
	f(&inv)
	m.byID[id] = inv
	return true
}

func (m *memInvitations) MarkConsumed(_ context.Context, id, userID string, now time.Time) (bool, error) {
	return m.update(id, now, func(i *domain.Invitation) { i.ConsumedAt, i.ConsumedBy = &now, userID }), nil
}

func (m *memInvitations) Revoke(_ context.Context, id string, now time.Time) (bool, error) {
	return m.update(id, now, func(i *domain.Invitation) { i.RevokedAt = &now }), nil
}

type memProjects struct {
	mu       sync.Mutex
	names    map[string]string
	grants   map[string]string // projectID/userID → role
	grantErr error
}

func (p *memProjects) Lookup(_ context.Context, id string) (ProjectRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, ok := p.names[id]
	if !ok {
		return ProjectRef{}, domain.ErrNotFound
	}
	return ProjectRef{ID: id, Name: n}, nil
}

func (p *memProjects) ValidateRole(role string) error {
	if role != "member" && role != "project_admin" {
		return &domain.ValidationError{Field: "role", Code: "role_unknown"}
	}
	return nil
}

func (p *memProjects) Grant(_ context.Context, projectID, userID, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.grantErr != nil {
		return p.grantErr
	}
	p.grants[projectID+"/"+userID] = role
	return nil
}

// snapshotTx gives the in-memory fakes transaction semantics: state is restored when fn fails.
type snapshotTx struct {
	mu    sync.Mutex
	accs  *memAccounts
	invs  *memInvitations
	proj  *memProjects
	audit *memAudit
}

func (s *snapshotTx) InTx(ctx context.Context, fn func(context.Context) error) error {
	s.mu.Lock() // serialised like the single SQLite writer
	defer s.mu.Unlock()
	users, invs, grants := maps.Clone(s.accs.users), maps.Clone(s.invs.byID), maps.Clone(s.proj.grants)
	entries := slices.Clone(s.audit.entries)
	if err := fn(ctx); err != nil {
		s.accs.users, s.invs.byID, s.proj.grants, s.audit.entries = users, invs, grants, entries
		return err
	}
	return nil
}

type invFixture struct {
	svc   *InvitationsService
	accs  *memAccounts
	invs  *memInvitations
	proj  *memProjects
	audit *memAudit
	clock *clock
	owner auth.Principal
}

func newInvFixture() *invFixture {
	f := &invFixture{
		accs:  &memAccounts{users: map[string]domain.User{"o": {ID: "o", Login: "owner", Role: domain.RoleOwner}}},
		invs:  &memInvitations{byID: map[string]domain.Invitation{}},
		proj:  &memProjects{names: map[string]string{"p1": "Alpha"}, grants: map[string]string{}},
		audit: &memAudit{},
		clock: &clock{t: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)},
		owner: auth.Principal{UserID: "o", Login: "owner", Owner: true},
	}
	n := 0
	f.svc = &InvitationsService{
		Invitations: f.invs, Accounts: f.accs, Projects: f.proj,
		Tx:     &snapshotTx{accs: f.accs, invs: f.invs, proj: f.proj, audit: f.audit},
		Audit:  f.audit,
		Hasher: hasher{}, Rand: rand.Reader, Now: f.clock.Now,
		NewID: func() string { n++; return "id" + string(rune('0'+n)) },
	}
	return f
}

func (f *invFixture) invite(t *testing.T) (InvitationView, string) {
	t.Helper()
	v, token, err := f.svc.Create(t.Context(), f.owner, "p1", "member", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return v, token
}

func validationCode(err error) string {
	var ve *domain.ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func TestCreateInvitation(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	v, token := f.invite(t)
	if token == "" || v.ProjectName != "Alpha" || v.State != domain.InvitationPending {
		t.Fatalf("view = %+v, token %q", v, token)
	}
	if got := v.Invitation.ExpiresAt.Sub(f.clock.t); got != domain.DefaultInvitationTTL {
		t.Fatalf("ttl = %v, want default", got)
	}
	stored, _ := f.invs.ByID(t.Context(), v.Invitation.ID)
	if string(stored.TokenHash) != string(domain.HashInvitationToken(token)) {
		t.Fatal("only the token hash must be stored")
	}

	tests := []struct {
		name    string
		actor   auth.Principal
		project string
		role    string
		ttl     int
		want    error
		code    string
	}{
		{"non-owner", auth.Principal{UserID: "u"}, "p1", "member", 0, domain.ErrForbidden, ""},
		{"unknown project", f.owner, "nope", "member", 0, nil, "project_not_found"},
		{"unknown role", f.owner, "p1", "owner", 0, nil, "role_unknown"},
		{"ttl too long", f.owner, "p1", "member", 169, nil, "ttl_out_of_range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := f.svc.Create(t.Context(), tt.actor, tt.project, tt.role, tt.ttl)
			if tt.want != nil && !errors.Is(err, tt.want) || tt.code != "" && validationCode(err) != tt.code {
				t.Fatalf("err = %v, want %v / %s", err, tt.want, tt.code)
			}
		})
	}
}

func TestAcceptCreatesAccountAndMembershipOnce(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	v, token := f.invite(t)

	p, err := f.svc.Preview(t.Context(), token)
	if err != nil || p.ProjectName != "Alpha" || p.Role != "member" {
		t.Fatalf("Preview = %+v, %v", p, err)
	}
	u, err := f.svc.Accept(t.Context(), token, " New.User ", "a long enough password")
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if u.Login != "new.user" || u.Role != domain.RoleUser || u.MustChangePassword || u.PasswordHash != "h:a long enough password" {
		t.Fatalf("user = %+v", u)
	}
	if f.proj.grants["p1/"+u.ID] != "member" {
		t.Fatalf("grants = %v", f.proj.grants)
	}
	inv, _ := f.invs.ByID(t.Context(), v.Invitation.ID)
	if inv.State(f.clock.t) != domain.InvitationUsed || inv.ConsumedBy != u.ID {
		t.Fatalf("invitation = %+v", inv)
	}
	if _, err := f.svc.Accept(t.Context(), token, "second", "a long enough password"); !errors.Is(err, domain.ErrInvitationUsed) {
		t.Fatalf("reuse: err = %v, want ErrInvitationUsed", err)
	}
	if _, err := f.svc.Preview(t.Context(), token); !errors.Is(err, domain.ErrInvitationUsed) {
		t.Fatalf("preview after use: err = %v", err)
	}
}

func TestAcceptRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(f *invFixture, token, id string)
		token string // "" = the issued token
		login string
		want  error
		code  string
	}{
		{"unknown token", nil, "nope", "newuser", domain.ErrInvitationInvalid, ""},
		{"expired", func(f *invFixture, _, _ string) { f.clock.t = f.clock.t.Add(domain.DefaultInvitationTTL) }, "", "newuser", domain.ErrInvitationExpired, ""},
		{"revoked", func(f *invFixture, _, id string) {
			if err := f.svc.Revoke(context.Background(), f.owner, id); err != nil {
				panic(err)
			}
		}, "", "newuser", domain.ErrInvitationRevoked, ""},
		{"login taken keeps the invitation usable", nil, "", "owner", domain.ErrLoginTaken, ""},
		{"bad login", nil, "", "x", nil, "login_invalid"},
		{"project deleted", func(f *invFixture, _, _ string) { delete(f.proj.names, "p1") }, "", "newuser", domain.ErrInvitationInvalid, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInvFixture()
			v, token := f.invite(t)
			if tt.setup != nil {
				tt.setup(f, token, v.Invitation.ID)
			}
			if tt.token != "" {
				token = tt.token
			}
			if tt.name == "project deleted" {
				f.proj.grantErr = domain.ErrNotFound
			}
			_, err := f.svc.Accept(t.Context(), token, tt.login, "a long enough password")
			if tt.want != nil && !errors.Is(err, tt.want) || tt.code != "" && validationCode(err) == "" {
				t.Fatalf("err = %v, want %v / %s", err, tt.want, tt.code)
			}
			if len(f.accs.users) != 1 || len(f.proj.grants) != 0 {
				t.Fatalf("nothing may be created: users %d, grants %v", len(f.accs.users), f.proj.grants)
			}
			if tt.name == "login taken keeps the invitation usable" {
				if inv, _ := f.invs.ByID(t.Context(), v.Invitation.ID); inv.State(f.clock.t) != domain.InvitationPending {
					t.Fatalf("invitation state = %s, want pending", inv.State(f.clock.t))
				}
			}
		})
	}
}

func TestAcceptRollsBackWhenGrantFails(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	v, token := f.invite(t)
	f.proj.grantErr = errors.New("disk full")
	if _, err := f.svc.Accept(t.Context(), token, "newuser", "a long enough password"); err == nil {
		t.Fatal("want error")
	}
	inv, _ := f.invs.ByID(t.Context(), v.Invitation.ID)
	if len(f.accs.users) != 1 || inv.State(f.clock.t) != domain.InvitationPending {
		t.Fatalf("partial accept: users %d, invitation %s", len(f.accs.users), inv.State(f.clock.t))
	}
}

func TestRevokeAndList(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	v, _ := f.invite(t)
	if err := f.svc.Revoke(t.Context(), auth.Principal{UserID: "u"}, v.Invitation.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner revoke: %v", err)
	}
	if err := f.svc.Revoke(t.Context(), f.owner, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing revoke: %v", err)
	}
	if err := f.svc.Revoke(t.Context(), f.owner, v.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := f.svc.Revoke(t.Context(), f.owner, v.Invitation.ID); validationCode(err) != "invitation_not_pending" {
		t.Fatalf("second revoke: %v", err)
	}
	list, err := f.svc.List(t.Context(), f.owner)
	if err != nil || len(list) != 1 || list[0].State != domain.InvitationRevoked || list[0].ProjectName != "Alpha" {
		t.Fatalf("List = %+v, %v", list, err)
	}
	f.invs.createErr = domain.ErrNotFound
	if _, _, err := f.svc.Create(t.Context(), f.owner, "p1", "member", 0); validationCode(err) != "project_not_found" {
		t.Fatalf("project deleted during create: %v", err)
	}
	if _, err := f.svc.List(t.Context(), auth.Principal{UserID: "u"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner list: %v", err)
	}
}

func TestInvitationLifecycleIsAudited(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	ctx := t.Context()
	v, token := f.invite(t)
	e := f.audit.last()
	if e.Kind != KindInvitationCreated || e.Actor != auditapp.UserActor(f.owner) || e.ProjectID != "p1" ||
		e.Subject != "invitation:"+v.Invitation.ID || e.Details["role"] != "member" {
		t.Fatalf("created entry = %+v", e)
	}
	u, err := f.svc.Accept(ctx, token, "newuser", "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	e = f.audit.last()
	if e.Kind != KindInvitationAccepted || e.Actor != auditapp.User(u.ID, "newuser") || e.ProjectID != "p1" ||
		e.Subject != "invitation:"+v.Invitation.ID || e.Details["userId"] != u.ID {
		t.Fatalf("accepted entry = %+v", e)
	}
	v2, _ := f.invite(t)
	if err := f.svc.Revoke(ctx, f.owner, v2.Invitation.ID); err != nil {
		t.Fatal(err)
	}
	e = f.audit.last()
	if e.Kind != KindInvitationRevoked || e.ProjectID != "p1" || e.Subject != "invitation:"+v2.Invitation.ID {
		t.Fatalf("revoked entry = %+v", e)
	}
	for _, x := range f.audit.entries {
		for _, val := range x.Details {
			if val == token {
				t.Fatal("an invitation token reached the audit log")
			}
		}
	}
	if got := f.audit.kinds(); !slices.Equal(got, []string{KindInvitationCreated, KindInvitationAccepted, KindInvitationCreated, KindInvitationRevoked}) {
		t.Fatalf("kinds = %v", got)
	}
}

func TestFailedAcceptanceIsNotAudited(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	_, token := f.invite(t)
	f.proj.grantErr = errors.New("disk full")
	if _, err := f.svc.Accept(t.Context(), token, "newuser", "a long enough password"); err == nil {
		t.Fatal("Accept succeeded")
	}
	if got := f.audit.kinds(); !slices.Equal(got, []string{KindInvitationCreated}) {
		t.Fatalf("kinds = %v, want only the creation", got)
	}
}

func TestInvitationAuditFailureFailsTheAction(t *testing.T) {
	t.Parallel()
	f := newInvFixture()
	v, token := f.invite(t)
	f.audit.err = errors.New("disk full")
	if _, _, err := f.svc.Create(t.Context(), f.owner, "p1", "member", 0); err == nil {
		t.Fatal("Create succeeded without its audit event")
	}
	if len(f.invs.byID) != 1 {
		t.Fatalf("invitation kept although the audit event failed: %d stored", len(f.invs.byID))
	}
	if err := f.svc.Revoke(t.Context(), f.owner, v.Invitation.ID); err == nil {
		t.Fatal("Revoke succeeded without its audit event")
	}
	if _, err := f.svc.Accept(t.Context(), token, "newuser", "a long enough password"); err == nil {
		t.Fatal("Accept succeeded without its audit event")
	}
	if inv, _ := f.invs.ByID(t.Context(), v.Invitation.ID); inv.State(f.clock.Now()) != domain.InvitationPending {
		t.Fatalf("invitation state = %v, want still pending", inv.State(f.clock.Now()))
	}
	if _, err := f.accs.ByLogin(t.Context(), "newuser"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("account kept although the audit event failed")
	}
}
