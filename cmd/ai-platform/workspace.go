package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// checkoutSources is the integrations context's GitHubService (KIN-28 App, KIN-26 registry).
type checkoutSources interface {
	CheckoutSource(ctx context.Context, projectID, fullName string) (integrationsapp.CheckoutSource, error)
}

// attachCheckoutSource completes execution.assign right before it is sent on the runner's authenticated session
// (KIN-29): the repository's integration branch as the worktree base and a short-lived installation token for
// that repository only. The command log keeps the body without them, so no token is ever stored. An assignment
// whose repository is not registered for its project goes out unchanged (the attempt runs in an empty
// directory); an error leaves the stored body to be sent (Channel.Prepare's rule: the stream never stalls).
func attachCheckoutSource(ch *runnersapp.Channel, src checkoutSources) {
	prev := ch.Prepare
	ch.Prepare = func(ctx context.Context, runnerID string, q runners.QueuedCommand) (json.RawMessage, error) {
		body := q.Body
		if prev != nil {
			b, err := prev(ctx, runnerID, q)
			if err != nil {
				return nil, err
			}
			body = b
		}
		if q.Kind != protocol.CommandAssign {
			return body, nil
		}
		var a protocol.Assign
		if err := json.Unmarshal(body, &a); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", q.Kind, err)
		}
		s, err := src.CheckoutSource(ctx, a.Project, a.Repository)
		if errors.Is(err, integrations.ErrNotFound) {
			return body, nil
		}
		if err != nil {
			return nil, fmt.Errorf("checkout source of %s: %w", a.Repository, err)
		}
		a.Base = s.Branch
		if !s.Token.IsZero() {
			a.Credential = &protocol.GitCredential{Token: s.Token.Reveal()}
		}
		return json.Marshal(a)
	}
}
