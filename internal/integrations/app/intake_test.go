package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

var alicePrincipal = auth.Principal{UserID: alice, Login: "alice"}

func issue(id string, updated int64, reporter string, tags ...string) domain.IssueSnapshot {
	if tags == nil {
		tags = []string{"kindevai"}
	}
	return domain.IssueSnapshot{ID: id, Project: strings.Split(id, "-")[0], Summary: "Summary of " + id, Updated: updated,
		Reporter: reporter, Tags: tags}
}

func (e *intakeEnv) poll(t *testing.T) {
	t.Helper()
	if err := e.intake.PollOnce(t.Context()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	e.drain(t)
}

func (e *intakeEnv) record(t *testing.T, project, id string) domain.IntakeRecord {
	t.Helper()
	r, ok, _ := e.istore.IntakeRecord(t.Context(), project, id)
	if !ok {
		t.Fatalf("no intake record for %s", id)
	}
	return r
}

// AC: polling by the `updated` cursor finds new and changed issues without webhooks.
func TestPollingCreatesTasksForTaggedIssuesWithoutAWebhook(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.src.put(issue("KIN-1", 100, "jdoe"))
	e.src.put(issue("KIN-2", 200, "jdoe"))
	e.src.put(issue("KIN-3", 300, "jdoe"))            // third page item: PageSize is 2
	e.src.put(issue("KIN-4", 400, "jdoe", "backend")) // not tagged
	e.src.put(issue("OTHER-1", 500, "jdoe"))          // another YouTrack project
	e.poll(t)
	if len(e.tasks.titles) != 3 || e.tasks.titles["task1"] != "KIN-1: Summary of KIN-1" || e.tasks.by["task1"] != alice {
		t.Fatalf("tasks = %+v by %+v", e.tasks.titles, e.tasks.by)
	}
	b, _ := e.istore.Binding(t.Context(), "p1")
	if b.Cursor != 300 || b.LastPolledAt == nil || e.health.successes != 1 {
		t.Fatalf("binding %+v, successes %d", b, e.health.successes)
	}
	if r := e.record(t, "p1", "KIN-2"); r.Outcome != domain.OutcomeCreated || r.Initiator != alice || r.Channel != domain.ChannelPoll {
		t.Fatalf("record %+v", r)
	}
	if !slices.Contains(e.audit.kinds(), app.KindYouTrackTaskCreated) {
		t.Fatalf("audit %v", e.audit.kinds())
	}

	// Next poll starts from the cursor; a changed summary follows into the task, unchanged issues stay as they are.
	e.now = t0.Add(time.Minute)
	changed := issue("KIN-2", 600, "jdoe")
	changed.Summary = "Renamed"
	e.src.put(changed)
	e.poll(t)
	if q := e.src.queries[len(e.src.queries)-1]; q.Since != 300 || q.Tag != "kindevai" || q.ShortName != "KIN" {
		t.Fatalf("query %+v", q)
	}
	if len(e.tasks.titles) != 3 || e.tasks.titles["task2"] != "KIN-2: Renamed" {
		t.Fatalf("tasks after change %+v", e.tasks.titles)
	}
	if r := e.record(t, "p1", "KIN-2"); r.Outcome != domain.OutcomeUpdated || r.IssueUpdated != 600 || r.Initiator != alice {
		t.Fatalf("record after change %+v", r)
	}
}

// AC: duplicates and out-of-order events are harmless.
func TestDuplicateAndOutOfOrderSnapshotsAreHarmless(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	newer := issue("KIN-1", 200, "jdoe")
	newer.Summary = "New"
	older := issue("KIN-1", 100, "jdoe")
	older.Summary = "Old"
	for _, s := range []domain.IssueSnapshot{newer, newer, older} {
		p, _ := json.Marshal(map[string]any{"issue": s, "channel": "webhook", "taggedBy": "jdoe", "taggedTag": "kindevai"})
		if err := e.intake.HandleIssue(t.Context(), messaging.InboundEvent{Topic: app.TopicYouTrackIssue, Payload: p}); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.tasks.titles) != 1 || e.tasks.titles["task1"] != "KIN-1: New" {
		t.Fatalf("tasks %+v", e.tasks.titles)
	}
	if r := e.record(t, "p1", "KIN-1"); r.IssueUpdated != 200 {
		t.Fatalf("an older state overwrote the newer one: %+v", r)
	}
	// Polling the same state twice delivers it to the inbox once.
	e.src.put(issue("KIN-5", 300, "jdoe"))
	e.poll(t)
	n := len(e.in.seen)
	e.poll(t)
	if len(e.in.seen) != n {
		t.Fatalf("a re-polled issue was delivered again")
	}
}

// AC: the initiator is mapped to a platform user; without the right to start AI no task is created and the reason
// is recorded for the UI. The fake's tag history says the reporter applied the tag; since KIN-50 the applier counts.
func TestIssuesFromUnlinkedOrUnauthorisedTaggersAreRejectedWithAReason(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.src.put(issue("KIN-1", 100, "stranger"))
	e.src.put(issue("KIN-2", 200, "bobyt")) // linked, but bob may not start AI in p1
	e.src.put(issue("KIN-3", 300, ""))      // no reporter
	e.poll(t)
	if len(e.tasks.titles) != 0 {
		t.Fatalf("tasks created: %+v", e.tasks.titles)
	}
	for id, reason := range map[string]string{"KIN-1": domain.ReasonTagNotAllowed, "KIN-2": domain.ReasonTagNotAllowed, "KIN-3": domain.ReasonTagNotAllowed} {
		if r := e.record(t, "p1", id); r.Outcome != domain.OutcomeRejected || r.Reason != reason {
			t.Errorf("%s: %+v", id, r)
		}
	}
	if !slices.Contains(e.audit.kinds(), app.KindYouTrackIssueRejected) {
		t.Fatalf("audit %v", e.audit.kinds())
	}
	v, err := e.intake.ProjectIntake(t.Context(), alicePrincipal, "p1")
	if err != nil || len(v.Recent) != 3 || v.Recent[0].Reason == "" {
		t.Fatalf("view %+v, %v", v, err)
	}
	// Once the Owner links the account, the next change of the issue creates the task.
	if _, err := e.intake.LinkUser(t.Context(), owner, "alice", "stranger"); err != nil {
		t.Fatal(err)
	}
	e.src.put(issue("KIN-1", 400, "stranger"))
	e.poll(t)
	if r := e.record(t, "p1", "KIN-1"); r.Outcome != domain.OutcomeCreated || r.Initiator != alice {
		t.Fatalf("after linking %+v", r)
	}
}

func TestResolvedIssuesDoNotStartNewWork(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	s := issue("KIN-1", 100, "jdoe")
	s.Resolved = true
	e.src.put(s)
	e.poll(t)
	if len(e.tasks.titles) != 0 {
		t.Fatalf("task for a resolved issue: %+v", e.tasks.titles)
	}
}

// AC: the optional webhook checks the secret, stores the hint in the inbox and re-reads the issue through REST.
func TestWebhookIsVerifiedAndOnlyNamesTheIssue(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	ctx := t.Context()
	body := []byte(`{"issueId":"KIN-9","updated":900,"summary":"forged","reporter":"owner","tags":["kindevai"]}`)
	if err := e.intake.ReceiveWebhook(ctx, "anything", body); !errors.Is(err, domain.ErrWebhookDisabled) {
		t.Fatalf("without a secret: %v", err)
	}
	secret := strings.Repeat("s", 40)
	if err := e.intake.SetWebhookSecret(ctx, bob, secret); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	if err := e.intake.SetWebhookSecret(ctx, owner, secret); err != nil {
		t.Fatal(err)
	}
	if err := e.intake.ReceiveWebhook(ctx, "wrong", body); !errors.Is(err, domain.ErrWebhookUnauthorized) {
		t.Fatalf("bad secret: %v", err)
	}
	var ve *domain.ValidationError
	if err := e.intake.ReceiveWebhook(ctx, secret, []byte(`{"issueId":"../x"}`)); !errors.As(err, &ve) {
		t.Fatalf("bad id: %v", err)
	}
	for range 2 { // a retried call is deduplicated by the inbox
		if err := e.intake.ReceiveWebhook(ctx, secret, body); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.in.events) != 1 || strings.Contains(string(e.in.events[0].Payload), "forged") {
		t.Fatalf("inbox %+v", e.in.events)
	}
	e.drain(t)
	// YouTrack's authoritative state decides: the real reporter is jdoe (alice), the summary is the real one.
	e.src.put(issue("KIN-9", 950, "jdoe"))
	if errs := e.deliver(t); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("fetch: %v", errs)
	}
	e.drain(t)
	if e.tasks.titles["task1"] != "KIN-9: Summary of KIN-9" || e.tasks.by["task1"] != alice || !slices.Equal(e.src.reads, []string{"KIN-9"}) {
		t.Fatalf("tasks %+v by %+v reads %v", e.tasks.titles, e.tasks.by, e.src.reads)
	}
	if r := e.record(t, "p1", "KIN-9"); r.Channel != domain.ChannelWebhook {
		t.Fatalf("record %+v", r)
	}
	// The same state seen by the poller afterwards is the same inbox delivery.
	n := len(e.in.seen)
	e.poll(t)
	if len(e.in.seen) != n || len(e.tasks.titles) != 1 {
		t.Fatalf("poll after webhook: seen %d -> %d, tasks %+v", n, len(e.in.seen), e.tasks.titles)
	}
}

// AC: "run by issue ID" for project members.
func TestRunByIssueIDForProjectMembers(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	ctx := t.Context()
	s := issue("KIN-5", 500, "stranger", "backend") // untagged, reporter unknown: the member is the initiator
	e.src.put(s)
	if _, err := e.intake.RunIssue(ctx, bob, "p1", "KIN-5"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-member: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := e.intake.RunIssue(ctx, alicePrincipal, "p1", "OTHER-5"); !errors.As(err, &ve) || ve.Code != "issue_project" {
		t.Fatalf("other project: %v", err)
	}
	if _, err := e.intake.RunIssue(ctx, owner, "p2", "KIN-5"); !errors.Is(err, domain.ErrNotBound) {
		t.Fatalf("unbound project: %v", err)
	}
	rec, err := e.intake.RunIssue(ctx, alicePrincipal, "p1", "kin-5")
	if err != nil || rec.Outcome != domain.OutcomeQueued || rec.IssueID != "KIN-5" {
		t.Fatalf("run: %+v, %v", rec, err)
	}
	if !slices.Contains(e.audit.kinds(), app.KindYouTrackRunRequested) {
		t.Fatalf("audit %v", e.audit.kinds())
	}
	if errs := e.deliver(t); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("fetch: %v", errs)
	}
	e.drain(t)
	if r := e.record(t, "p1", "KIN-5"); r.Outcome != domain.OutcomeCreated || r.Initiator != alice || r.Channel != domain.ChannelManual {
		t.Fatalf("record %+v", r)
	}
	// A missing issue is reported back as a rejection.
	if _, err := e.intake.RunIssue(ctx, alicePrincipal, "p1", "KIN-404"); err != nil {
		t.Fatal(err)
	}
	if errs := e.deliver(t); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("fetch missing: %v", errs)
	}
	if r := e.record(t, "p1", "KIN-404"); r.Outcome != domain.OutcomeRejected || r.Reason != domain.ReasonIssueNotFound {
		t.Fatalf("missing issue %+v", r)
	}
	// The permission is checked again when the issue arrives: a member removed meanwhile gets no task.
	e.intake.Access = access{}
	e.src.put(issue("KIN-6", 600, "jdoe"))
	e.intake.Access = access{alice: {"p1"}}
	if _, err := e.intake.RunIssue(ctx, alicePrincipal, "p1", "KIN-6"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t)
	e.intake.Access = access{}
	e.drain(t)
	if r := e.record(t, "p1", "KIN-6"); r.Outcome != domain.OutcomeRejected || r.Reason != domain.ReasonNotAllowed {
		t.Fatalf("revoked member %+v", r)
	}
}

// AC: YouTrack being unavailable does not block the UI, and the integration status is visible.
func TestYouTrackOutageIsReportedAndDoesNotBlockViews(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.src.err = &app.StatusError{Status: 503, RetryAfter: 30 * time.Minute}
	if err := e.intake.PollOnce(t.Context()); err == nil {
		t.Fatal("poll error swallowed")
	}
	if len(e.health.failures) != 1 {
		t.Fatalf("health failures %v", e.health.failures)
	}
	if b, _ := e.istore.Binding(t.Context(), "p1"); b.LastPolledAt != nil {
		t.Fatalf("a failed poll counted as done: %+v", b)
	}
	v, err := e.intake.ProjectIntake(t.Context(), alicePrincipal, "p1")
	if err != nil || !v.Connected || v.Binding == nil || v.Status.State != messaging.StateDegraded {
		t.Fatalf("view %+v, %v", v, err)
	}
	if _, err := e.intake.ProjectIntake(t.Context(), bob, "p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("non-member view: %v", err)
	}
	// While the circuit is open nothing is called.
	e.health.open = true
	n := len(e.src.queries)
	if err := e.intake.PollOnce(t.Context()); err != nil || len(e.src.queries) != n {
		t.Fatalf("polled with an open circuit: %v", err)
	}
	// A run by ID that YouTrack refuses for good is reported to the member.
	e.src.err = &app.StatusError{Status: 403}
	if _, err := e.intake.RunIssue(t.Context(), alicePrincipal, "p1", "KIN-1"); err != nil {
		t.Fatal(err)
	}
	if errs := e.deliver(t); len(errs) != 1 || errs[0] == nil {
		t.Fatalf("permanent failure not returned: %v", errs)
	}
	if r := e.record(t, "p1", "KIN-1"); r.Outcome != domain.OutcomeRejected || r.Reason != domain.ReasonUnavailable {
		t.Fatalf("record %+v", r)
	}
}

func TestNextPollBacksOff(t *testing.T) {
	t.Parallel()
	m := time.Minute
	for _, c := range []struct {
		failures int
		err      error
		want     time.Duration
	}{
		{1, errors.New("x"), 2 * m},
		{3, errors.New("x"), 8 * m},
		{10, errors.New("x"), 15 * m},
		{1, &app.StatusError{Status: 429, RetryAfter: 20 * m}, 20 * m},
	} {
		if got := app.NextPoll(m, c.failures, c.err); got != c.want {
			t.Errorf("NextPoll(1m, %d, %v) = %v, want %v", c.failures, c.err, got, c.want)
		}
	}
	if got := app.NextPoll(time.Hour, 3, errors.New("x")); got != time.Hour {
		t.Errorf("long interval backoff = %v", got)
	}
}

func TestOwnerIntakeSettings(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	ctx := t.Context()
	if err := e.intake.SetPollInterval(ctx, bob, 120); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("non-owner: %v", err)
	}
	var ve *domain.ValidationError
	if err := e.intake.SetPollInterval(ctx, owner, 5); !errors.As(err, &ve) {
		t.Fatalf("too short: %v", err)
	}
	if err := e.intake.SetPollInterval(ctx, owner, 120); err != nil {
		t.Fatal(err)
	}
	if _, err := e.intake.LinkUser(ctx, owner, "nobody", "x"); !errors.As(err, &ve) || ve.Code != "user_not_found" {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := e.intake.LinkUser(ctx, bob, "bob", "bob.yt"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("self-linking: %v", err)
	}
	b, err := e.intake.SetTriggerTag(ctx, owner, "p1", "ai run")
	if err != nil || b.TriggerTag != "ai run" {
		t.Fatalf("tag %+v, %v", b, err)
	}
	if _, err := e.intake.SetTriggerTag(ctx, owner, "p2", "x"); !errors.Is(err, domain.ErrNotBound) {
		t.Fatalf("unbound: %v", err)
	}
	o, err := e.intake.IntakeOverview(ctx, owner)
	if err != nil || o.Settings.PollInterval != 2*time.Minute || len(o.Links) != 2 || o.Links[0].Login == "" || o.WebhookSecret.Set {
		t.Fatalf("overview %+v, %v", o, err)
	}
	if err := e.intake.UnlinkUser(ctx, owner, "bob1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.audit.dump(), ytToken) {
		t.Fatal("token in audit")
	}
}

// Review finding: a broken binding (e.g. its YouTrack project was deleted) must not stop the others.
func TestAFailingBindingDoesNotBlockTheOthers(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	if _, err := e.svc.BindYouTrackProject(t.Context(), owner, "p2", "AAA"); err != nil { // polled first
		t.Fatal(err)
	}
	e.intake.Source = &failingFor{source: e.src, shortName: "AAA"}
	e.src.put(issue("KIN-1", 100, "jdoe"))
	e.poll(t)
	if len(e.tasks.titles) != 1 || len(e.health.failures) != 0 || e.health.successes != 1 {
		t.Fatalf("tasks %+v, failures %v", e.tasks.titles, e.health.failures)
	}
}

type failingFor struct {
	*source
	shortName string
}

func (f *failingFor) UpdatedIssues(ctx context.Context, base string, tok domain.Secret, q app.IssueQuery) ([]domain.IssueSnapshot, error) {
	if q.ShortName == f.shortName {
		return nil, &app.StatusError{Status: 400}
	}
	return f.source.UpdatedIssues(ctx, base, tok, q)
}

// Review finding: more tagged issues in the coarse date window than one cycle reads must not stall the cursor.
func TestThePollCursorDoesNotStallAtThePageCap(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.intake.MaxPages = 1 // two issues per cycle; the fake ignores the date window like a coarse query would
	for i := range 5 {
		e.src.put(issue(fmt.Sprintf("KIN-%d", i+1), int64(100*(i+1)), "jdoe"))
	}
	for range 10 {
		e.poll(t)
	}
	if len(e.tasks.titles) != 5 {
		t.Fatalf("tasks after 10 cycles: %+v", e.tasks.titles)
	}
	if b, _ := e.istore.Binding(t.Context(), "p1"); b.Cursor != 500 {
		t.Fatalf("cursor %d", b.Cursor)
	}
}

// Review finding: a failed run by ID must not hide the task an issue already has.
func TestAFailedRunByIDKeepsTheTaskLink(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.src.put(issue("KIN-1", 100, "jdoe"))
	e.poll(t)
	e.src.err = &app.StatusError{Status: 403}
	if _, err := e.intake.RunIssue(t.Context(), alicePrincipal, "p1", "KIN-1"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t)
	if r := e.record(t, "p1", "KIN-1"); r.TaskID == "" || r.Outcome == domain.OutcomeQueued {
		t.Fatalf("task link lost or run never settled: %+v", r)
	}
	if !slices.Contains(e.audit.kinds(), app.KindYouTrackIssueRejected) {
		t.Fatal("failure not audited")
	}
}
