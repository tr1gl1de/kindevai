package app_test

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// intakeStore is an in-memory app.IntakeStore that reads bindings from the shared store fake.
type intakeStore struct {
	mu       sync.Mutex
	base     *store
	tags     map[string]string
	cursors  map[string]int64
	polled   map[string]time.Time
	settings *domain.IntakeSettings
	links    map[string]domain.UserLink // lower-case login
	records  map[string]domain.IntakeRecord
	autoPlan map[string]bool
}

func newIntakeStore(base *store) *intakeStore {
	return &intakeStore{base: base, tags: map[string]string{}, cursors: map[string]int64{}, polled: map[string]time.Time{},
		links: map[string]domain.UserLink{}, records: map[string]domain.IntakeRecord{}, autoPlan: map[string]bool{}}
}

func (s *intakeStore) binding(projectID, short string) domain.Binding {
	b := domain.Binding{ProjectID: projectID, ShortName: short, TriggerTag: cmp.Or(s.tags[projectID], domain.DefaultTriggerTag),
		Cursor: s.cursors[projectID], AutoPlan: s.autoPlan[projectID]}
	if at, ok := s.polled[projectID]; ok {
		b.LastPolledAt = &at
	}
	return b
}

func (s *intakeStore) Bindings(context.Context) ([]domain.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Binding
	for p, sn := range s.base.bindings {
		out = append(out, s.binding(p, sn))
	}
	slices.SortFunc(out, func(a, b domain.Binding) int { return strings.Compare(a.ShortName, b.ShortName) })
	return out, nil
}

func (s *intakeStore) Binding(_ context.Context, projectID string) (domain.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, ok := s.base.bindings[projectID]
	if !ok {
		return domain.Binding{}, domain.ErrNotFound
	}
	return s.binding(projectID, sn), nil
}

func (s *intakeStore) BindingByShortName(_ context.Context, short string) (domain.Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, sn := range s.base.bindings {
		if sn == short {
			return s.binding(p, sn), nil
		}
	}
	return domain.Binding{}, domain.ErrNotFound
}

func (s *intakeStore) SetTriggerTag(_ context.Context, projectID, tag string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.base.bindings[projectID]; !ok {
		return domain.ErrNotFound
	}
	s.tags[projectID] = tag
	return nil
}

func (s *intakeStore) SetAutoPlan(_ context.Context, projectID string, on bool, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.base.bindings[projectID]; !ok {
		return domain.ErrNotFound
	}
	s.autoPlan[projectID] = on
	return nil
}

func (s *intakeStore) AdvanceCursor(_ context.Context, projectID, short string, cursor int64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base.bindings[projectID] == short {
		s.cursors[projectID] = max(s.cursors[projectID], cursor)
		s.polled[projectID] = at
	}
	return nil
}

func (s *intakeStore) IntakeSettings(context.Context) (domain.IntakeSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings == nil {
		return domain.DefaultIntakeSettings(), nil
	}
	return *s.settings, nil
}

func (s *intakeStore) SaveIntakeSettings(_ context.Context, v domain.IntakeSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = &v
	return nil
}

func (s *intakeStore) UserLinks(context.Context) ([]domain.UserLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []domain.UserLink{}
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range s.links {
			if !yield(k) {
				return
			}
		}
	}) {
		out = append(out, s.links[k])
	}
	return out, nil
}

func (s *intakeStore) LinkedUser(_ context.Context, login string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[strings.ToLower(login)]
	if !ok {
		return "", domain.ErrNotFound
	}
	return l.UserID, nil
}

func (s *intakeStore) LinkUser(_ context.Context, l domain.UserLink) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.links[strings.ToLower(l.YouTrackLogin)]; ok && o.UserID != l.UserID {
		return domain.ErrYouTrackLoginTaken
	}
	for k, o := range s.links {
		if o.UserID == l.UserID {
			delete(s.links, k)
		}
	}
	s.links[strings.ToLower(l.YouTrackLogin)] = l
	return nil
}

func (s *intakeStore) UnlinkUser(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, o := range s.links {
		if o.UserID == userID {
			delete(s.links, k)
		}
	}
	return nil
}

func (s *intakeStore) IntakeRecord(_ context.Context, projectID, issueID string) (domain.IntakeRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[projectID+"/"+issueID]
	return r, ok, nil
}

func (s *intakeStore) SaveIntakeRecord(_ context.Context, r domain.IntakeRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ProjectID+"/"+r.IssueID] = r
	return nil
}

func (s *intakeStore) RecentIntake(_ context.Context, projectID string, limit int) ([]domain.IntakeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.IntakeRecord
	for _, r := range s.records {
		if r.ProjectID == projectID {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b domain.IntakeRecord) int { return b.ProcessedAt.Compare(a.ProcessedAt) })
	return out[:min(limit, len(out))], nil
}

// tasks is an in-memory app.Tasks.
type tasks struct {
	mu     sync.Mutex
	titles map[string]string // task id -> title
	issues map[string]string // project/issue -> task id
	by     map[string]string // task id -> creator
	// planned lists AutoStartPlanning calls (task/requestedBy); outcome is their answer ("" = started).
	planned []string
	outcome string
}

func (t *tasks) AutoStartPlanning(_ context.Context, taskID, requestedBy string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.outcome != "" {
		return t.outcome, nil
	}
	t.planned = append(t.planned, taskID+"/"+requestedBy)
	return "started", nil
}

func (t *tasks) TaskForIssue(_ context.Context, projectID, issueID string) (string, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	id, ok := t.issues[projectID+"/"+issueID]
	return id, ok, nil
}

func (t *tasks) CreateIssueTask(_ context.Context, projectID, issueID, title, by string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.issues[projectID+"/"+issueID]; ok {
		return "", fmt.Errorf("duplicate task for %s", issueID)
	}
	id := fmt.Sprintf("task%d", len(t.titles)+1)
	t.titles[id], t.issues[projectID+"/"+issueID], t.by[id] = title, id, by
	return id, nil
}

func (t *tasks) RetitleTask(_ context.Context, id, title string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.titles[id] == title {
		return false, nil
	}
	t.titles[id] = title
	return true, nil
}

// access: user -> projects where the user may start AI ("<project>:admin": also the project's admin).
type access map[string][]string

func (a access) CanRunAI(_ context.Context, projectID, userID string) (bool, error) {
	return slices.Contains(a[userID], projectID) || slices.Contains(a[userID], projectID+":admin"), nil
}

func (a access) CanManageAI(_ context.Context, projectID, userID string) (bool, error) {
	return slices.Contains(a[userID], projectID+":admin"), nil
}

type users map[string]app.UserRef // by id

func (u users) ByLogin(_ context.Context, login string) (app.UserRef, error) {
	for _, r := range u {
		if r.Login == login {
			return r, nil
		}
	}
	return app.UserRef{}, domain.ErrNotFound
}

func (u users) ByIDs(_ context.Context, ids []string) (map[string]app.UserRef, error) {
	out := map[string]app.UserRef{}
	for _, id := range ids {
		if r, ok := u[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

// source is a scripted YouTrack: issues by ID, filtered like the real query (project and tag).
// Tag history: scripted per issue in history; without a script the reporter added the issue's tags when it was last
// updated.
type source struct {
	mu       sync.Mutex
	issues   map[string]domain.IssueSnapshot
	err      error
	queries  []app.IssueQuery
	reads    []string
	history  map[string][]domain.TagEvent
	tagErr   error
	tagReads []string
}

func (s *source) TagEvents(_ context.Context, _ string, _ domain.Secret, id string) ([]domain.TagEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tagReads = append(s.tagReads, id)
	if err := cmp.Or(s.tagErr, s.err); err != nil {
		return nil, err
	}
	if h, ok := s.history[id]; ok {
		return h, nil
	}
	is, ok := s.issues[id]
	if !ok {
		return nil, domain.ErrIssueNotFound
	}
	return []domain.TagEvent{{At: is.Updated, Author: is.Reporter, Added: is.Tags}}, nil
}

func (s *source) setHistory(id string, events ...domain.TagEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.history == nil {
		s.history = map[string][]domain.TagEvent{}
	}
	s.history[id] = events
}

func (s *source) UpdatedIssues(_ context.Context, _ string, token domain.Secret, q app.IssueQuery) ([]domain.IssueSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token.Reveal() == "" {
		return nil, fmt.Errorf("no token")
	}
	s.queries = append(s.queries, q)
	if s.err != nil {
		return nil, s.err
	}
	var all []domain.IssueSnapshot
	for _, is := range s.issues {
		if is.Project == q.ShortName && slices.ContainsFunc(is.Tags, func(t string) bool { return strings.EqualFold(t, q.Tag) }) {
			all = append(all, is)
		}
	}
	slices.SortFunc(all, func(a, b domain.IssueSnapshot) int {
		return cmp.Or(cmp.Compare(a.Updated, b.Updated), strings.Compare(a.ID, b.ID))
	})
	from := min(q.Skip, len(all))
	return all[from:min(from+q.Top, len(all))], nil
}

func (s *source) Issue(_ context.Context, _ string, _ domain.Secret, id string) (domain.IssueSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, id)
	if s.err != nil {
		return domain.IssueSnapshot{}, s.err
	}
	is, ok := s.issues[id]
	if !ok {
		return domain.IssueSnapshot{}, domain.ErrIssueNotFound
	}
	return is, nil
}

func (s *source) put(is domain.IssueSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issues[is.ID] = is
}

// intakeInbox records events with (source, delivery id) deduplication, like messaging.Inbox.
type intakeInbox struct {
	mu     sync.Mutex
	seen   map[string]bool
	events []messaging.InboundEvent
}

func (i *intakeInbox) Receive(_ context.Context, e messaging.InboundEvent) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	k := e.Source + "\x00" + e.DeliveryID
	if i.seen[k] {
		return false, nil
	}
	i.seen[k] = true
	i.events = append(i.events, e)
	return true, nil
}

// drain hands every pending event to the intake's inbox handlers.
func (e *intakeEnv) drain(t *testing.T) {
	t.Helper()
	for {
		e.in.mu.Lock()
		evs := e.in.events
		e.in.events = nil
		e.in.mu.Unlock()
		if len(evs) == 0 {
			return
		}
		for _, ev := range evs {
			var err error
			switch ev.Topic {
			case app.TopicYouTrackIssue:
				err = e.intake.HandleIssue(t.Context(), ev)
			case app.TopicYouTrackWebhook:
				err = e.intake.HandleWebhook(t.Context(), ev)
			default:
				t.Fatalf("unexpected topic %s", ev.Topic)
			}
			if err != nil {
				t.Fatalf("handling %s %s: %v", ev.Topic, ev.DeliveryID, err)
			}
		}
	}
}

// deliver runs the outbox fetches enqueued so far, returning their errors.
func (e *intakeEnv) deliver(t *testing.T) []error {
	t.Helper()
	e.out.mu.Lock()
	msgs := e.out.msgs
	e.out.msgs = nil
	e.out.mu.Unlock()
	var errs []error
	for _, m := range msgs {
		if m.Topic != app.TopicYouTrackFetch {
			continue
		}
		errs = append(errs, e.intake.HandleFetch(t.Context(), messaging.Delivery{Adapter: m.Adapter, Topic: m.Topic,
			Key: m.Key, Payload: m.Payload, Attempt: 1}))
	}
	return errs
}

type health struct {
	mu        sync.Mutex
	open      bool
	successes int
	failures  []string
}

func (h *health) Allow(string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.open
}

func (h *health) Success(string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.successes++
}

func (h *health) Failure(_ string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures = append(h.failures, err.Error())
}

func (h *health) Status(a string) messaging.AdapterStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := messaging.StateOK
	if len(h.failures) > 0 {
		st = messaging.StateDegraded
	}
	return messaging.AdapterStatus{Adapter: a, State: st}
}

type intakeEnv struct {
	*env
	intake *app.Intake
	istore *intakeStore
	tasks  *tasks
	src    *source
	in     *intakeInbox
	health *health
}

const (
	ytToken = "perm:cm9ib3Q=.s3cr3tT0ken.Xy12AbCd"
	alice   = "alice1"
)

// newIntakeEnv: a configured connection, project p1 bound to KIN, alice may run AI in p1, bob in nothing.
func newIntakeEnv(t *testing.T) *intakeEnv {
	t.Helper()
	e := &intakeEnv{env: newEnv(t)}
	e.istore = newIntakeStore(e.store)
	e.tasks = &tasks{titles: map[string]string{}, issues: map[string]string{}, by: map[string]string{}}
	e.src = &source{issues: map[string]domain.IssueSnapshot{}}
	e.in = &intakeInbox{seen: map[string]bool{}}
	e.health = &health{}
	e.intake = &app.Intake{
		Integrations: e.svc,
		Store:        e.istore,
		Tasks:        e.tasks,
		Access:       access{alice: {"p1"}, "adm1": {"p1:admin"}, "owner1": {"p1", "p2"}},
		Users:        users{alice: {ID: alice, Login: "alice"}, "bob1": {ID: "bob1", Login: "bob"}, "owner1": {ID: "owner1", Login: "owner"}},
		Source:       e.src,
		Inbox:        e.in,
		Health:       e.health,
		PageSize:     2,
	}
	if _, err := e.svc.ConfigureYouTrack(t.Context(), owner, "https://yt.example.com", ytToken); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.BindYouTrackProject(t.Context(), owner, "p1", "KIN"); err != nil {
		t.Fatal(err)
	}
	e.istore.links["jdoe"] = domain.UserLink{YouTrackLogin: "jdoe", UserID: alice}
	e.istore.links["bobyt"] = domain.UserLink{YouTrackLogin: "bobyt", UserID: "bob1"}
	return e
}
