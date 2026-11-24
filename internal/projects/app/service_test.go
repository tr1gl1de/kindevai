package app

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/projects/domain"
)

var (
	owner  = domain.Actor{UserID: "u-owner", Owner: true}
	alice  = domain.Actor{UserID: "u-alice"}
	bob    = domain.Actor{UserID: "u-bob"}
	ownerU = UserRef{ID: "u-owner", Login: "owner", Owner: true}
	aliceU = UserRef{ID: "u-alice", Login: "alice"}
	bobU   = UserRef{ID: "u-bob", Login: "bob"}
)

func newService() *Service {
	var n atomic.Int64
	return &Service{
		Repo:  newMemRepo(),
		Users: memUsers{"owner": ownerU, "alice": aliceU, "bob": bobU},
		Now:   func() time.Time { return time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC) },
		NewID: func() string { return fmt.Sprint("p", n.Add(1)) },
	}
}

func TestOnlyOwnerCreatesProjects(t *testing.T) {
	t.Parallel()
	s := newService()
	p, err := s.Create(t.Context(), owner, "Web App", "web")
	if err != nil || p.Slug != "web" || p.ID == "" {
		t.Fatalf("Create = %+v, %v", p, err)
	}
	if _, err := s.Create(t.Context(), alice, "Other", "other"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner create = %v", err)
	}
	if _, err := s.Create(t.Context(), owner, "Dup", "WEB"); !errors.Is(err, domain.ErrSlugTaken) {
		t.Fatalf("duplicate slug = %v", err)
	}
	var ve *domain.ValidationError
	if _, err := s.Create(t.Context(), owner, "", "ok-slug"); !errors.As(err, &ve) {
		t.Fatalf("bad name = %v", err)
	}
}

// two projects: alice is project_admin of web, bob is member of api.
func seeded(t *testing.T) (*Service, domain.Project, domain.Project) {
	t.Helper()
	s := newService()
	web, _ := s.Create(t.Context(), owner, "Web", "web")
	api, _ := s.Create(t.Context(), owner, "API", "api")
	if _, err := s.SetMember(t.Context(), owner, web.ID, "alice", "project_admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMember(t.Context(), owner, api.ID, "bob", "member"); err != nil {
		t.Fatal(err)
	}
	return s, web, api
}

func TestListShowsOnlyVisibleProjects(t *testing.T) {
	t.Parallel()
	s, web, api := seeded(t)
	ids := func(a domain.Actor) []string {
		vs, err := s.List(t.Context(), a)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, v := range vs {
			out = append(out, v.Project.ID+":"+v.MyRole)
		}
		return out
	}
	if got := fmt.Sprint(ids(owner)); got != fmt.Sprintf("[%s:owner %s:owner]", api.ID, web.ID) {
		t.Fatalf("owner sees %s", got)
	}
	if got := fmt.Sprint(ids(alice)); got != fmt.Sprintf("[%s:project_admin]", web.ID) {
		t.Fatalf("alice sees %s", got)
	}
	if got := fmt.Sprint(ids(domain.Actor{UserID: "nobody"})); got != "[]" {
		t.Fatalf("stranger sees %s", got)
	}
}

func TestCrossProjectIsolation(t *testing.T) {
	t.Parallel()
	s, web, api := seeded(t)
	if _, err := s.Get(t.Context(), alice, api.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alice get api = %v", err)
	}
	if _, err := s.Members(t.Context(), alice, api.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alice members api = %v", err)
	}
	if _, err := s.SetMember(t.Context(), alice, api.ID, "alice", "member"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alice self-adds to api = %v", err)
	}
	if err := s.RemoveMember(t.Context(), alice, api.ID, "bob"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alice removes bob from api = %v", err)
	}
	if _, err := s.Get(t.Context(), alice, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing project = %v", err)
	}
	if v, err := s.Get(t.Context(), alice, web.ID); err != nil || v.MyRole != "project_admin" {
		t.Fatalf("alice get web = %+v, %v", v, err)
	}
}

func TestOnlyOwnerManagesMembers(t *testing.T) {
	t.Parallel()
	s, web, _ := seeded(t)
	// Project Admin cannot grant memberships in the MVP (KIN-A-21).
	if _, err := s.SetMember(t.Context(), alice, web.ID, "bob", "member"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("project admin adds member = %v", err)
	}
	if err := s.RemoveMember(t.Context(), alice, web.ID, "alice"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("project admin removes = %v", err)
	}
	if _, err := s.SetMember(t.Context(), owner, web.ID, "ghost", "member"); !errors.Is(err, domain.ErrUserNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
	var ve *domain.ValidationError
	if _, err := s.SetMember(t.Context(), owner, web.ID, "bob", "owner"); !errors.As(err, &ve) {
		t.Fatalf("owner role via membership = %v", err)
	}
	if _, err := s.SetMember(t.Context(), owner, web.ID, "owner", "member"); !errors.As(err, &ve) {
		t.Fatalf("membership for the platform owner = %v", err)
	}
	m, err := s.SetMember(t.Context(), owner, web.ID, "bob", "member")
	if err != nil || m.Login != "bob" || m.Role != "member" {
		t.Fatalf("owner adds bob = %+v, %v", m, err)
	}
	if _, err := s.SetMember(t.Context(), owner, web.ID, "bob", "project_admin"); err != nil {
		t.Fatalf("change role: %v", err)
	}
	members, _ := s.Members(t.Context(), bob, web.ID)
	if fmt.Sprint(members) != "[{u-alice alice project_admin} {u-bob bob project_admin}]" {
		t.Fatalf("members = %v", members)
	}
	if err := s.RemoveMember(t.Context(), owner, web.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), bob, web.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("removed member still sees project: %v", err)
	}
	if err := s.RemoveMember(t.Context(), owner, web.ID, "bob"); !errors.Is(err, domain.ErrNotMember) {
		t.Fatalf("removing a non-member = %v", err)
	}
}

func TestDisabledUsers(t *testing.T) {
	t.Parallel()
	s, web, _ := seeded(t)
	users := s.Users.(memUsers)
	alice := users["alice"]
	alice.Disabled = true
	users["alice"] = alice
	users["carol"] = UserRef{ID: "u-carol", Login: "carol", Disabled: true}
	var ve *domain.ValidationError
	if _, err := s.SetMember(t.Context(), owner, web.ID, "carol", "member"); !errors.As(err, &ve) || ve.Code != "user_disabled" {
		t.Fatalf("granting to a disabled user = %v", err)
	}
	// A disabled member must still be removable, or re-enabling them would silently restore access.
	if err := s.RemoveMember(t.Context(), owner, web.ID, "alice"); err != nil {
		t.Fatalf("removing a disabled member = %v", err)
	}
}

func TestTrustedLookupAndGrantForInvitations(t *testing.T) {
	t.Parallel()
	s := newService()
	p, err := s.Create(t.Context(), owner, "Web App", "web")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Lookup(t.Context(), p.ID)
	if err != nil || got.Name != "Web App" {
		t.Fatalf("Lookup = %+v, %v", got, err)
	}
	if _, err := s.Lookup(t.Context(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Lookup missing = %v", err)
	}
	if err := s.ValidateRole("owner"); err == nil {
		t.Fatal("owner is not a project role")
	}
	if err := s.Grant(t.Context(), p.ID, "u-carol", "project_admin"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	m, ok, err := s.Repo.Membership(t.Context(), p.ID, "u-carol")
	if err != nil || !ok || m.Role != domain.RoleProjectAdmin {
		t.Fatalf("membership = %+v, %v, %v", m, ok, err)
	}
	if err := s.Grant(t.Context(), p.ID, "u-carol", "bogus"); err == nil {
		t.Fatal("Grant must validate the role")
	}
}
