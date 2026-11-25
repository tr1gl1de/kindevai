package domain

import (
	"slices"
	"testing"
)

func TestArticleIDs(t *testing.T) {
	t.Parallel()
	text := "See [KIN-A-15](https://x.youtrack.cloud/articles/KIN-A-15), KIN-A-14 and KIN-34; also XKIN-A-1x, AB-A-2, CD-A-3."
	if got := ArticleIDs(text, 3); !slices.Equal(got, []string{"KIN-A-15", "KIN-A-14", "AB-A-2"}) {
		t.Fatalf("ids %v", got)
	}
	if got := ArticleIDs("no links", 3); len(got) != 0 {
		t.Fatalf("ids %v", got)
	}
}
