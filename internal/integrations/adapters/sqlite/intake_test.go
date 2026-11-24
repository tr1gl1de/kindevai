package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func TestBindingCursorAndTriggerTag(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	if _, err := r.Binding(ctx, "p1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unbound: %v", err)
	}
	if err := r.SetTriggerTag(ctx, "p1", "ai", t0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("tag on unbound: %v", err)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "KIN", t0); err != nil {
		t.Fatal(err)
	}
	b, err := r.Binding(ctx, "p1")
	if err != nil || b != (domain.Binding{ProjectID: "p1", ShortName: "KIN", TriggerTag: domain.DefaultTriggerTag}) {
		t.Fatalf("binding %+v, %v", b, err)
	}
	later := t0.Add(time.Minute)
	if err := r.AdvanceCursor(ctx, "p1", "KIN", 500, later); err != nil {
		t.Fatal(err)
	}
	if err := r.AdvanceCursor(ctx, "p1", "KIN", 100, later); err != nil { // never moves back
		t.Fatal(err)
	}
	if err := r.AdvanceCursor(ctx, "p1", "OLD", 900, later); err != nil { // a poll of a replaced binding is dropped
		t.Fatal(err)
	}
	if err := r.SetTriggerTag(ctx, "p1", "ai run", later); err != nil {
		t.Fatal(err)
	}
	b, err = r.BindingByShortName(ctx, "KIN")
	if err != nil || b.Cursor != 500 || b.TriggerTag != "ai run" || b.LastPolledAt == nil || !b.LastPolledAt.Equal(later) {
		t.Fatalf("after poll %+v, %v", b, err)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "KIN", later); err != nil { // re-saving the same binding keeps the cursor
		t.Fatal(err)
	}
	if b, _ := r.Binding(ctx, "p1"); b.Cursor != 500 {
		t.Fatalf("cursor lost: %+v", b)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "NEW", later); err != nil { // another YouTrack project starts over
		t.Fatal(err)
	}
	if b, _ := r.Binding(ctx, "p1"); b.Cursor != 0 || b.LastPolledAt != nil {
		t.Fatalf("cursor kept across projects: %+v", b)
	}
	if err := r.SetProjectYouTrack(ctx, "p2", "KIN", t0); err != nil {
		t.Fatal(err)
	}
	bs, err := r.Bindings(ctx)
	if err != nil || len(bs) != 2 {
		t.Fatalf("bindings %+v, %v", bs, err)
	}
	if _, err := r.BindingByShortName(ctx, "NONE"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown short name: %v", err)
	}
}

func TestIntakeSettingsDefaultAndSave(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	s, err := r.IntakeSettings(ctx)
	if err != nil || s != domain.DefaultIntakeSettings() {
		t.Fatalf("default %+v, %v", s, err)
	}
	want := domain.IntakeSettings{PollInterval: 5 * time.Minute, UpdatedAt: t0}
	if err := r.SaveIntakeSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	if s, err := r.IntakeSettings(ctx); err != nil || s.PollInterval != want.PollInterval || !s.UpdatedAt.Equal(t0) {
		t.Fatalf("saved %+v, %v", s, err)
	}
}

func TestUserLinks(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	if _, err := d.ExecContext(t.Context(), "INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u2', 'alice', 'h', 'user', 'x')"); err != nil {
		t.Fatal(err)
	}
	r := sqlite.New(d)
	ctx := t.Context()
	if _, err := r.LinkedUser(ctx, "jdoe"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unlinked: %v", err)
	}
	if err := r.LinkUser(ctx, domain.UserLink{YouTrackLogin: "jdoe", UserID: "u2", UpdatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if id, err := r.LinkedUser(ctx, "JDoe"); err != nil || id != "u2" {
		t.Fatalf("case-insensitive lookup %q, %v", id, err)
	}
	if err := r.LinkUser(ctx, domain.UserLink{YouTrackLogin: "JDOE", UserID: "owner1", UpdatedAt: t0}); !errors.Is(err, domain.ErrYouTrackLoginTaken) {
		t.Fatalf("taken: %v", err)
	}
	if err := r.LinkUser(ctx, domain.UserLink{YouTrackLogin: "x", UserID: "ghost", UpdatedAt: t0}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if err := r.LinkUser(ctx, domain.UserLink{YouTrackLogin: "alice.yt", UserID: "u2", UpdatedAt: t0}); err != nil { // replaces
		t.Fatal(err)
	}
	ls, err := r.UserLinks(ctx)
	if err != nil || len(ls) != 1 || ls[0].YouTrackLogin != "alice.yt" {
		t.Fatalf("links %+v, %v", ls, err)
	}
	if err := r.UnlinkUser(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	if ls, _ := r.UserLinks(ctx); len(ls) != 0 {
		t.Fatalf("after unlink %+v", ls)
	}
}

func TestIntakeRecords(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	if _, ok, err := r.IntakeRecord(ctx, "p1", "KIN-1"); err != nil || ok {
		t.Fatalf("missing: %v %v", ok, err)
	}
	rec := domain.IntakeRecord{ProjectID: "p1", IssueID: "KIN-1", IssueUpdated: 10, Outcome: domain.OutcomeRejected,
		Reason: domain.ReasonUnknownUser, Reporter: "jdoe", Channel: domain.ChannelPoll, ProcessedAt: t0}
	if err := r.SaveIntakeRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	rec2 := domain.IntakeRecord{ProjectID: "p1", IssueID: "KIN-2", IssueUpdated: 20, Outcome: domain.OutcomeCreated,
		Initiator: "owner1", TaskID: "t1", Channel: domain.ChannelWebhook, ProcessedAt: t0.Add(time.Second)}
	if err := r.SaveIntakeRecord(ctx, rec2); err != nil {
		t.Fatal(err)
	}
	rec.Outcome, rec.Reason, rec.TaskID, rec.ProcessedAt = domain.OutcomeCreated, "", "t9", t0.Add(2*time.Second)
	if err := r.SaveIntakeRecord(ctx, rec); err != nil { // upsert
		t.Fatal(err)
	}
	got, ok, err := r.IntakeRecord(ctx, "p1", "KIN-1")
	if err != nil || !ok || got.TaskID != "t9" || got.Reporter != "jdoe" || !got.ProcessedAt.Equal(rec.ProcessedAt) {
		t.Fatalf("got %+v %v %v", got, ok, err)
	}
	recent, err := r.RecentIntake(ctx, "p1", 1)
	if err != nil || len(recent) != 1 || recent[0].IssueID != "KIN-1" {
		t.Fatalf("recent %+v, %v", recent, err)
	}
}
