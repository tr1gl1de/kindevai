package domain_test

import (
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func add(at int64, author string, tags ...string) domain.TagEvent {
	return domain.TagEvent{At: at, Author: author, Added: tags}
}

func remove(at int64, author string, tags ...string) domain.TagEvent {
	return domain.TagEvent{At: at, Author: author, Removed: tags}
}

// KIN-50: the account that applied the trigger tag is the one that must be allowed to start AI, so the applier is
// derived from YouTrack's tag history and anything unclear yields no applier (fail closed).
func TestTagApplierIsTheAuthorOfTheNewestEventThatAddedTheTag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		events []domain.TagEvent
		want   string
	}{
		{"single add", []domain.TagEvent{add(10, "alice", "kindevai")}, "alice"},
		{"case and spaces of the tag name", []domain.TagEvent{add(10, "alice", " KinDevAI ")}, "alice"},
		{"order of the response does not matter",
			[]domain.TagEvent{add(30, "mallory", "kindevai"), remove(20, "alice", "kindevai"), add(10, "alice", "kindevai")}, "mallory"},
		{"re-added by someone else after removal",
			[]domain.TagEvent{add(10, "alice", "kindevai"), remove(20, "alice", "kindevai"), add(30, "mallory", "kindevai")}, "mallory"},
		{"other tags are ignored",
			[]domain.TagEvent{add(10, "mallory", "kindevai"), add(20, "alice", "kindevai2"), add(30, "alice", "ui")}, "mallory"},
		{"same author twice at one instant", []domain.TagEvent{add(10, "alice", "kindevai"), add(10, "alice", "kindevai")}, "alice"},
		{"no events", nil, ""},
		{"only other tags", []domain.TagEvent{add(10, "alice", "ui")}, ""},
		{"newest event removes the tag", []domain.TagEvent{add(10, "alice", "kindevai"), remove(20, "bob", "kindevai")}, ""},
		{"two authors at one instant", []domain.TagEvent{add(10, "alice", "kindevai"), add(10, "mallory", "kindevai")}, ""},
		{"add and remove at one instant", []domain.TagEvent{add(10, "alice", "kindevai"), remove(10, "alice", "kindevai")}, ""},
		{"one event adds and removes", []domain.TagEvent{{At: 10, Author: "alice", Added: []string{"kindevai"}, Removed: []string{"KINDEVAI"}}}, ""},
		{"an authorless add next to a known one", []domain.TagEvent{add(10, "", "kindevai"), add(10, "alice", "kindevai")}, ""},
		{"newest add has no author", []domain.TagEvent{add(10, "alice", "kindevai"), add(20, "", "kindevai")}, ""},
		{"author with spaces is not a login", []domain.TagEvent{add(10, "alice bob", "kindevai")}, ""},
		{"event without a time cannot be ordered", []domain.TagEvent{add(10, "alice", "kindevai"), add(0, "mallory", "kindevai")}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := domain.TagApplier(c.events, "kindevai")
			if got != c.want || ok != (c.want != "") {
				t.Fatalf("TagApplier = %q, %v; want %q", got, ok, c.want)
			}
		})
	}
}
