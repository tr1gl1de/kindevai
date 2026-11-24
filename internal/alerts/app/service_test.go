package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

type memRepo struct {
	mu     sync.Mutex
	alerts []domain.Alert
	prefs  map[string]domain.Preferences
}

func (r *memRepo) Active(_ context.Context, key string) (domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.alerts {
		if a.DedupKey == key && a.Active() {
			return a, nil
		}
	}
	return domain.Alert{}, domain.ErrNotFound
}

func (r *memRepo) Applied(_ context.Context, key, event string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.alerts {
		if a.DedupKey == key && a.LastEvent == event {
			return true, nil
		}
	}
	return false, nil
}

func (r *memRepo) Create(_ context.Context, a domain.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

func (r *memRepo) Save(_ context.Context, a domain.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.alerts {
		if r.alerts[i].ID == a.ID {
			r.alerts[i] = a
			return nil
		}
	}
	return domain.ErrNotFound
}

func (r *memRepo) Get(_ context.Context, id string) (domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.alerts {
		if a.ID == id {
			return a, nil
		}
	}
	return domain.Alert{}, domain.ErrNotFound
}

func (r *memRepo) List(_ context.Context, q app.ListQuery) ([]domain.Alert, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Alert
	for _, a := range r.alerts {
		if (a.ProjectID == "" && !q.Platform) || (a.ProjectID != "" && !slices.Contains(q.ProjectIDs, a.ProjectID)) {
			continue
		}
		if q.ActiveOnly && !a.Active() {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *memRepo) Preferences(_ context.Context, userID string) (domain.Preferences, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.prefs[userID]; ok {
		return p, nil
	}
	return domain.DefaultPreferences(userID), nil
}

func (r *memRepo) SavePreferences(_ context.Context, p domain.Preferences, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefs[p.UserID] = p
	return nil
}

type fakeProjects struct {
	visible map[string][]app.ProjectRef // user id → projects
	members map[string][]string
	admins  map[string][]string
}

func (f fakeProjects) Visible(_ context.Context, p auth.Principal) ([]app.ProjectRef, error) {
	return f.visible[p.UserID], nil
}

func (f fakeProjects) Name(_ context.Context, id string) (string, error) {
	if id == "p1" {
		return "Shop", nil
	}
	return "", domain.ErrNotFound
}

func (f fakeProjects) Members(_ context.Context, id string) ([]string, error) {
	return f.members[id], nil
}

func (f fakeProjects) Admins(_ context.Context, id string) ([]string, error) {
	return f.admins[id], nil
}

type fakePeople struct {
	owners []string
	chats  map[string]int64
}

func (f fakePeople) Owners(context.Context) ([]string, error) { return f.owners, nil }

func (f fakePeople) TelegramChat(_ context.Context, u string) (int64, bool, error) {
	c, ok := f.chats[u]
	return c, ok, nil
}

type fakeApprovals map[string]bool

func (f fakeApprovals) Pending(_ context.Context, id string) (bool, error) { return f[id], nil }

type noTx struct{}

func (noTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

type audit struct {
	mu    sync.Mutex
	kinds []string
}

func (a *audit) Record(_ context.Context, e auditapp.Entry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kinds = append(a.kinds, e.Kind)
	return nil
}

type outbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (o *outbox) Enqueue(_ context.Context, m messaging.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.msgs = append(o.msgs, m)
	return nil
}

func (o *outbox) sent(t *testing.T) []app.SendPayload {
	t.Helper()
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []app.SendPayload
	for _, m := range o.msgs {
		if m.Topic != app.TopicTelegramSend || m.Adapter != app.AdapterTelegram {
			t.Fatalf("unexpected message %+v", m)
		}
		var p app.SendPayload
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

var (
	owner  = auth.Principal{UserID: "owner", Login: "owner", Owner: true}
	admin  = auth.Principal{UserID: "admin", Login: "admin"}
	member = auth.Principal{UserID: "member", Login: "member"}
	alien  = auth.Principal{UserID: "alien", Login: "alien"}
)

type fixture struct {
	svc   *app.Service
	repo  *memRepo
	out   *outbox
	audit *audit
}

func newFixture() fixture {
	repo := &memRepo{prefs: map[string]domain.Preferences{}}
	out, au := &outbox{}, &audit{}
	n := 0
	var mu sync.Mutex
	svc := &app.Service{
		Repo: repo,
		Projects: fakeProjects{
			visible: map[string][]app.ProjectRef{
				"owner":  {{ID: "p1", Name: "Shop", Role: "owner"}, {ID: "p2", Name: "Other", Role: "owner"}},
				"admin":  {{ID: "p1", Name: "Shop", Role: app.RoleProjectAdmin}},
				"member": {{ID: "p1", Name: "Shop", Role: "member"}},
				"alien":  {{ID: "p2", Name: "Other", Role: app.RoleProjectAdmin}},
			},
			members: map[string][]string{"p1": {"admin", "member"}},
			admins:  map[string][]string{"p1": {"admin"}},
		},
		People:    fakePeople{owners: []string{"owner"}, chats: map[string]int64{"owner": 100, "member": 300}},
		Approvals: fakeApprovals{"AP1": true, "AP2": true},
		Tx:        noTx{}, Audit: au, Outbox: out,
		Now: func() time.Time { return time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC) },
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "A" + strconv.Itoa(n)
		},
	}
	return fixture{svc: svc, repo: repo, out: out, audit: au}
}

func runnerDown(sev domain.Severity) domain.Occurrence {
	return domain.Occurrence{Kind: domain.KindRunnerUnreachable, Severity: sev, DedupKey: "runner/r1/unreachable", Subject: "r1"}
}

// AC: dedup — repeats group into the active alert; a redelivered event is not counted twice; only new and
// escalated alerts notify; linked Owners get a Telegram message with a Mini App deep link.
func TestRaiseGroupsAndNotifies(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := t.Context()
	for _, ev := range []string{"e1", "e1", "e2"} {
		if err := f.svc.Raise(ctx, ev, runnerDown(domain.SeverityWarning)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.repo.alerts) != 1 || f.repo.alerts[0].Occurrences != 2 {
		t.Fatalf("alerts = %+v", f.repo.alerts)
	}
	sent := f.out.sent(t)
	if len(sent) != 1 || sent[0].ChatID != 100 || sent[0].StartParam != "alert_A1" || !strings.Contains(sent[0].Text, "r1") {
		t.Fatalf("sent = %+v", sent)
	}
	if err := f.svc.Raise(ctx, "e3", runnerDown(domain.SeverityCritical)); err != nil {
		t.Fatal(err)
	}
	if got := len(f.out.sent(t)); got != 2 {
		t.Fatalf("escalation sent %d messages in total, want 2", got)
	}
	// A system resolve closes it; the next occurrence is a new alert.
	if err := f.svc.ResolveKey(ctx, "runner/r1/unreachable"); err != nil {
		t.Fatal(err)
	}
	// Review: a redelivery of the last event after the alert closed does not open a duplicate.
	if err := f.svc.Raise(ctx, "e3", runnerDown(domain.SeverityCritical)); err != nil {
		t.Fatal(err)
	}
	if len(f.repo.alerts) != 1 {
		t.Fatalf("redelivery after close opened a duplicate: %+v", f.repo.alerts)
	}
	if err := f.svc.Raise(ctx, "e4", runnerDown(domain.SeverityWarning)); err != nil {
		t.Fatal(err)
	}
	if len(f.repo.alerts) != 2 || f.repo.alerts[0].State != domain.StateResolved {
		t.Fatalf("alerts = %+v", f.repo.alerts)
	}
}

// AC: project isolation — platform alerts are the Owner's; project alerts are visible to the project's members,
// managed by its admins; others get not-found.
func TestVisibilityAndActions(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := t.Context()
	if err := f.svc.Raise(ctx, "e1", runnerDown(domain.SeverityWarning)); err != nil {
		t.Fatal(err)
	}
	proj := domain.Occurrence{ProjectID: "p1", Kind: "ci_failed", Severity: domain.SeverityWarning, DedupKey: "ci/p1/dev", Subject: "dev"}
	if err := f.svc.Raise(ctx, "e2", proj); err != nil {
		t.Fatal(err)
	}
	count := func(p auth.Principal) int {
		vs, err := f.svc.List(ctx, p, false)
		if err != nil {
			t.Fatal(err)
		}
		return len(vs)
	}
	if count(owner) != 2 || count(admin) != 1 || count(member) != 1 || count(alien) != 0 {
		t.Fatalf("visible: owner %d admin %d member %d alien %d", count(owner), count(admin), count(member), count(alien))
	}
	if _, err := f.svc.Act(ctx, alien, "A2", app.ActionAcknowledge); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("alien: %v", err)
	}
	if _, err := f.svc.Act(ctx, admin, "A1", app.ActionAcknowledge); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("admin on platform alert: %v", err)
	}
	if _, err := f.svc.Act(ctx, member, "A2", app.ActionDismiss); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	v, err := f.svc.Act(ctx, admin, "A2", app.ActionAcknowledge)
	if err != nil || v.Alert.State != domain.StateAcknowledged || v.Alert.AckedBy != "admin" {
		t.Fatalf("admin ack: %+v %v", v, err)
	}
	if _, err := f.svc.Act(ctx, owner, "A2", app.ActionAcknowledge); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("double ack: %v", err)
	}
	if _, err := f.svc.Act(ctx, owner, "A2", app.ActionResolve); err != nil {
		t.Fatal(err)
	}
	if vs, _ := f.svc.List(ctx, owner, true); len(vs) != 1 {
		t.Fatalf("active = %d, want 1", len(vs))
	}
	if !slices.Equal(f.audit.kinds, []string{app.KindAcknowledged, app.KindResolved}) {
		t.Fatalf("audit = %q", f.audit.kinds)
	}
	// The project alert notified the linked Owner and the linked member (not the unlinked admin).
	var chats []int64
	for _, s := range f.out.sent(t) {
		if s.StartParam == "alert_A2" {
			chats = append(chats, s.ChatID)
		}
	}
	if slices.Sort(chats); !slices.Equal(chats, []int64{100, 300}) {
		t.Fatalf("chats = %v", chats)
	}
}

// AC: approval notification only to linked Owners who want it, in their language, opening the approval in the
// Mini App (no decision from the chat); per-type opt-out.
func TestNotifyApprovalHonoursPreferences(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := t.Context()
	if _, err := f.svc.SavePreferences(ctx, owner, "ru", nil); err != nil {
		t.Fatal(err)
	}
	req := app.ApprovalRequest{ApprovalID: "AP1", ProjectID: "p1", TaskID: "T1", Risk: "L2", Reason: "execute"}
	if err := f.svc.NotifyApproval(ctx, req); err != nil {
		t.Fatal(err)
	}
	sent := f.out.sent(t)
	if len(sent) != 1 || sent[0].ChatID != 100 || sent[0].StartParam != "approval_AP1" ||
		!strings.Contains(sent[0].Text, "Shop") || !strings.Contains(sent[0].Text, "одобрение") || sent[0].Button == "" {
		t.Fatalf("sent = %+v", sent)
	}
	if f.out.msgs[0].Key != "approval/AP1/owner" {
		t.Fatalf("key = %q", f.out.msgs[0].Key)
	}
	v, err := f.svc.SavePreferences(ctx, owner, "en", []domain.NotificationType{domain.NotifyApprovalRequested})
	if err != nil || !v.TelegramLinked || v.Preferences.Wants(domain.NotifyApprovalRequested) {
		t.Fatalf("prefs = %+v, %v", v, err)
	}
	if err := f.svc.NotifyApproval(ctx, app.ApprovalRequest{ApprovalID: "AP2"}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.out.sent(t)); got != 1 {
		t.Fatalf("opted-out owner still notified (%d messages)", got)
	}
	// Review: an approval decided meanwhile is not announced.
	if _, err := f.svc.SavePreferences(ctx, owner, "en", nil); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.NotifyApproval(ctx, app.ApprovalRequest{ApprovalID: "DECIDED"}); err != nil {
		t.Fatal(err)
	}
	if got := len(f.out.sent(t)); got != 1 {
		t.Fatalf("decided approval announced (%d messages)", got)
	}
	var ve *domain.ValidationError
	if _, err := f.svc.SavePreferences(ctx, owner, "xx", nil); !errors.As(err, &ve) {
		t.Fatalf("invalid language: %v", err)
	}
}

// KIN-62 AC: a finished task notifies the Owners, the project's admins and the task's author (while still a
// member), each linked user who wants task_finished once, in their language, with a Mini App link to the task.
func TestNotifyTaskReachesItsAudienceOnce(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := t.Context()
	f.svc.People = fakePeople{owners: []string{"owner"},
		chats: map[string]int64{"owner": 100, "admin": 200, "member": 300, "alien": 400, "gone": 500}}
	if _, err := f.svc.SavePreferences(ctx, member, "ru", nil); err != nil {
		t.Fatal(err)
	}
	ev := app.TaskFinished{TaskID: "T1", ProjectID: "p1", State: app.TaskFailed, Title: "Fix login", CreatedBy: "member"}
	if err := f.svc.NotifyTask(ctx, ev); err != nil {
		t.Fatal(err)
	}
	sent := f.out.sent(t)
	chats := map[int64]app.SendPayload{}
	for _, p := range sent {
		chats[p.ChatID] = p
	}
	if len(sent) != 3 || len(chats) != 3 || chats[100].Text == "" || chats[200].Text == "" {
		t.Fatalf("sent = %+v", sent)
	}
	ru := chats[300]
	if ru.StartParam != "task_T1" || ru.Button != "Открыть задачу" || !strings.Contains(ru.Text, "ошибкой") ||
		!strings.Contains(ru.Text, "Shop") || !strings.Contains(ru.Text, "Fix login") {
		t.Fatalf("member message = %+v", ru)
	}
	keys := []string{}
	for _, m := range f.out.msgs {
		keys = append(keys, m.Key)
	}
	slices.Sort(keys)
	if want := []string{"task/T1/FAILED/admin", "task/T1/FAILED/member", "task/T1/FAILED/owner"}; !slices.Equal(keys, want) {
		t.Fatalf("keys = %v", keys)
	}

	// An author who left the project (or a stranger named as author) learns nothing about it.
	f.out.msgs = nil
	if err := f.svc.NotifyTask(ctx, app.TaskFinished{TaskID: "T2", ProjectID: "p1", State: app.TaskCompleted,
		Title: "x", CreatedBy: "gone"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.out.sent(t) {
		if p.ChatID == 500 || p.ChatID == 400 {
			t.Fatalf("non-member notified: %+v", p)
		}
	}
	if got := len(f.out.msgs); got != 2 {
		t.Fatalf("messages = %d, want owner + admin", got)
	}
}

func TestNotifyTaskHonoursPreferencesAndValidates(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := t.Context()
	if _, err := f.svc.SavePreferences(ctx, owner, "en", []domain.NotificationType{domain.NotifyTaskFinished}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.NotifyTask(ctx, app.TaskFinished{TaskID: "T1", ProjectID: "p1", State: app.TaskCompleted,
		Title: "x", CreatedBy: "member"}); err != nil {
		t.Fatal(err)
	}
	sent := f.out.sent(t)
	if len(sent) != 1 || sent[0].ChatID != 300 || !strings.Contains(sent[0].Text, "completed") {
		t.Fatalf("sent = %+v (owner opted out; admin has no Telegram)", sent)
	}
	var ve *domain.ValidationError
	for _, bad := range []app.TaskFinished{{ProjectID: "p1", State: app.TaskFailed}, {TaskID: "T1", State: app.TaskFailed},
		{TaskID: "T1", ProjectID: "p1", State: "CANCELLED"}} {
		if err := f.svc.NotifyTask(ctx, bad); !errors.As(err, &ve) {
			t.Fatalf("%+v: err = %v", bad, err)
		}
	}
}
