package auth

import (
	"context"
	"testing"
)

func TestPrincipalContext(t *testing.T) {
	t.Parallel()
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("principal in empty context")
	}
	p := Principal{UserID: "u1", Login: "owner", Owner: true}
	got, ok := FromContext(WithPrincipal(t.Context(), p))
	if !ok || got != p {
		t.Fatalf("got %+v, %v", got, ok)
	}
}
