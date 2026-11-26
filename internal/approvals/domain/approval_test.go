package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

const hash1 = "1111111111111111111111111111111111111111111111111111111111111111"

func request(scope Scope) Request {
	return Request{ID: "a1", ProjectID: "p1", TaskID: "t1", TaskTitle: "Add login", Revision: 2, PlanHash: hash1,
		Scope: scope, Reason: ReasonPlan, Risk: RiskL2, ProposedRisk: RiskL1}
}

func TestNewRequestValidates(t *testing.T) {
	t.Parallel()
	bad := []func(r *Request){
		func(r *Request) { r.TaskID = "" },
		func(r *Request) { r.Revision = 0 },
		func(r *Request) { r.PlanHash = "abc" },
		func(r *Request) { r.Scope = "release_main" }, // main/master is never approved through the platform
		func(r *Request) { r.Risk = "L7" },
		func(r *Request) { r.Reason = ReasonScopeExceeded; r.StepKey = "" },
	}
	for i, mut := range bad {
		r := request(ScopeExecute)
		mut(&r)
		if _, err := NewRequest(r, t0); err == nil {
			t.Errorf("case %d: NewRequest accepted %+v", i, r)
		}
	}
	a, err := NewRequest(request(ScopeExecute), t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StatePending || a.Approver != "" || a.DecidedAt != nil {
		t.Fatalf("new request = %+v", a)
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	owner := Approver{ID: "u1", Login: "owner"}
	tests := []struct {
		name    string
		d       Decision
		want    State
		wantErr error
		field   string
	}{
		{"approve", Decision{Approver: owner, Approve: true, Revision: 2, PlanHash: hash1}, StateApproved, nil, ""},
		{"reject with comment", Decision{Approver: owner, Revision: 2, PlanHash: hash1, Comment: " split the migration "}, StateRejected, nil, ""},
		{"reject needs a comment", Decision{Approver: owner, Revision: 2, PlanHash: hash1}, StatePending, nil, "comment"},
		{"older revision", Decision{Approver: owner, Approve: true, Revision: 1, PlanHash: hash1}, StatePending, ErrStaleRevision, ""},
		{"other hash", Decision{Approver: owner, Approve: true, Revision: 2, PlanHash: strings.Repeat("2", 64)}, StatePending, ErrStaleRevision, ""},
		{"comment too long", Decision{Approver: owner, Approve: true, Revision: 2, PlanHash: hash1, Comment: strings.Repeat("x", MaxCommentLen+1)}, StatePending, nil, "comment"},
		{"no approver", Decision{Approve: true, Revision: 2, PlanHash: hash1}, StatePending, nil, "approver"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := NewRequest(request(ScopeExecute), t0)
			if err != nil {
				t.Fatal(err)
			}
			err = a.Decide(tt.d, t0.Add(time.Minute))
			var ve *ValidationError
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case tt.field != "":
				if !errors.As(err, &ve) || ve.Field != tt.field {
					t.Fatalf("err = %v, want validation of %s", err, tt.field)
				}
			case err != nil:
				t.Fatal(err)
			}
			if a.State != tt.want {
				t.Fatalf("state = %s, want %s", a.State, tt.want)
			}
			if tt.want != StatePending && (a.ApproverID != "u1" || a.Approver != "owner" || a.DecidedAt == nil) {
				t.Fatalf("decision not recorded: %+v", a)
			}
			if tt.want == StateRejected && a.Comment != "split the migration" {
				t.Fatalf("comment = %q", a.Comment)
			}
		})
	}
}

func TestDecideOnlyOnce(t *testing.T) {
	t.Parallel()
	a, _ := NewRequest(request(ScopeExecute), t0)
	d := Decision{Approver: Approver{ID: "u1", Login: "owner"}, Approve: true, Revision: 2, PlanHash: hash1}
	if err := a.Decide(d, t0); err != nil {
		t.Fatal(err)
	}
	if err := a.Decide(d, t0); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second decision: %v", err)
	}
	a.Supersede(t0)
	if a.State != StateApproved {
		t.Fatalf("a decided approval must not be superseded, state %s", a.State)
	}
}

func TestSupersededCannotBeDecided(t *testing.T) {
	t.Parallel()
	a, _ := NewRequest(request(ScopeExecute), t0)
	a.Supersede(t0)
	if a.State != StateSuperseded {
		t.Fatalf("state = %s", a.State)
	}
	err := a.Decide(Decision{Approver: Approver{ID: "u1", Login: "o"}, Approve: true, Revision: 2, PlanHash: hash1}, t0)
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("decide superseded: %v", err)
	}
}

func TestGrantByPolicy(t *testing.T) {
	t.Parallel()
	r := request(ScopeIntegrateDev)
	r.Risk = RiskL1
	a, err := GrantByPolicy(r, t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateApproved || a.Approver != PolicyApprover || a.ApproverID != "" || a.DecidedAt == nil {
		t.Fatalf("policy grant = %+v", a)
	}
	r.Risk = RiskL2
	if _, err := GrantByPolicy(r, t0); err == nil {
		t.Fatal("policy must not grant integrate_dev for L2")
	}
	r = request(ScopeExecute)
	r.Risk = RiskL0
	if _, err := GrantByPolicy(r, t0); err == nil {
		t.Fatal("policy grants only integrate_dev")
	}
}

func TestCanDecide(t *testing.T) {
	t.Parallel()
	if !CanDecide(true) || CanDecide(false) {
		t.Fatal("only the Owner decides approvals")
	}
}
