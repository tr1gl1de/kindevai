package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/redact"
)

// GitHub App use cases (KIN-28): the Owner configures the App (ID, private key, webhook secret), tests it and reads
// each registered repository from GitHub (installation, default branch, main/master protection) through the outbox;
// webhooks are verified, scoped to registered repositories and stored in the inbox; adapters get repository-scoped
// installation tokens through RepositoryToken.

// Audit event kinds of the GitHub App.
const (
	KindGitHubConfigured     = "integration.github_configured"
	KindGitHubDisconnected   = "integration.github_disconnected"
	KindGitHubCheckRequested = "integration.github_check_requested"
	KindGitHubChecked        = "integration.github_checked"
	KindRepositorySynced     = "integration.repository_synced"
)

// Outbox and inbox topics of the GitHub App.
const (
	AdapterGitHub       = "github"
	TopicGitHubCheck    = "github.check_app"
	TopicRepositorySync = "github.sync_repository"
	TopicGitHubEvent    = "github.event"
	SourceGitHub        = "github"
)

// GitHubStore keeps the App connection and the repositories' GitHub state. Every method runs on the caller's
// transaction (db.Conn).
type GitHubStore interface {
	GitHubApp(ctx context.Context) (domain.GitHubApp, error) // domain.ErrNotConfigured
	SaveGitHubApp(ctx context.Context, a domain.GitHubApp) error
	DeleteGitHubApp(ctx context.Context) error

	AllRepositories(ctx context.Context) ([]domain.Repository, error)
	Repository(ctx context.Context, projectID, id string) (domain.Repository, error)      // domain.ErrNotFound
	RepositoryByID(ctx context.Context, id string) (domain.Repository, error)             // domain.ErrNotFound
	RepositoryByFullName(ctx context.Context, fullName string) (domain.Repository, error) // domain.ErrNotFound
	SaveRepository(ctx context.Context, r domain.Repository) error
}

// AppCredentials sign the App's JWT. The key is a domain.Secret and never printed.
type AppCredentials struct {
	AppID      int64
	PrivateKey domain.Secret
}

// GitHubAPI is the GitHub client as the use cases need it. Check and read failures are results with stable codes
// ("unauthorized", "not_installed", "unreachable", ...) and short details without credentials or response bodies.
type GitHubAPI interface {
	CheckApp(ctx context.Context, c AppCredentials) domain.CheckResult
	ReadRepository(ctx context.Context, c AppCredentials, owner, name string) domain.SyncResult
	// InstallationToken mints (or returns a cached, still valid) installation token restricted to one repository.
	InstallationToken(ctx context.Context, c AppCredentials, installationID int64, repo string) (domain.Secret, error)
}

// Inbox stores verified webhook deliveries durably, deduplicated by delivery id (messaging.Inbox).
type Inbox interface {
	Receive(ctx context.Context, e messaging.InboundEvent) (bool, error)
}

// GitHubEventHandler consumes a processed webhook event of a registered repository. It runs inside the inbox
// transaction, so it must be quick and must not call external systems (enqueue to the outbox instead).
type GitHubEventHandler func(ctx context.Context, repo domain.Repository, e domain.GitHubEvent) error

// GitHubService implements the GitHub App use cases.
type GitHubService struct {
	Vault    *Vault
	Store    GitHubStore
	Projects Projects
	Tx       TxRunner
	Audit    AuditLog
	Outbox   Outbox
	Inbox    Inbox
	API      GitHubAPI
	Now      func() time.Time
	NewID    func() string
	Logger   *slog.Logger
	// Handlers receive webhook events of registered repositories (CI handling, PR tracking of later tasks).
	Handlers []GitHubEventHandler
}

// GitHubView is the App as the Owner sees it: credentials only as metadata.
type GitHubView struct {
	App           domain.GitHubApp
	PrivateKey    domain.SecretMeta
	WebhookSecret domain.SecretMeta
}

// GitHubInput configures the App; an empty PrivateKey or WebhookSecret keeps the stored one.
type GitHubInput struct {
	AppID         string
	PrivateKey    string
	WebhookSecret string
}

// WebhookDelivery is one POST from GitHub, before verification.
type WebhookDelivery struct {
	ID        string // X-GitHub-Delivery
	Event     string // X-GitHub-Event
	Signature string // X-Hub-Signature-256
	Body      []byte
}

// WebhookOutcome tells the webhook endpoint what happened to a verified delivery.
type WebhookOutcome string

const (
	WebhookAccepted  WebhookOutcome = "accepted"  // stored in the inbox
	WebhookDuplicate WebhookOutcome = "duplicate" // the delivery id was already stored
	WebhookIgnored   WebhookOutcome = "ignored"   // an event the platform does not process, or an unregistered repository
)

const githubSubject = "integration:github"

func (s *GitHubService) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func (s *GitHubService) inTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if s.Audit == nil || s.Tx == nil || s.Outbox == nil {
		return errNotConfigured
	}
	return s.Tx.InTx(ctx, fn)
}

func (s *GitHubService) record(ctx context.Context, actor auditapp.Actor, kind, projectID, subject string, details map[string]string) error {
	return s.Audit.Record(ctx, auditapp.Entry{Actor: actor, Kind: kind, ProjectID: projectID, Subject: subject, Details: details})
}

func (s *GitHubService) view(ctx context.Context) (*GitHubView, error) {
	a, err := s.Store.GitHubApp(ctx)
	if errors.Is(err, domain.ErrNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("loading github app: %w", err)
	}
	v := &GitHubView{App: a}
	if v.PrivateKey, err = s.Vault.Meta(ctx, domain.SecretGitHubAppPrivateKey); err != nil {
		return nil, fmt.Errorf("loading private key metadata: %w", err)
	}
	if v.WebhookSecret, err = s.Vault.Meta(ctx, domain.SecretGitHubWebhookSecret); err != nil {
		return nil, fmt.Errorf("loading webhook secret metadata: %w", err)
	}
	v.App.Check = a.Check.At(s.Now())
	return v, nil
}

// GitHub returns the App connection (Owner only); nil when none is configured.
func (s *GitHubService) GitHub(ctx context.Context, actor auth.Principal) (*GitHubView, error) {
	if err := ownerOnly(actor); err != nil {
		return nil, err
	}
	return s.view(ctx)
}

// ConfigureGitHub sets the App ID and replaces the credentials that are given (Owner only). The first configuration
// needs both; a new App ID needs its private key again. A new key re-reads every registered repository.
func (s *GitHubService) ConfigureGitHub(ctx context.Context, actor auth.Principal, in GitHubInput) (GitHubView, error) {
	if err := ownerOnly(actor); err != nil {
		return GitHubView{}, err
	}
	appID, err := domain.NewAppID(in.AppID)
	if err != nil {
		return GitHubView{}, err
	}
	var key, hook domain.Secret
	if in.PrivateKey != "" {
		if key, err = domain.NewPrivateKey(in.PrivateKey); err != nil {
			return GitHubView{}, err
		}
	}
	if in.WebhookSecret != "" {
		if hook, err = domain.NewWebhookSecret(in.WebhookSecret); err != nil {
			return GitHubView{}, err
		}
	}
	var view *GitHubView
	err = s.inTx(ctx, func(ctx context.Context) error {
		now := s.Now()
		a, err := s.Store.GitHubApp(ctx)
		switch {
		case errors.Is(err, domain.ErrNotConfigured):
			if key.IsZero() {
				return &domain.ValidationError{Field: "privateKey", Code: "required"}
			}
			if hook.IsZero() {
				return &domain.ValidationError{Field: "webhookSecret", Code: "required"}
			}
			a = domain.NewGitHubApp(appID, now)
		case err != nil:
			return fmt.Errorf("loading github app: %w", err)
		default:
			if a.AppID != appID && key.IsZero() { // a key belongs to one App
				return &domain.ValidationError{Field: "privateKey", Code: "key_required_for_app"}
			}
			if a.AppID != appID || !key.IsZero() {
				a.Check = domain.Check{Status: domain.CheckNone}
			}
			a.AppID, a.UpdatedAt = appID, now
		}
		if !key.IsZero() {
			if err := s.Vault.Put(ctx, domain.SecretGitHubAppPrivateKey, key, actor.UserID); err != nil {
				return err
			}
		}
		if !hook.IsZero() {
			if err := s.Vault.Put(ctx, domain.SecretGitHubWebhookSecret, hook, actor.UserID); err != nil {
				return err
			}
		}
		if err := s.Store.SaveGitHubApp(ctx, a); err != nil {
			return fmt.Errorf("saving github app: %w", err)
		}
		if !key.IsZero() {
			repos, err := s.Store.AllRepositories(ctx)
			if err != nil {
				return fmt.Errorf("listing repositories: %w", err)
			}
			for _, r := range repos {
				if err := s.startSync(ctx, &r); err != nil {
					return err
				}
			}
		}
		if view, err = s.view(ctx); err != nil {
			return err
		}
		return s.record(ctx, auditapp.UserActor(actor), KindGitHubConfigured, "", githubSubject, map[string]string{
			"appId": strconv.FormatInt(appID, 10), "privateKeyChanged": strconv.FormatBool(!key.IsZero()),
			"webhookSecretChanged": strconv.FormatBool(!hook.IsZero())})
	})
	if err != nil {
		return GitHubView{}, err
	}
	s.logger().Info("github app configured", "app_id", appID, "private_key_changed", !key.IsZero(),
		"webhook_secret_changed", !hook.IsZero(), "by", actor.UserID)
	return *view, nil
}

// DisconnectGitHub removes the App and its credentials (Owner only); the repository registry stays.
func (s *GitHubService) DisconnectGitHub(ctx context.Context, actor auth.Principal) error {
	if err := ownerOnly(actor); err != nil {
		return err
	}
	return s.inTx(ctx, func(ctx context.Context) error {
		if _, err := s.Store.GitHubApp(ctx); err != nil {
			return err
		}
		if err := s.Store.DeleteGitHubApp(ctx); err != nil {
			return fmt.Errorf("deleting github app: %w", err)
		}
		for _, name := range []domain.SecretName{domain.SecretGitHubAppPrivateKey, domain.SecretGitHubWebhookSecret} {
			if err := s.Vault.Delete(ctx, name); err != nil {
				return fmt.Errorf("deleting %s: %w", name, err)
			}
		}
		return s.record(ctx, auditapp.UserActor(actor), KindGitHubDisconnected, "", githubSubject, nil)
	})
}

// CheckGitHub starts an App test (Owner only) through the outbox; HandleGitHubCheck runs it.
func (s *GitHubService) CheckGitHub(ctx context.Context, actor auth.Principal) (GitHubView, error) {
	if err := ownerOnly(actor); err != nil {
		return GitHubView{}, err
	}
	id := s.NewID()
	payload, err := json.Marshal(checkPayload{CheckID: id})
	if err != nil {
		return GitHubView{}, fmt.Errorf("encoding check: %w", err)
	}
	var view *GitHubView
	err = s.inTx(ctx, func(ctx context.Context) error {
		a, err := s.Store.GitHubApp(ctx)
		if err != nil {
			return err
		}
		if !a.Check.Running(s.Now()) {
			a.Check.Start(id, s.Now())
			if err := s.Store.SaveGitHubApp(ctx, a); err != nil {
				return fmt.Errorf("saving github app: %w", err)
			}
			if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterGitHub, Topic: TopicGitHubCheck,
				Key: "github-check/" + id, Payload: payload}); err != nil {
				return fmt.Errorf("enqueueing app test: %w", err)
			}
			if err := s.record(ctx, auditapp.UserActor(actor), KindGitHubCheckRequested, "", githubSubject, map[string]string{"checkId": id}); err != nil {
				return err
			}
		}
		view, err = s.view(ctx)
		return err
	})
	if err != nil {
		return GitHubView{}, err
	}
	return *view, nil
}

// credentials loads the App ID and private key for a call to GitHub.
func (s *GitHubService) credentials(ctx context.Context) (AppCredentials, error) {
	a, err := s.Store.GitHubApp(ctx)
	if err != nil {
		return AppCredentials{}, err
	}
	key, err := s.Vault.Credential(ctx, domain.SecretGitHubAppPrivateKey)
	if err != nil {
		return AppCredentials{}, err
	}
	return AppCredentials{AppID: a.AppID, PrivateKey: key}, nil
}

// scrub is a safety net: the client must not put credentials into results; mask anything that looks like one.
func scrub(r domain.CheckResult) domain.CheckResult {
	r.Detail, r.Code = redact.Text(r.Detail), redact.Text(r.Code)
	return r
}

// HandleGitHubCheck is the outbox handler of TopicGitHubCheck: it authenticates as the App and records the result.
// A superseded or finished test is dropped; a failed test is a result, not a delivery error.
func (s *GitHubService) HandleGitHubCheck(ctx context.Context, d messaging.Delivery) error {
	var p checkPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.CheckID == "" {
		return messaging.Permanent(errors.New("malformed app test payload"))
	}
	a, err := s.Store.GitHubApp(ctx)
	if errors.Is(err, domain.ErrNotConfigured) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("loading github app: %w", err)
	}
	if a.Check.Status != domain.CheckPending || a.Check.ID != p.CheckID {
		return nil
	}
	var res domain.CheckResult
	creds, err := s.credentials(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured):
		return nil
	case errors.Is(err, domain.ErrSecretNotSet):
		res = domain.CheckResult{Code: "not_configured"}
	case err != nil:
		return err
	default:
		res = s.API.CheckApp(ctx, creds)
	}
	res = scrub(res)
	var applied bool
	err = s.inTx(ctx, func(ctx context.Context) error {
		a, err := s.Store.GitHubApp(ctx)
		if errors.Is(err, domain.ErrNotConfigured) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loading github app: %w", err)
		}
		if applied = a.Check.Finish(p.CheckID, res, s.Now()); !applied {
			return nil
		}
		if err := s.Store.SaveGitHubApp(ctx, a); err != nil {
			return fmt.Errorf("saving github app: %w", err)
		}
		return s.record(ctx, auditapp.System(), KindGitHubChecked, "", githubSubject,
			map[string]string{"checkId": p.CheckID, "status": string(a.Check.Status), "code": a.Check.Code})
	})
	if err == nil && applied {
		s.logger().Info("github app tested", "check", p.CheckID, "ok", res.OK, "code", res.Code)
	}
	return err
}

type syncPayload struct {
	RepositoryID string `json:"repositoryId"`
	SyncID       string `json:"syncId"`
}

// startSync marks a read of r from GitHub as pending and enqueues it, in the caller's transaction.
func (s *GitHubService) startSync(ctx context.Context, r *domain.Repository) error {
	id := s.NewID()
	payload, err := json.Marshal(syncPayload{RepositoryID: r.ID, SyncID: id})
	if err != nil {
		return fmt.Errorf("encoding repository read: %w", err)
	}
	r.StartSync(id, s.Now())
	if err := s.Store.SaveRepository(ctx, *r); err != nil {
		return fmt.Errorf("saving repository: %w", err)
	}
	if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterGitHub, Topic: TopicRepositorySync,
		Key: "github-sync/" + id, Payload: payload}); err != nil {
		return fmt.Errorf("enqueueing repository read: %w", err)
	}
	return nil
}

// RepositoryAdded reads a newly registered repository from GitHub when the App is configured. Service calls it
// inside the registration's transaction.
func (s *GitHubService) RepositoryAdded(ctx context.Context, r domain.Repository) error {
	if _, err := s.Store.GitHubApp(ctx); errors.Is(err, domain.ErrNotConfigured) {
		return nil
	} else if err != nil {
		return fmt.Errorf("loading github app: %w", err)
	}
	return s.startSync(ctx, &r)
}

// SyncRepository reads a project's repository from GitHub again (Owner only): installation, default branch and
// main/master protection.
func (s *GitHubService) SyncRepository(ctx context.Context, actor auth.Principal, projectID, id string) (domain.Repository, error) {
	if err := ownerOnly(actor); err != nil {
		return domain.Repository{}, err
	}
	if _, err := s.Projects.Lookup(ctx, projectID); err != nil {
		return domain.Repository{}, err
	}
	var r domain.Repository
	err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		if r, err = s.Store.Repository(ctx, projectID, id); err != nil {
			return err
		}
		if _, err := s.Store.GitHubApp(ctx); err != nil {
			return err
		}
		if r.GitHub.Sync.Running(s.Now()) {
			return nil
		}
		return s.startSync(ctx, &r)
	})
	if err != nil {
		return domain.Repository{}, err
	}
	r.GitHub.Sync = r.GitHub.Sync.At(s.Now())
	return r, nil
}

// HandleRepositorySync is the outbox handler of TopicRepositorySync.
func (s *GitHubService) HandleRepositorySync(ctx context.Context, d messaging.Delivery) error {
	var p syncPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.RepositoryID == "" || p.SyncID == "" {
		return messaging.Permanent(errors.New("malformed repository read payload"))
	}
	r, err := s.Store.RepositoryByID(ctx, p.RepositoryID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // removed meanwhile
	}
	if err != nil {
		return fmt.Errorf("loading repository: %w", err)
	}
	if r.GitHub.Sync.Status != domain.CheckPending || r.GitHub.Sync.ID != p.SyncID {
		return nil
	}
	var res domain.SyncResult
	creds, err := s.credentials(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured), errors.Is(err, domain.ErrSecretNotSet):
		res.Code = "not_configured"
	case err != nil:
		return err
	default:
		res = s.API.ReadRepository(ctx, creds, r.Owner, r.Name)
	}
	res.CheckResult = scrub(res.CheckResult)
	var applied bool
	err = s.inTx(ctx, func(ctx context.Context) error {
		r, err := s.Store.RepositoryByID(ctx, p.RepositoryID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loading repository: %w", err)
		}
		if applied = r.FinishSync(p.SyncID, res, s.Now()); !applied {
			return nil
		}
		if err := s.Store.SaveRepository(ctx, r); err != nil {
			return fmt.Errorf("saving repository: %w", err)
		}
		return s.record(ctx, auditapp.System(), KindRepositorySynced, r.ProjectID, repoSubject(r.ID), map[string]string{
			"repository": r.FullName(), "status": string(r.GitHub.Sync.Status), "code": r.GitHub.Sync.Code,
			"protection": string(r.GitHub.Protection), "unprotected": strings.Join(r.GitHub.Unprotected, ",")})
	})
	if err == nil && applied {
		s.logger().Info("repository read from github", "repository", r.FullName(), "ok", res.OK, "code", res.Code)
	}
	return err
}

// MaxDeliveryIDLen bounds X-GitHub-Delivery (a UUID in practice).
const MaxDeliveryIDLen = 100

// ReceiveWebhook verifies a delivery with the webhook secret (HMAC-SHA256, constant time), drops events the
// platform does not process and events of unregistered repositories, and stores the rest in the inbox in compact
// form, deduplicated by delivery id. domain.ErrNotConfigured without a webhook secret, domain.ErrBadSignature for a
// forged or corrupted delivery.
func (s *GitHubService) ReceiveWebhook(ctx context.Context, d WebhookDelivery) (WebhookOutcome, error) {
	secret, err := s.Vault.Credential(ctx, domain.SecretGitHubWebhookSecret)
	if errors.Is(err, domain.ErrSecretNotSet) {
		return "", domain.ErrNotConfigured
	}
	if err != nil {
		return "", err
	}
	if !domain.VerifySignature(secret, d.Body, d.Signature) {
		return "", domain.ErrBadSignature
	}
	if d.ID == "" || len(d.ID) > MaxDeliveryIDLen || strings.ContainsFunc(d.ID, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return "", &domain.ValidationError{Field: "X-GitHub-Delivery", Code: "delivery_id_format"}
	}
	if !domain.SupportedEvent(d.Event) {
		return WebhookIgnored, nil
	}
	e, err := domain.ParseGitHubEvent(d.Event, d.Body)
	if err != nil {
		return "", err
	}
	if e.Event != domain.EventPing {
		// Scope: only registered repositories reach the platform; anything else the App sees is dropped.
		r, err := s.Store.RepositoryByFullName(ctx, e.Repository)
		if errors.Is(err, domain.ErrNotFound) {
			return WebhookIgnored, nil
		}
		if err != nil {
			return "", fmt.Errorf("looking up repository: %w", err)
		}
		// A repository read from GitHub belongs to one installation; a delivery naming another is not ours.
		if r.GitHub.InstallationID != 0 && e.InstallationID != 0 && r.GitHub.InstallationID != e.InstallationID {
			return WebhookIgnored, nil
		}
	}
	payload, err := json.Marshal(e)
	if err != nil {
		return "", fmt.Errorf("encoding event: %w", err)
	}
	fresh, err := s.Inbox.Receive(ctx, messaging.InboundEvent{Source: SourceGitHub, DeliveryID: d.ID, Topic: TopicGitHubEvent, Payload: payload})
	if err != nil {
		return "", fmt.Errorf("storing webhook delivery: %w", err)
	}
	if !fresh {
		return WebhookDuplicate, nil
	}
	return WebhookAccepted, nil
}

// HandleGitHubEvent is the inbox handler of TopicGitHubEvent (inside the inbox transaction): it records the
// delivery on the App and hands events of still-registered repositories to the Handlers.
func (s *GitHubService) HandleGitHubEvent(ctx context.Context, in messaging.InboundEvent) error {
	var e domain.GitHubEvent
	if err := json.Unmarshal(in.Payload, &e); err != nil || e.Event == "" {
		return messaging.Permanent(errors.New("malformed github event"))
	}
	e.Delivery = in.DeliveryID
	a, err := s.Store.GitHubApp(ctx)
	switch {
	case errors.Is(err, domain.ErrNotConfigured):
		return nil // disconnected meanwhile
	case err != nil:
		return fmt.Errorf("loading github app: %w", err)
	}
	a.Delivered(e.Event, s.Now())
	if err := s.Store.SaveGitHubApp(ctx, a); err != nil {
		return fmt.Errorf("saving github app: %w", err)
	}
	if e.Event == domain.EventPing {
		return nil
	}
	r, err := s.Store.RepositoryByFullName(ctx, e.Repository)
	if errors.Is(err, domain.ErrNotFound) {
		return nil // unregistered meanwhile
	}
	if err != nil {
		return fmt.Errorf("loading repository: %w", err)
	}
	for _, h := range s.Handlers {
		if err := h(ctx, r, e); err != nil {
			return err
		}
	}
	s.logger().Debug("github event", "event", e.Event, "action", e.Action, "repository", r.FullName(), "delivery", in.DeliveryID)
	return nil
}

// CheckoutSource is where a runner checks a repository out from (KIN-29).
type CheckoutSource struct {
	Branch string        // the integration branch: the base of the attempt's work branch
	Token  domain.Secret // zero when the App is not installed on the repository (public repositories)
}

// CheckoutSource returns the registered repository's integration branch and, when the App is installed on it, a
// token for that repository only. A repository not registered for the project is domain.ErrNotFound.
func (s *GitHubService) CheckoutSource(ctx context.Context, projectID, fullName string) (CheckoutSource, error) {
	r, err := s.Store.RepositoryByFullName(ctx, fullName)
	if err != nil {
		return CheckoutSource{}, err
	}
	if r.ProjectID != projectID {
		return CheckoutSource{}, domain.ErrNotFound
	}
	src := CheckoutSource{Branch: r.IntegrationBranch}
	tok, err := s.RepositoryToken(ctx, projectID, fullName)
	switch {
	case errors.Is(err, domain.ErrNotInstalled):
	case err != nil:
		return CheckoutSource{}, err
	default:
		src.Token = tok
	}
	return src, nil
}

// RepositoryToken returns an installation token that can reach only the given repository, for adapters acting
// for a project (no actor: callers are trusted adapters). A repository not registered for that project is
// domain.ErrNotFound, so one project's work never gets a token for another project's repository.
// domain.ErrNotInstalled when the App is not installed on it (as of the last read). The token is minted on demand
// and cached in memory only, by the API client, for less than its lifetime.
func (s *GitHubService) RepositoryToken(ctx context.Context, projectID, fullName string) (domain.Secret, error) {
	r, err := s.Store.RepositoryByFullName(ctx, fullName)
	if err != nil {
		return domain.Secret{}, err
	}
	if r.ProjectID != projectID {
		return domain.Secret{}, domain.ErrNotFound
	}
	if r.GitHub.InstallationID == 0 {
		return domain.Secret{}, domain.ErrNotInstalled
	}
	creds, err := s.credentials(ctx)
	if err != nil {
		return domain.Secret{}, err
	}
	return s.API.InstallationToken(ctx, creds, r.GitHub.InstallationID, r.Name)
}
