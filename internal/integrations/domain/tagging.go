package domain

import "strings"

// Who applied the trigger tag (KIN-50). Anybody who may edit a YouTrack issue may tag it, so the account that
// starts AI work through the tag is the one linked to the YouTrack user who applied it — not the issue's reporter.
// That user is read from YouTrack's tag history; whatever is unclear about it yields no applier (fail closed).

// MaxTagEvents bounds the tag history read for one issue; a longer history is not trusted to be complete.
const MaxTagEvents = 100

// TagEvent is one change of an issue's tags from YouTrack's activity log. Every field is untrusted text: the author
// only becomes a permission through an Owner-made login link and the project's access check.
type TagEvent struct {
	At      int64    // Unix ms
	Author  string   // YouTrack login
	Added   []string // tag names
	Removed []string
}

// TagApplier returns the YouTrack login that applied tag as it is on the issue now: the author of the newest event
// that touched the tag. ok is false when that cannot be told for certain: no such event, the newest one removes
// the tag, events at the newest instant disagree (other authors, or an add and a removal), an event has no time,
// or the author is not a plausible login.
func TagApplier(events []TagEvent, tag string) (login string, ok bool) {
	var newest int64
	var latest []TagEvent
	for _, e := range events {
		added, removed := hasTag(e.Added, tag), hasTag(e.Removed, tag)
		if !added && !removed {
			continue
		}
		switch {
		case e.At <= 0:
			return "", false
		case e.At > newest:
			newest, latest = e.At, []TagEvent{e}
		case e.At == newest:
			latest = append(latest, e)
		}
	}
	for i, e := range latest {
		if hasTag(e.Removed, tag) || (i > 0 && e.Author != login) {
			return "", false
		}
		login = e.Author
	}
	if _, err := NewYouTrackLogin(login); err != nil || login != strings.TrimSpace(login) {
		return "", false
	}
	return login, true
}

func hasTag(names []string, tag string) bool {
	tag = strings.TrimSpace(tag)
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), tag) {
			return true
		}
	}
	return false
}
