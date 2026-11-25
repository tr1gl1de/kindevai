package app

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func (g *fakeGit) Push(_ context.Context, mirror, repository, branch, sha string, tok Token) error {
	g.log("push " + mirror + " " + repository + " " + branch + " " + sha)
	g.mu.Lock()
	g.tokens = append(g.tokens, tok.Reveal())
	g.mu.Unlock()
	return g.fetchErr
}

// KIN-36: the publish step pushes the reviewed head to the step's own work branch from the repository cache, with
// the assignment's token.
func TestPushPublishesTheStepBranch(t *testing.T) {
	t.Parallel()
	m, g, _, _ := newManager()
	branch, err := m.Push(t.Context(), PushRequest{Step: "st1", Task: "KIN-29", Repository: "acme/api",
		Head: "abc", Token: NewToken("ghs_x")})
	if err != nil || branch != "feature/KIN-29--st1" {
		t.Fatalf("%q %v", branch, err)
	}
	if !slices.Equal(g.calls, []string{"push /r/acme/api.git acme/api feature/KIN-29--st1 abc"}) || g.tokens[0] != "ghs_x" {
		t.Fatalf("%v %v", g.calls, g.tokens)
	}
	g.fetchErr = errors.New("rejected")
	if _, err := m.Push(t.Context(), PushRequest{Step: "st1", Task: "KIN-29", Repository: "acme/api", Head: "abc"}); err == nil {
		t.Fatal("a rejected push succeeded")
	}
	for _, r := range []PushRequest{{Step: "st1", Task: "KIN-29", Repository: "bad"}, {Step: "a--b", Task: "KIN-29", Repository: "acme/api"}} {
		if _, err := m.Push(t.Context(), r); err == nil {
			t.Errorf("%+v pushed", r)
		}
	}
}
