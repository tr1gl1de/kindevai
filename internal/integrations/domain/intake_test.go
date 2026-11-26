package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func code(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func TestIntakeValueValidation(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"KIN-7": "KIN-7", " kin-12 ": "KIN-12", "A_B2-1": "A_B2-1"} {
		if got, err := NewIssueID(in); err != nil || got != want {
			t.Errorf("NewIssueID(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "KIN", "KIN-0", "7-KIN", "KIN-7/../x", "KIN-7?fields=x", strings.Repeat("A", 21) + "-1"} {
		if _, err := NewIssueID(in); code(err) != "issue_format" {
			t.Errorf("NewIssueID(%q) = %v", in, err)
		}
	}
	for in, want := range map[string]string{"": DefaultTriggerTag, " ai run ": "ai run", "kin-dev_ai.2": "kin-dev_ai.2"} {
		if got, err := NewTriggerTag(in); err != nil || got != want {
			t.Errorf("NewTriggerTag(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"a}b", "{x", "a\nb", strings.Repeat("t", 51), "a\"b"} {
		if _, err := NewTriggerTag(in); code(err) != "tag_format" {
			t.Errorf("NewTriggerTag(%q) = %v", in, err)
		}
	}
	if got, err := NewYouTrackLogin(" j.doe@corp "); err != nil || got != "j.doe@corp" {
		t.Errorf("login = %q, %v", got, err)
	}
	for _, in := range []string{"", "a b", "a\x00", strings.Repeat("x", 101)} {
		if _, err := NewYouTrackLogin(in); code(err) != "youtrack_login_format" {
			t.Errorf("NewYouTrackLogin(%q) = %v", in, err)
		}
	}
	for _, s := range []int{MinPollSeconds, 60, MaxPollSeconds} {
		if d, err := NewPollInterval(s); err != nil || d != time.Duration(s)*time.Second {
			t.Errorf("NewPollInterval(%d) = %v, %v", s, d, err)
		}
	}
	for _, s := range []int{0, MinPollSeconds - 1, MaxPollSeconds + 1} {
		if _, err := NewPollInterval(s); code(err) != "poll_interval_range" {
			t.Errorf("NewPollInterval(%d) = %v", s, err)
		}
	}
	if DefaultIntakeSettings().PollInterval != 60*time.Second {
		t.Error("default poll interval must be 60s")
	}
	if _, err := NewYouTrackWebhookSecret("short"); code(err) != "webhook_secret_range" {
		t.Errorf("short webhook secret = %v", err)
	}
	if s, err := NewYouTrackWebhookSecret(strings.Repeat("k", 32)); err != nil || s.Reveal() != strings.Repeat("k", 32) {
		t.Errorf("webhook secret = %v", err)
	}
}

func snap() IssueSnapshot {
	return IssueSnapshot{ID: "KIN-7", Project: "KIN", Summary: "Login page", Updated: 1000, Reporter: "jdoe",
		Tags: []string{"backend", "KinDevAI"}}
}

func TestSnapshotValidationRejectsForeignShapes(t *testing.T) {
	t.Parallel()
	if err := snap().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*IssueSnapshot){
		"bad id":                func(s *IssueSnapshot) { s.ID = "x" },
		"id of another project": func(s *IssueSnapshot) { s.Project = "OTHER" },
		"no update time":        func(s *IssueSnapshot) { s.Updated = 0 },
		"too many tags":         func(s *IssueSnapshot) { s.Tags = make([]string, MaxSnapshotTags+1) },
	} {
		s := snap()
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBindingDecidesWhichIssuesItTakes(t *testing.T) {
	t.Parallel()
	b := Binding{ProjectID: "p1", ShortName: "KIN", TriggerTag: "kindevai"}
	s := snap()
	if !b.Wants(s, false) {
		t.Fatal("tagged issue (tags compare case-insensitively) must be taken")
	}
	s.Tags = []string{"backend"}
	if b.Wants(s, false) {
		t.Fatal("untagged issue taken by polling")
	}
	if !b.Wants(s, true) {
		t.Fatal("run by issue ID needs no tag")
	}
	s.Project = "OTHER"
	if b.Wants(s, true) {
		t.Fatal("issue of another YouTrack project taken")
	}
}

func TestStaleSnapshotsAreHarmless(t *testing.T) {
	t.Parallel()
	prev := IntakeRecord{IssueUpdated: 2000, Outcome: OutcomeCreated}
	s := snap()
	if !prev.Supersedes(s) {
		t.Fatal("an older snapshot arriving late must be ignored")
	}
	s.Updated = 2000
	if prev.Supersedes(s) {
		t.Fatal("a duplicate of the same state is re-applied idempotently, not dropped")
	}
	if (IntakeRecord{IssueUpdated: 5000, Outcome: OutcomeQueued}).Supersedes(s) {
		t.Fatal("a queued manual run never hides an issue state")
	}
}

func TestTaskTitleIsSanitised(t *testing.T) {
	t.Parallel()
	s := snap()
	s.Summary = "  Fix\tthe\n\x07login   page "
	if got := s.TaskTitle(); got != "KIN-7: Fix the login page" {
		t.Fatalf("title = %q", got)
	}
	s.Summary = strings.Repeat("я", 400)
	if got := s.TaskTitle(); len([]rune(got)) != MaxTaskTitle || !strings.HasSuffix(got, "…") {
		t.Fatalf("long title = %d runes", len([]rune(got)))
	}
	s.Summary = " "
	if got := s.TaskTitle(); got != "KIN-7" {
		t.Fatalf("empty summary title = %q", got)
	}
}
