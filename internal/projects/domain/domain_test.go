package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestNewProjectFields(t *testing.T) {
	t.Parallel()
	if n, err := NewName("  Платформа AI "); err != nil || n != "Платформа AI" {
		t.Fatalf("NewName = %q, %v", n, err)
	}
	for _, bad := range []string{"", "  ", strings.Repeat("я", 81), "a\nb"} {
		if _, err := NewName(bad); err == nil {
			t.Fatalf("NewName(%q) accepted", bad)
		}
	}
	if s, err := NewSlug(" Web-App "); err != nil || s != "web-app" {
		t.Fatalf("NewSlug = %q, %v", s, err)
	}
	for _, bad := range []string{"a", "-ab", "ab-", "a b", "проект", strings.Repeat("a", 41)} {
		var ve *ValidationError
		if _, err := NewSlug(bad); !errors.As(err, &ve) || ve.Field != "slug" {
			t.Fatalf("NewSlug(%q) = %v", bad, err)
		}
	}
}

func TestParseRole(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"project_admin", "member"} {
		if _, err := ParseRole(ok); err != nil {
			t.Fatalf("ParseRole(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "owner", "admin", "MEMBER"} {
		if _, err := ParseRole(bad); err == nil {
			t.Fatalf("ParseRole(%q) accepted", bad)
		}
	}
}

func TestAccessPolicy(t *testing.T) {
	t.Parallel()
	owner := Actor{UserID: "o", Owner: true}
	admin := Actor{UserID: "a"}
	member := Actor{UserID: "m"}
	stranger := Actor{UserID: "s"}
	roles := map[string]Role{"a": RoleProjectAdmin, "m": RoleMember}
	access := func(a Actor) Access {
		r, ok := roles[a.UserID]
		return NewAccess(a, r, ok)
	}
	tests := []struct {
		name                        string
		actor                       Actor
		view, manageMembers, create bool
	}{
		{"owner", owner, true, true, true},
		{"project admin", admin, true, false, false}, // MVP: Project Admin does not grant memberships
		{"member", member, true, false, false},
		{"stranger", stranger, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := access(tt.actor)
			if a.CanView() != tt.view || a.CanManageMembers() != tt.manageMembers || CanCreateProject(tt.actor) != tt.create {
				t.Fatalf("view=%v manage=%v create=%v", a.CanView(), a.CanManageMembers(), CanCreateProject(tt.actor))
			}
		})
	}
}

func TestCanRunAIIsOwnerOrAnyProjectRole(t *testing.T) {
	t.Parallel()
	if !NewAccess(Actor{Owner: true}, "", false).CanRunAI() {
		t.Error("owner")
	}
	for _, r := range []Role{RoleProjectAdmin, RoleMember} {
		if !NewAccess(Actor{UserID: "u"}, r, true).CanRunAI() {
			t.Errorf("%s", r)
		}
	}
	if NewAccess(Actor{UserID: "u"}, "", false).CanRunAI() {
		t.Error("non-member")
	}
}
