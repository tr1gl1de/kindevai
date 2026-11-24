package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// KIN-50: whoever applies the trigger tag starts the work, so that user — not the reporter — must be linked to an
// account that may start AI in the project.

func tagged(at int64, author string, tags ...string) domain.TagEvent {
	if tags == nil {
		tags = []string{"kindevai"}
	}
	return domain.TagEvent{At: at, Author: author, Added: tags}
}

func untagged(at int64, author string) domain.TagEvent {
	return domain.TagEvent{At: at, Author: author, Removed: []string{"kindevai"}}
}

func (e *intakeEnv) rejectedAudit(t *testing.T, issueID string) map[string]string {
	t.Helper()
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	for i := len(e.audit.entries) - 1; i >= 0; i-- {
		if en := e.audit.entries[i]; en.Kind == app.KindYouTrackIssueRejected && en.Details["issue"] == issueID {
			return en.Details
		}
	}
	t.Fatalf("no rejection audited for %s", issueID)
	return nil
}

// AC: the tag's applier is read from the issue's tag activity and becomes the initiator.
func TestTheUserWhoAppliedTheTagIsTheInitiator(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	e.src.put(issue("KIN-1", 100, "stranger")) // reported by an outsider, tagged by jdoe (alice)
	e.src.setHistory("KIN-1", tagged(90, "jdoe"))
	e.poll(t)
	if r := e.record(t, "p1", "KIN-1"); r.Outcome != domain.OutcomeCreated || r.Initiator != alice || e.tasks.by["task1"] != alice {
		t.Fatalf("record %+v, tasks by %+v", r, e.tasks.by)
	}
	if len(e.src.tagReads) != 1 || e.src.tagReads[0] != "KIN-1" {
		t.Fatalf("tag reads %v", e.src.tagReads)
	}
}

// AC: no task unless the applier is linked to an account allowed to start AI; the reason is tag_not_allowed. The
// reporter being allowed does not help: tagging somebody else's issue is the spoofing attempt.
func TestSpoofedTagApplicationsAreRejected(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		history   []domain.TagEvent
		initiator string // linked account of the applier, if any
		tagger    string
	}{
		"tagged by an unlinked user":    {[]domain.TagEvent{tagged(90, "stranger")}, "", "stranger"},
		"tagged by a linked non-member": {[]domain.TagEvent{tagged(90, "bobyt")}, "bob1", "bobyt"},
		"removed by the reporter and re-added by an outsider": {
			[]domain.TagEvent{tagged(50, "jdoe"), untagged(60, "jdoe"), tagged(70, "stranger")}, "", "stranger"},
		"the reporter only added a look-alike tag": {
			[]domain.TagEvent{tagged(50, "stranger"), tagged(60, "jdoe", "kindevai2", "kin devai")}, "", "stranger"},
		"two appliers at one instant":  {[]domain.TagEvent{tagged(50, "jdoe"), tagged(50, "stranger")}, "", ""},
		"no tag history":               {[]domain.TagEvent{}, "", ""},
		"history without an author":    {[]domain.TagEvent{tagged(50, "")}, "", ""},
		"history says the tag is gone": {[]domain.TagEvent{tagged(50, "jdoe"), untagged(60, "jdoe")}, "", ""},
	}
	e := newIntakeEnv(t)
	n := 0
	ids := map[string]string{}
	for name, c := range cases {
		n++
		id := fmt.Sprintf("KIN-%d", n)
		ids[name] = id
		e.src.put(issue(id, int64(100+n), "jdoe")) // the reporter alone would be allowed
		e.src.setHistory(id, c.history...)
	}
	e.poll(t)
	if len(e.tasks.titles) != 0 {
		t.Fatalf("tasks created: %+v", e.tasks.titles)
	}
	for name, c := range cases {
		id := ids[name]
		r := e.record(t, "p1", id)
		if r.Outcome != domain.OutcomeRejected || r.Reason != domain.ReasonTagNotAllowed || r.Initiator != c.initiator || r.Reporter != "jdoe" {
			t.Errorf("%s: %+v", name, r)
		}
		if d := e.rejectedAudit(t, id); d["reason"] != domain.ReasonTagNotAllowed || d["tagger"] != c.tagger {
			t.Errorf("%s: audit %v", name, d)
		}
	}
	// The member who may start AI tags the issue himself: the next change creates the task.
	id := ids["tagged by an unlinked user"]
	e.src.put(issue(id, 500, "jdoe"))
	e.src.setHistory(id, tagged(90, "stranger"), untagged(400, "jdoe"), tagged(450, "jdoe"))
	e.poll(t)
	if r := e.record(t, "p1", id); r.Outcome != domain.OutcomeCreated || r.Initiator != alice {
		t.Fatalf("after the member tagged it: %+v", r)
	}
}

// Fail closed: a tag history that cannot be relied on rejects the issue; a transient failure or a refused token
// retries (the cursor stays, the poll fails) instead of rejecting tagged issues for good.
func TestUnreadableTagHistoryFailsClosed(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"unusable":  fmt.Errorf("%w: malformed", app.ErrTagHistoryUnusable),
		"not found": domain.ErrIssueNotFound,
		"gone":      &app.StatusError{Status: 410},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newIntakeEnv(t)
			e.src.put(issue("KIN-1", 100, "jdoe"))
			e.src.tagErr = err
			e.poll(t)
			if r := e.record(t, "p1", "KIN-1"); r.Outcome != domain.OutcomeRejected || r.Reason != domain.ReasonTagNotAllowed {
				t.Fatalf("record %+v", r)
			}
			if b, _ := e.istore.Binding(t.Context(), "p1"); b.Cursor != 100 {
				t.Fatalf("a permanently unreadable history stalled the cursor: %+v", b)
			}
		})
	}
	e := newIntakeEnv(t)
	e.src.put(issue("KIN-1", 100, "jdoe"))
	for _, status := range []int{503, 429, 403, 401} {
		e.src.tagErr = &app.StatusError{Status: status}
		if err := e.intake.PollOnce(t.Context()); err == nil {
			t.Fatalf("HTTP %d from the tag history was swallowed", status)
		}
		e.drain(t)
	}
	if _, seen, _ := e.istore.IntakeRecord(t.Context(), "p1", "KIN-1"); seen || len(e.tasks.titles) != 0 {
		t.Fatal("an issue was decided without its tag history")
	}
	if b, _ := e.istore.Binding(t.Context(), "p1"); b.Cursor != 0 {
		t.Fatalf("cursor moved past an undecided issue: %+v", b)
	}
	e.src.tagErr = nil
	e.poll(t)
	if r := e.record(t, "p1", "KIN-1"); r.Outcome != domain.OutcomeCreated {
		t.Fatalf("after recovery %+v", r)
	}
}

// Only untagged-yet or open issues without a task need the history: existing tasks follow their issue without it,
// and runs by ID keep the member as the initiator.
func TestTagHistoryIsReadOnlyWhenItDecides(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	s := issue("KIN-1", 100, "jdoe")
	s.Resolved = true
	e.src.put(s)
	e.src.put(issue("KIN-2", 200, "stranger", "backend")) // untagged: runs by ID only
	e.poll(t)
	if len(e.src.tagReads) != 0 {
		t.Fatalf("tag history read for %v", e.src.tagReads)
	}
	if _, err := e.intake.RunIssue(t.Context(), alicePrincipal, "p1", "KIN-2"); err != nil {
		t.Fatal(err)
	}
	e.deliver(t)
	e.drain(t)
	if r := e.record(t, "p1", "KIN-2"); r.Outcome != domain.OutcomeCreated || r.Initiator != alice || len(e.src.tagReads) != 0 {
		t.Fatalf("run by ID %+v, tag reads %v", r, e.src.tagReads)
	}
}

// The webhook names an issue only: what it claims about the tag's applier is ignored, the activity log decides.
func TestWebhookCannotClaimTheTagApplier(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	secret := strings.Repeat("s", 40)
	if err := e.intake.SetWebhookSecret(t.Context(), owner, secret); err != nil {
		t.Fatal(err)
	}
	e.src.put(issue("KIN-9", 900, "jdoe"))
	e.src.setHistory("KIN-9", tagged(800, "stranger"))
	body := []byte(`{"issueId":"KIN-9","updated":900,"taggedBy":"jdoe","taggedTag":"kindevai","author":{"login":"jdoe"}}`)
	if err := e.intake.ReceiveWebhook(t.Context(), secret, body); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	e.src.tagErr = &app.StatusError{Status: 502}
	if errs := e.deliver(t); len(errs) != 1 || errs[0] == nil {
		t.Fatalf("a transient failure must be retried by the outbox: %v", errs)
	}
	e.drain(t)
	if _, seen, _ := e.istore.IntakeRecord(t.Context(), "p1", "KIN-9"); seen {
		t.Fatal("decided without the tag history")
	}
	e.src.tagErr = nil
	if err := e.intake.ReceiveWebhook(t.Context(), secret, []byte(`{"issueId":"KIN-9","updated":901}`)); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if errs := e.deliver(t); len(errs) != 1 || errs[0] != nil {
		t.Fatalf("fetch: %v", errs)
	}
	e.drain(t)
	if r := e.record(t, "p1", "KIN-9"); r.Outcome != domain.OutcomeRejected || r.Reason != domain.ReasonTagNotAllowed || len(e.tasks.titles) != 0 {
		t.Fatalf("record %+v", r)
	}
}

// Inbox events without a verified applier for the current trigger tag (older events, a tag changed meanwhile) are
// rejected rather than falling back to the reporter.
func TestIssueEventsNeedAnApplierOfTheCurrentTriggerTag(t *testing.T) {
	t.Parallel()
	e := newIntakeEnv(t)
	for id, extra := range map[string]map[string]any{
		"KIN-1": {},                                             // queued before the upgrade: no applier
		"KIN-2": {"taggedBy": "jdoe", "taggedTag": "ai-run"},    // the trigger tag changed meanwhile
		"KIN-3": {"taggedBy": "jdoe", "taggedTag": "KINDEVAI "}, // the current tag
	} {
		p := map[string]any{"issue": issue(id, 100, "jdoe"), "channel": domain.ChannelPoll}
		for k, v := range extra {
			p[k] = v
		}
		body, _ := json.Marshal(p)
		if err := e.intake.HandleIssue(t.Context(), messaging.InboundEvent{Topic: app.TopicYouTrackIssue, Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]domain.Outcome{"KIN-1": domain.OutcomeRejected, "KIN-2": domain.OutcomeRejected, "KIN-3": domain.OutcomeCreated} {
		if r := e.record(t, "p1", id); r.Outcome != want || (want == domain.OutcomeRejected && r.Reason != domain.ReasonTagNotAllowed) {
			t.Errorf("%s: %+v", id, r)
		}
	}
}

// The whole path against an HTTP fake of YouTrack: the real client parses the activity log, a spoofed tag on an
// allowed reporter's issue is rejected and the allowed member's own tag creates the task.
func TestSpoofingIsRejectedThroughTheYouTrackClient(t *testing.T) {
	t.Parallel()
	issueBody := func(id string, updated int) string {
		return fmt.Sprintf(`{"idReadable":%q,"summary":"S","updated":%d,"resolved":null,"reporter":{"login":"jdoe"},
			"tags":[{"name":"kindevai"}],"project":{"shortName":"KIN"}}`, id, updated)
	}
	activities := map[string]string{
		"KIN-1": `[{"timestamp":90,"author":{"login":"stranger"},"added":[{"name":"kindevai"}],"removed":[]}]`,
		"KIN-2": `[{"timestamp":95,"author":{"login":"jdoe"},"added":[{"name":"kindevai"}],"removed":[]},
			{"timestamp":80,"author":{"login":"stranger"},"added":[{"name":"kindevai"}],"removed":[]},
			{"timestamp":85,"author":{"login":"stranger"},"added":[],"removed":[{"name":"kindevai"}]}]`,
		"KIN-3": `{"error":"unexpected"}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/issues", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[" + issueBody("KIN-1", 100) + "," + issueBody("KIN-2", 200) + "," + issueBody("KIN-3", 300) + "]"))
	})
	mux.HandleFunc("GET /api/issues/{id}/activities", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("categories") != "TagsCategory" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(activities[r.PathValue("id")]))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := newIntakeEnv(t)
	if _, err := e.svc.ConfigureYouTrack(t.Context(), owner, srv.URL, ytToken); err != nil {
		t.Fatal(err)
	}
	e.intake.Source = &youtrack.Client{HTTP: srv.Client()}
	e.intake.PageSize = 50
	e.poll(t)
	for id, want := range map[string]domain.Outcome{"KIN-1": domain.OutcomeRejected, "KIN-2": domain.OutcomeCreated, "KIN-3": domain.OutcomeRejected} {
		if r := e.record(t, "p1", id); r.Outcome != want {
			t.Errorf("%s: %+v", id, r)
		}
	}
	if len(e.tasks.titles) != 1 || e.tasks.by["task1"] != alice {
		t.Fatalf("tasks %+v by %+v", e.tasks.titles, e.tasks.by)
	}
}
