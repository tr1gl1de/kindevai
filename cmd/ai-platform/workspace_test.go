package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
)

type fakeSources map[string]integrationsapp.CheckoutSource

func (f fakeSources) CheckoutSource(_ context.Context, project, repo string) (integrationsapp.CheckoutSource, error) {
	src, ok := f[project+" "+repo]
	if !ok {
		return integrationsapp.CheckoutSource{}, integrations.ErrNotFound
	}
	return src, nil
}

func assignCmd(t *testing.T, a protocol.Assign) runners.QueuedCommand {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return runners.QueuedCommand{ID: "assign/s/1", Kind: protocol.CommandAssign, Body: b}
}

// KIN-29: the base branch and the repository token are added to execution.assign only when it is sent, so the
// token never sits in the command log; the other commands and unregistered repositories pass unchanged.
func TestCheckoutSourceIsAddedWhenAnAssignmentIsSent(t *testing.T) {
	t.Parallel()
	tok, _ := integrations.NewSecret("ghs_live")
	ch := &runnersapp.Channel{}
	var prevCalled bool
	ch.Prepare = func(_ context.Context, _ string, q runners.QueuedCommand) (json.RawMessage, error) {
		prevCalled = true
		return q.Body, nil
	}
	attachCheckoutSource(ch, fakeSources{"p1 acme/api": {Branch: "dev", Token: tok}, "p1 acme/pub": {Branch: "main-int"}})
	ctx := t.Context()

	stored := assignCmd(t, protocol.Assign{Step: "s", Fence: 1, Project: "p1", Repository: "acme/api"})
	b, err := ch.Prepare(ctx, "r1", stored)
	var got protocol.Assign
	if err != nil || json.Unmarshal(b, &got) != nil || got.Base != "dev" || got.Credential == nil ||
		got.Credential.Token != "ghs_live" || !prevCalled {
		t.Fatalf("%s %v", b, err)
	}
	if strings.Contains(string(stored.Body), "ghs_") {
		t.Fatal("stored body changed")
	}
	b, _ = ch.Prepare(ctx, "r1", assignCmd(t, protocol.Assign{Step: "s", Fence: 1, Project: "p1", Repository: "acme/pub"}))
	got = protocol.Assign{}
	if err := json.Unmarshal(b, &got); err != nil || got.Base != "main-int" || got.Credential != nil {
		t.Fatalf("public: %s", b)
	}
	for _, q := range []runners.QueuedCommand{
		assignCmd(t, protocol.Assign{Step: "s", Fence: 1, Project: "p2", Repository: "acme/api"}), // other project
		{ID: "c", Kind: protocol.CommandCancel, Body: json.RawMessage(`{"step":"s","fence":1}`)},
	} {
		b, err := ch.Prepare(ctx, "r1", q)
		if err != nil || string(b) != string(q.Body) {
			t.Errorf("%s: %s %v", q.Kind, b, err)
		}
	}
}

func TestCheckoutSourceErrorKeepsTheStoredBody(t *testing.T) {
	t.Parallel()
	ch := &runnersapp.Channel{}
	attachCheckoutSource(ch, failingSources{})
	if _, err := ch.Prepare(t.Context(), "r1", assignCmd(t, protocol.Assign{Project: "p1", Repository: "acme/api"})); err == nil {
		t.Fatal("no error")
	}
}

type failingSources struct{}

func (failingSources) CheckoutSource(context.Context, string, string) (integrationsapp.CheckoutSource, error) {
	return integrationsapp.CheckoutSource{}, errors.New("github down")
}
