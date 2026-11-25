package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// KIN-38: the write-back's comments link to the task page on the first public origin.
func TestTaskLinkBase(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":                                       "",
		"https://kd.example.com":                 "https://kd.example.com",
		" https://kd.example.com/x , https://b ": "https://kd.example.com",
		"ftp://kd":                               "",
		"kd.example.com":                         "",
	} {
		if got := taskLinkBase(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// KIN-108 regression: a youtrack.write message that goes dead outside IssueSync.HandleWrite (here a handler that
// rejects without recording anything, like a panic on the last attempt) fails its write with code "dead" and enqueues
// the write-back alert in the same transaction; retrying the message puts the write back to "awaiting publication".
func TestServedWriteBackFollowsDeadAndRetriedMessages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	inst, err := bootstrap.Run(ctx, bootstrap.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	logger := slog.New(slog.DiscardHandler)
	msg := newMessaging(inst, logger)
	integ := newIntegrations(inst, msg, newAudit(inst, logger), "", logger)
	wf := &workflowapp.Service{}
	wireWriteBack(wf, inst, msg, integ, "")
	key := "task/T1/v4/final"
	if err := wf.IssueSync.Comment(ctx, key, "T1", "p1", "KIN-7", "Done"); err != nil {
		t.Fatal(err)
	}
	msg.Outbox.Handle(integrationsapp.TopicYouTrackWrite, func(context.Context, messaging.Delivery) error {
		return messaging.Permanent(errors.New("died outside the write-back"))
	})
	if _, err := msg.Outbox.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	writes := integrationsdb.New(inst.DB)
	if w, err := writes.IssueWrite(ctx, key); err != nil || w.Status != integrations.WriteFailed || w.Error != "dead" {
		t.Fatalf("write = %+v, %v, want failed dead", w, err)
	}
	var alerts int
	if err := inst.DB.QueryRowContext(ctx, "SELECT count(*) FROM outbox_events WHERE topic = ?",
		integrationsapp.TopicYouTrackWriteFailed).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("failure alerts = %d, %v", alerts, err)
	}
	dead, err := msg.Outbox.Dead(ctx, 10)
	if err != nil || len(dead) != 1 {
		t.Fatalf("dead = %+v, %v", dead, err)
	}
	if err := msg.Outbox.Retry(ctx, dead[0].ID); err != nil {
		t.Fatal(err)
	}
	if w, err := writes.IssueWrite(ctx, key); err != nil || w.Status != integrations.WritePending {
		t.Fatalf("retried write = %+v, %v, want pending", w, err)
	}
}
