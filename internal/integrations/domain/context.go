package domain

import (
	"regexp"
	"slices"
)

// Article is a YouTrack Knowledge Base article as the planner's context package needs it (KIN-34).
type Article struct {
	ID      string
	Title   string
	Content string
}

var articleIDRe = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{0,19}-A-[1-9][0-9]{0,9}\b`)

// ArticleIDs returns the Knowledge Base article ids (e.g. KIN-A-15) an issue text mentions or links, in order of
// first mention, at most limit.
func ArticleIDs(text string, limit int) []string {
	var out []string
	for _, id := range articleIDRe.FindAllString(text, -1) {
		if len(out) == limit {
			break
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
