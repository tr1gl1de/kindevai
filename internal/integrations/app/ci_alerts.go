package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// CI alert hooks (KIN-60): CI outcomes on a project's integration branch or PRs, for the Alert Center. Alert First:
// a failure only raises an alert; no task is created (an analysis task needs a person's confirmation).
const (
	AdapterAlerts = "alerts"
	TopicCIFailed = "alerts.ci_failed"
	TopicCIPassed = "alerts.ci_passed"
)

// CIAlerts is a GitHubEventHandler: a completed check suite on the integration branch or a PR becomes an
// alerts.ci_failed / alerts.ci_passed event, enqueued in the inbox transaction.
func (s *GitHubService) CIAlerts(ctx context.Context, r domain.Repository, e domain.GitHubEvent) error {
	topic := ""
	switch domain.CIOutcomeOf(r, e) {
	case domain.CIFailed:
		topic = TopicCIFailed
	case domain.CIPassed, domain.CIClosed:
		topic = TopicCIPassed
	default:
		return nil
	}
	pr := domain.CIPullRequest(r, e)
	b, err := json.Marshal(map[string]any{"projectId": r.ProjectID, "repository": r.FullName(), "branch": e.Branch,
		"pullRequest": pr, "headSha": e.HeadSHA, "conclusion": e.Conclusion})
	if err != nil {
		return err
	}
	// One event per webhook delivery (the inbox already dropped redeliveries), so a re-run failing again on the
	// same commit raises again; hashed, as branch and repository names may exceed the key limit.
	sum := sha256.Sum256([]byte(e.Delivery + "\x00" + r.FullName() + "\x00" + e.Branch + "\x00" + strconv.Itoa(pr) +
		"\x00" + e.HeadSHA + "\x00" + e.Conclusion))
	key := "ci/" + hex.EncodeToString(sum[:])
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}
