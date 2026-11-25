package httpapi

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// discussionServer is the task API with the plan discussion on and an L2 task awaiting approval in project p1.
func discussionServer(t *testing.T) (srvCall func(user, method, path, body string, out any) int, taskID string, svc *app.Service, later func(time.Duration)) {
	t.Helper()
	srv, svc := newServer(t)
	var mu sync.Mutex
	now := time.Now().UTC()
	svc.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	later = func(d time.Duration) { mu.Lock(); defer mu.Unlock(); now = now.Add(d) }
	repo := svc.Repo.(*sqlite.Repository)
	svc.Planning, svc.Discussion = repo, repo
	svc.Access = access{"owner": {"p1": app.RoleOwner}, "admin": {"p1": app.RoleProjectAdmin}, "member": {"p1": "member"},
		"viewer": {"p1": "viewer"}, "outsider": {"p2": app.RoleProjectAdmin}}
	task, err := svc.CreateTask(t.Context(), "p1", "Discuss", "", "owner")
	if err != nil {
		t.Fatal(err)
	}
	plan := domain.Plan{Summary: "S", Risk: domain.RiskL2, MaxParallel: 1, Steps: []domain.PlanStep{{Key: "a", Title: "A",
		Repository: "acme/app", Scope: []string{"x/**"}}}}
	if _, err := svc.StartAnalysis(t.Context(), task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitPlan(t.Context(), task.ID, plan, "owner"); err != nil {
		t.Fatal(err)
	}
	return func(user, method, path, body string, out any) int { return call(t, srv, user, method, path, body, out) }, task.ID, svc, later
}

// AC (KIN-43): only the Owner and the project's admins write; members and viewers of the project read; another
// project's user gets 404.
func TestDiscussionAPIRoles(t *testing.T) {
	t.Parallel()
	do, id, _, _ := discussionServer(t)
	base := "/api/tasks/" + id + "/discussion"
	var m MessageView
	if c := do("admin", "POST", base+"/messages", `{"text":"Why L2?"}`, &m); c != http.StatusAccepted || m.Status != "PENDING" ||
		m.Role != "user" || m.PlanRevision != 1 {
		t.Fatalf("admin post %d %+v", c, m)
	}
	for _, u := range []string{"member", "viewer"} {
		if c := do(u, "POST", base+"/messages", `{"text":"hi"}`, nil); c != http.StatusForbidden {
			t.Fatalf("%s post %d", u, c)
		}
		var th ThreadView
		if c := do(u, "GET", base, "", &th); c != http.StatusOK || len(th.Messages) != 1 || th.CanWrite || !th.InFlight {
			t.Fatalf("%s read %d %+v", u, c, th)
		}
	}
	if c := do("outsider", "GET", base, "", nil); c != http.StatusNotFound {
		t.Fatalf("other project read %d", c)
	}
	if c := do("outsider", "POST", base+"/messages", `{"text":"hi"}`, nil); c != http.StatusNotFound {
		t.Fatalf("other project post %d", c)
	}
	if c := do("outsider", "POST", base+"/messages/"+m.ID+"/retry", "", nil); c != http.StatusNotFound {
		t.Fatalf("other project retry %d", c)
	}
	var th ThreadView
	if c := do("owner", "GET", base, "", &th); c != http.StatusOK || !th.CanWrite || th.Used != 1 || th.Limits.MaxMessageChars != 4000 {
		t.Fatalf("owner read %d %+v", c, th)
	}
}

// AC: limits — too long, one answer in flight, N per plan, M per hour — are 429 with the limit's code; the Owner
// alone changes them.
func TestDiscussionAPILimits(t *testing.T) {
	t.Parallel()
	do, id, svc, later := discussionServer(t)
	base := "/api/tasks/" + id + "/discussion/messages"
	var e LimitErrorBody
	if c := do("owner", "POST", base, `{"text":"`+strings.Repeat("a", 4001)+`"}`, &e); c != http.StatusTooManyRequests ||
		e.Code != domain.LimitMessageTooLong || e.Limit != 4000 {
		t.Fatalf("too long %d %+v", c, e)
	}
	if c := do("owner", "POST", base, `{"text":"  "}`, nil); c != http.StatusUnprocessableEntity {
		t.Fatalf("empty %d", c)
	}
	if c := do("owner", "POST", base, `{"text":"first"}`, nil); c != http.StatusAccepted {
		t.Fatalf("first %d", c)
	}
	if c := do("owner", "POST", base, `{"text":"second"}`, &e); c != http.StatusTooManyRequests || e.Code != domain.LimitAnswerInFlight {
		t.Fatalf("in flight %d %+v", c, e)
	}
	if c := do("member", "PUT", "/api/plan-discussion/limits", `{"maxMessageChars":10,"maxMessagesPerPlan":1,"maxMessagesPerHour":1,"answerTimeoutSec":600}`, nil); c != http.StatusForbidden {
		t.Fatalf("member sets limits %d", c)
	}
	if c := do("owner", "PUT", "/api/plan-discussion/limits", `{"maxMessageChars":10,"maxMessagesPerPlan":0,"maxMessagesPerHour":1,"answerTimeoutSec":600}`, nil); c != http.StatusUnprocessableEntity {
		t.Fatalf("invalid limits %d", c)
	}
	var l LimitsView
	if c := do("owner", "PUT", "/api/plan-discussion/limits", `{"maxMessageChars":10,"maxMessagesPerPlan":1,"maxMessagesPerHour":5,"answerTimeoutSec":600}`, &l); c != http.StatusOK || l.MaxMessagesPerPlan != 1 {
		t.Fatalf("owner sets limits %d %+v", c, l)
	}
	if c := do("member", "GET", "/api/plan-discussion/limits", "", &l); c != http.StatusOK || l.MaxMessageChars != 10 {
		t.Fatalf("read limits %d %+v", c, l)
	}
	if c := do("owner", "POST", base, `{"text":"third"}`, &e); c != http.StatusTooManyRequests || e.Code != domain.LimitAnswerInFlight {
		t.Fatalf("still in flight %d %+v", c, e)
	}
	later(11 * time.Minute) // no runner: the turn times out
	if err := svc.Schedule(t.Context()); err != nil {
		t.Fatal(err)
	}
	var th ThreadView
	if c := do("owner", "GET", "/api/tasks/"+id+"/discussion", "", &th); c != http.StatusOK || th.InFlight ||
		th.Messages[0].Status != "FAILED" || !strings.Contains(th.Messages[0].Reason, "no answer within") {
		t.Fatalf("timed out %d %+v", c, th)
	}
	if c := do("owner", "POST", base, `{"text":"third"}`, &e); c != http.StatusTooManyRequests || e.Code != domain.LimitPerPlan || e.Limit != 1 {
		t.Fatalf("per plan %d %+v", c, e)
	}
	if c := do("owner", "PUT", "/api/plan-discussion/limits", `{"maxMessageChars":10,"maxMessagesPerPlan":50,"maxMessagesPerHour":1,"answerTimeoutSec":600}`, nil); c != http.StatusOK {
		t.Fatalf("limits %d", c)
	}
	if c := do("owner", "POST", base, `{"text":"third"}`, &e); c != http.StatusTooManyRequests || e.Code != domain.LimitPerHour ||
		e.RetryAfterSec < 48*60 || e.RetryAfterSec > 50*60 {
		t.Fatalf("per hour %d %+v", c, e)
	}
	var m MessageView
	if c := do("owner", "POST", base+"/"+th.Messages[0].ID+"/retry", "", &m); c != http.StatusAccepted || m.Status != "PENDING" {
		t.Fatalf("a failed message may be sent again %d %+v", c, m)
	}
}
