package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

// TelegramRepository stores Telegram identities, link requests and the Mini App settings.
type TelegramRepository interface {
	IdentityByTelegramID(ctx context.Context, telegramID int64) (domain.TelegramIdentity, error) // domain.ErrNotFound
	IdentityByUser(ctx context.Context, userID string) (domain.TelegramIdentity, error)          // domain.ErrNotFound
	// CreateIdentity inserts the link; a taken Telegram ID or user is domain.ErrTelegramTaken / ErrUserHasTelegram.
	CreateIdentity(ctx context.Context, id domain.TelegramIdentity) error
	// DeleteIdentity removes the user's link; false if there was none.
	DeleteIdentity(ctx context.Context, userID string) (bool, error)

	CreateLinkRequest(ctx context.Context, r domain.LinkRequest) error
	LinkRequestByTokenHash(ctx context.Context, hash []byte) (domain.LinkRequest, error) // domain.ErrNotFound
	LatestLinkRequest(ctx context.Context, userID string) (domain.LinkRequest, error)    // domain.ErrNotFound
	// SaveLinkRequest stores the mutable state (claim, code, attempts, consumed/cancelled).
	SaveLinkRequest(ctx context.Context, r domain.LinkRequest) error
	// CancelOpenLinkRequests cancels the user's requests that are neither used nor cancelled.
	CancelOpenLinkRequests(ctx context.Context, userID string, now time.Time) error
	// DeleteLinkRequestsBefore purges requests that expired before cutoff.
	DeleteLinkRequestsBefore(ctx context.Context, cutoff time.Time) error

	Settings(ctx context.Context) (domain.BotSettings, time.Time, error) // domain.ErrNotFound
	SaveSettings(ctx context.Context, s domain.BotSettings, now time.Time) error
}

// BotTokenVault keeps the bot token encrypted at rest (integrations vault).
type BotTokenVault interface {
	BotToken(ctx context.Context) (string, error) // domain.ErrNotFound if never set
	SetBotToken(ctx context.Context, token, by string) error
}

// TelegramSessions is the part of the session store the Telegram flows need.
type TelegramSessions interface {
	Create(ctx context.Context, s domain.Session) error
	Delete(ctx context.Context, hash []byte) error
	DeleteByUserVia(ctx context.Context, userID string, via domain.SessionVia) error
}

// Audit event kinds of the Telegram flows (sign-ins reuse auth.login_succeeded / auth.login_failed with method=telegram).
const (
	KindTelegramSettingsUpdated = "telegram.settings_updated"
	KindTelegramLinkRequested   = "telegram.link_requested"
	KindTelegramLinkCancelled   = "telegram.link_cancelled"
	KindTelegramLinked          = "telegram.linked"
	KindTelegramUnlinked        = "telegram.unlinked"
)

// TelegramService implements the Mini App sign-in, linking a Telegram account to an existing account (two-sided
// confirmation), unlinking, and the Owner's bot settings.
type TelegramService struct {
	Repo     TelegramRepository
	Vault    BotTokenVault
	Users    UserRepository
	Sessions TelegramSessions
	Tx       TxRunner
	Audit    AuditLog
	Now      func() time.Time
	Rand     io.Reader
	NewID    func() string
	Logger   *slog.Logger
}

func (s *TelegramService) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// TelegramSettingsView is the Owner's view of the bot settings; the token is never returned.
type TelegramSettingsView struct {
	Configured   bool
	BotUsername  string
	AppShortName string
	TokenSet     bool
	UpdatedAt    time.Time
}

// Settings returns the bot settings (Owner only).
func (s *TelegramService) Settings(ctx context.Context, actor auth.Principal) (TelegramSettingsView, error) {
	if err := ownerOnly(actor); err != nil {
		return TelegramSettingsView{}, err
	}
	return s.settingsView(ctx)
}

func (s *TelegramService) settingsView(ctx context.Context) (TelegramSettingsView, error) {
	var v TelegramSettingsView
	st, at, err := s.Repo.Settings(ctx)
	switch {
	case err == nil:
		v.BotUsername, v.AppShortName, v.UpdatedAt = st.BotUsername, st.AppShortName, at
	case !errors.Is(err, domain.ErrNotFound):
		return v, fmt.Errorf("loading Telegram settings: %w", err)
	}
	if _, err := s.Vault.BotToken(ctx); err == nil {
		v.TokenSet = true
	} else if !errors.Is(err, domain.ErrNotFound) {
		return v, fmt.Errorf("reading bot token: %w", err)
	}
	v.Configured = v.TokenSet && v.BotUsername != ""
	return v, nil
}

// SaveSettings stores the bot username, Mini App short name and (when given) a new bot token (Owner only). An empty
// token keeps the stored one; there must be one.
func (s *TelegramService) SaveSettings(ctx context.Context, actor auth.Principal, botUsername, appShortName, botToken string) (TelegramSettingsView, error) {
	if err := ownerOnly(actor); err != nil {
		return TelegramSettingsView{}, err
	}
	user, err := domain.NewBotUsername(botUsername)
	if err != nil {
		return TelegramSettingsView{}, err
	}
	short, err := domain.NewAppShortName(appShortName)
	if err != nil {
		return TelegramSettingsView{}, err
	}
	token := ""
	if botToken != "" {
		if token, err = domain.NewBotToken(botToken); err != nil {
			return TelegramSettingsView{}, err
		}
	} else if _, err := s.Vault.BotToken(ctx); errors.Is(err, domain.ErrNotFound) {
		return TelegramSettingsView{}, &domain.ValidationError{Field: "botToken", Code: "bot_token_required"}
	} else if err != nil {
		return TelegramSettingsView{}, fmt.Errorf("reading bot token: %w", err)
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.SaveSettings(ctx, domain.BotSettings{BotUsername: user, AppShortName: short}, s.Now()); err != nil {
			return fmt.Errorf("saving Telegram settings: %w", err)
		}
		if token != "" {
			if err := s.Vault.SetBotToken(ctx, token, actor.UserID); err != nil {
				return fmt.Errorf("storing bot token: %w", err)
			}
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTelegramSettingsUpdated,
			Subject: "integration:telegram", Details: map[string]string{
				"botUsername": user, "appShortName": short, "tokenChanged": strconv.FormatBool(token != "")}})
	})
	if err != nil {
		return TelegramSettingsView{}, err
	}
	return s.settingsView(ctx)
}

// botToken returns the token, or domain.ErrTelegramNotConfigured.
func (s *TelegramService) botToken(ctx context.Context) (string, error) {
	t, err := s.Vault.BotToken(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return "", domain.ErrTelegramNotConfigured
	}
	if err != nil {
		return "", fmt.Errorf("reading bot token: %w", err)
	}
	return t, nil
}

// validate checks raw initData against the configured bot.
func (s *TelegramService) validate(ctx context.Context, initData string) (domain.InitData, error) {
	token, err := s.botToken(ctx)
	if err != nil {
		return domain.InitData{}, err
	}
	return domain.ValidateInitData(initData, token, s.Now())
}

// linkRequestRetention keeps finished link requests this long (for the web status) before they are purged.
const linkRequestRetention = 24 * time.Hour

// Telegram sign-in failure reasons recorded with auth.login_failed.
const (
	failTelegramNotLinked = "telegram_not_linked"
)

// Login signs in the account linked to the Telegram user of the signed initData and opens a Telegram session.
// An unknown Telegram user gets domain.ErrTelegramNotLinked: accounts are never created from Telegram data. A
// disabled account is domain.ErrInvalidCredentials.
func (s *TelegramService) Login(ctx context.Context, initData, client string) (string, domain.User, error) {
	d, err := s.validate(ctx, initData)
	if err != nil {
		return "", domain.User{}, err
	}
	tgID := strconv.FormatInt(d.User.ID, 10)
	id, err := s.Repo.IdentityByTelegramID(ctx, d.User.ID)
	if errors.Is(err, domain.ErrNotFound) {
		s.loginFailed(ctx, domain.User{}, failTelegramNotLinked, client, tgID)
		return "", domain.User{}, domain.ErrTelegramNotLinked
	}
	if err != nil {
		return "", domain.User{}, fmt.Errorf("looking up Telegram identity: %w", err)
	}
	u, err := s.Users.ByID(ctx, id.UserID)
	if err != nil {
		return "", domain.User{}, fmt.Errorf("loading user: %w", err)
	}
	if u.Disabled {
		s.loginFailed(ctx, u, failDisabled, client, tgID)
		return "", domain.User{}, domain.ErrInvalidCredentials
	}
	plain, sess, err := domain.NewSession(s.Rand, u.ID, s.Now())
	if err != nil {
		return "", domain.User{}, err
	}
	sess.Via = domain.ViaTelegram
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Sessions.Create(ctx, sess); err != nil {
			return fmt.Errorf("creating session: %w", err)
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.User(u.ID, string(u.Login)), Kind: KindLoginSucceeded,
			Subject: userSubject(u.ID), Details: map[string]string{"ip": client, "method": "telegram", "telegramId": tgID}})
	})
	if err != nil {
		return "", domain.User{}, err
	}
	return plain, u, nil
}

// loginFailed records a failed Mini App sign-in, best-effort (see AuthService.loginFailed).
func (s *TelegramService) loginFailed(ctx context.Context, u domain.User, reason, client, tgID string) {
	e := auditapp.Entry{Actor: auditapp.Anonymous(), Kind: KindLoginFailed,
		Details: map[string]string{"ip": client, "reason": reason, "method": "telegram", "telegramId": tgID}}
	if u.ID != "" {
		e.Subject, e.Details["login"] = userSubject(u.ID), string(u.Login)
	}
	if err := record(ctx, s.Audit, e); err != nil {
		s.logger().Error("recording failed Telegram sign-in", "err", err)
	}
}

// LinkStatus is what the web session sees on its profile.
type LinkStatus struct {
	Configured bool
	Identity   *domain.TelegramIdentity
	Request    *domain.LinkRequest // the latest request of this web session, if any
	State      domain.LinkState
}

// Status returns the actor's Telegram link and, for the web session sessionHash, its latest link request.
func (s *TelegramService) Status(ctx context.Context, actor auth.Principal, sessionHash []byte) (LinkStatus, error) {
	var st LinkStatus
	v, err := s.settingsView(ctx)
	if err != nil {
		return st, err
	}
	st.Configured = v.Configured
	if id, err := s.Repo.IdentityByUser(ctx, actor.UserID); err == nil {
		st.Identity = &id
	} else if !errors.Is(err, domain.ErrNotFound) {
		return st, fmt.Errorf("loading Telegram identity: %w", err)
	}
	r, err := s.Repo.LatestLinkRequest(ctx, actor.UserID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
	case err != nil:
		return st, fmt.Errorf("loading link request: %w", err)
	case string(r.SessionHash) == string(sessionHash):
		st.Request, st.State = &r, r.State(s.Now())
	}
	return st, nil
}

// StartLink creates a link request for the actor's web session (replacing any open one) and returns the Mini App
// link with the one-time token.
func (s *TelegramService) StartLink(ctx context.Context, actor auth.Principal, sessionHash []byte) (domain.LinkRequest, string, error) {
	if len(sessionHash) == 0 {
		return domain.LinkRequest{}, "", domain.ErrUnauthenticated
	}
	st, _, err := s.Repo.Settings(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.LinkRequest{}, "", domain.ErrTelegramNotConfigured
	}
	if err != nil {
		return domain.LinkRequest{}, "", fmt.Errorf("loading Telegram settings: %w", err)
	}
	if _, err := s.botToken(ctx); err != nil {
		return domain.LinkRequest{}, "", err
	}
	if _, err := s.Repo.IdentityByUser(ctx, actor.UserID); err == nil {
		return domain.LinkRequest{}, "", domain.ErrUserHasTelegram
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.LinkRequest{}, "", fmt.Errorf("loading Telegram identity: %w", err)
	}
	now := s.Now()
	plain, r, err := domain.NewLinkRequest(s.Rand, s.NewID(), actor.UserID, sessionHash, now)
	if err != nil {
		return domain.LinkRequest{}, "", err
	}
	err = s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Repo.CancelOpenLinkRequests(ctx, actor.UserID, now); err != nil {
			return fmt.Errorf("cancelling previous link requests: %w", err)
		}
		// Housekeeping: old requests hold Telegram names and have no further use.
		if err := s.Repo.DeleteLinkRequestsBefore(ctx, now.Add(-linkRequestRetention)); err != nil {
			return err
		}
		if err := s.Repo.CreateLinkRequest(ctx, r); err != nil {
			return fmt.Errorf("saving link request: %w", err)
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTelegramLinkRequested,
			Subject: userSubject(actor.UserID), Details: map[string]string{"request": r.ID}})
	})
	if err != nil {
		return domain.LinkRequest{}, "", err
	}
	return r, st.LinkURL(domain.StartLink + "_" + plain), nil
}

// CancelLink withdraws the actor's open link request (no-op without one).
func (s *TelegramService) CancelLink(ctx context.Context, actor auth.Principal) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		return s.Repo.CancelOpenLinkRequests(ctx, actor.UserID, s.Now())
	})
}

// LinkPreview is what the Mini App shows before the Telegram user confirms.
type LinkPreview struct {
	Login     string
	ExpiresAt time.Time
}

// linkFromInitData validates initData and loads the usable link request named by its signed start parameter.
func (s *TelegramService) linkFromInitData(ctx context.Context, initData string) (domain.InitData, domain.LinkRequest, error) {
	d, err := s.validate(ctx, initData)
	if err != nil {
		return d, domain.LinkRequest{}, err
	}
	kind, token := domain.ParseStartParam(d.StartParam)
	if kind != domain.StartLink {
		return d, domain.LinkRequest{}, domain.ErrLinkInvalid
	}
	r, err := s.Repo.LinkRequestByTokenHash(ctx, domain.HashLinkToken(token))
	if errors.Is(err, domain.ErrNotFound) {
		return d, domain.LinkRequest{}, domain.ErrLinkInvalid
	}
	if err != nil {
		return d, domain.LinkRequest{}, fmt.Errorf("loading link request: %w", err)
	}
	if err := r.Usable(s.Now()); err != nil {
		return d, domain.LinkRequest{}, err
	}
	return d, r, nil
}

// PreviewLink tells the Telegram user which account the link would attach them to.
func (s *TelegramService) PreviewLink(ctx context.Context, initData string) (LinkPreview, error) {
	d, r, err := s.linkFromInitData(ctx, initData)
	if err != nil {
		return LinkPreview{}, err
	}
	if r.Telegram.ID != 0 && r.Telegram.ID != d.User.ID {
		return LinkPreview{}, domain.ErrLinkClaimed
	}
	u, err := s.Users.ByID(ctx, r.UserID)
	if err != nil {
		return LinkPreview{}, fmt.Errorf("loading user: %w", err)
	}
	return LinkPreview{Login: string(u.Login), ExpiresAt: r.ExpiresAt}, nil
}

// ConfirmLink records the Telegram user's confirmation in the Mini App and returns the code the web session must
// enter. A Telegram account already linked elsewhere is refused here, before any code is shown.
func (s *TelegramService) ConfirmLink(ctx context.Context, initData string) (string, LinkPreview, error) {
	var code string
	var pv LinkPreview
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		d, r, err := s.linkFromInitData(ctx, initData)
		if err != nil {
			return err
		}
		if _, err := s.Repo.IdentityByTelegramID(ctx, d.User.ID); err == nil {
			return domain.ErrTelegramTaken
		} else if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("looking up Telegram identity: %w", err)
		}
		u, err := s.Users.ByID(ctx, r.UserID)
		if err != nil {
			return fmt.Errorf("loading user: %w", err)
		}
		if code, err = r.Claim(s.Rand, d.User, s.Now()); err != nil {
			return err
		}
		if err := s.Repo.SaveLinkRequest(ctx, r); err != nil {
			return fmt.Errorf("saving link request: %w", err)
		}
		pv = LinkPreview{Login: string(u.Login), ExpiresAt: r.ExpiresAt}
		return nil
	})
	return code, pv, err
}

// CompleteLink checks the code in the web session that created the request and links the Telegram account:
// uniqueness is re-checked, the identity created, the request consumed and the event audited in one transaction.
// Wrong codes are counted even though the link fails.
func (s *TelegramService) CompleteLink(ctx context.Context, actor auth.Principal, sessionHash []byte, code string) (domain.TelegramIdentity, error) {
	var id domain.TelegramIdentity
	var codeErr error
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		r, err := s.Repo.LatestLinkRequest(ctx, actor.UserID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrLinkInvalid
		}
		if err != nil {
			return fmt.Errorf("loading link request: %w", err)
		}
		now := s.Now()
		if codeErr = r.CheckCode(code, sessionHash, now); codeErr != nil {
			if errors.Is(codeErr, domain.ErrLinkCodeWrong) || errors.Is(codeErr, domain.ErrLinkTooManyAttempts) {
				if err := s.Repo.SaveLinkRequest(ctx, r); err != nil {
					return fmt.Errorf("saving link request: %w", err)
				}
			}
			if errors.Is(codeErr, domain.ErrLinkTooManyAttempts) {
				return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTelegramLinkCancelled,
					Subject: userSubject(actor.UserID), Details: map[string]string{"request": r.ID, "reason": "too_many_attempts"}})
			}
			return nil // commit the counted attempt; codeErr is returned below
		}
		if _, err := s.Repo.IdentityByTelegramID(ctx, r.Telegram.ID); err == nil {
			return domain.ErrTelegramTaken
		} else if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("looking up Telegram identity: %w", err)
		}
		if _, err := s.Repo.IdentityByUser(ctx, actor.UserID); err == nil {
			return domain.ErrUserHasTelegram
		} else if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("loading Telegram identity: %w", err)
		}
		id = domain.TelegramIdentity{UserID: actor.UserID, TelegramID: r.Telegram.ID, Username: r.Telegram.Username, LinkedAt: now}
		if err := s.Repo.CreateIdentity(ctx, id); err != nil {
			return err
		}
		r.Consume(now)
		if err := s.Repo.SaveLinkRequest(ctx, r); err != nil {
			return fmt.Errorf("saving link request: %w", err)
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTelegramLinked,
			Subject: userSubject(actor.UserID), Details: map[string]string{
				"telegramId": strconv.FormatInt(id.TelegramID, 10), "telegramUsername": id.Username, "request": r.ID}})
	})
	if err != nil {
		return domain.TelegramIdentity{}, err
	}
	if codeErr != nil {
		return domain.TelegramIdentity{}, codeErr
	}
	s.logger().Info("Telegram linked", "user", actor.UserID)
	return id, nil
}

// Unlink removes the actor's Telegram link and ends their Telegram sessions; ErrTelegramNotLinked without one.
func (s *TelegramService) Unlink(ctx context.Context, actor auth.Principal) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		id, err := s.Repo.IdentityByUser(ctx, actor.UserID)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrTelegramNotLinked
		}
		if err != nil {
			return fmt.Errorf("loading Telegram identity: %w", err)
		}
		if _, err := s.Repo.DeleteIdentity(ctx, actor.UserID); err != nil {
			return fmt.Errorf("unlinking Telegram: %w", err)
		}
		if err := s.Repo.CancelOpenLinkRequests(ctx, actor.UserID, s.Now()); err != nil {
			return err
		}
		if err := s.Sessions.DeleteByUserVia(ctx, actor.UserID, domain.ViaTelegram); err != nil {
			return err
		}
		return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.UserActor(actor), Kind: KindTelegramUnlinked,
			Subject: userSubject(actor.UserID), Details: map[string]string{"telegramId": strconv.FormatInt(id.TelegramID, 10)}})
	})
}

// InviteTelegramUser validates the initData sent with an invitation acceptance from the Mini App
// (startapp=invite_…) and returns its Telegram user, refusing one that is already linked.
func (s *TelegramService) InviteTelegramUser(ctx context.Context, initData string) (domain.TelegramUser, error) {
	d, err := s.validate(ctx, initData)
	if err != nil {
		return domain.TelegramUser{}, err
	}
	if _, err := s.Repo.IdentityByTelegramID(ctx, d.User.ID); err == nil {
		return domain.TelegramUser{}, domain.ErrTelegramTaken
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.TelegramUser{}, fmt.Errorf("looking up Telegram identity: %w", err)
	}
	return d.User, nil
}

// LinkNewUser links tg to the account u created in the caller's transaction (invitation acceptance) and audits it.
func (s *TelegramService) LinkNewUser(ctx context.Context, u domain.User, tg domain.TelegramUser) error {
	id := domain.TelegramIdentity{UserID: u.ID, TelegramID: tg.ID, Username: tg.Username, LinkedAt: s.Now()}
	if err := s.Repo.CreateIdentity(ctx, id); err != nil {
		return err
	}
	return record(ctx, s.Audit, auditapp.Entry{Actor: auditapp.User(u.ID, string(u.Login)), Kind: KindTelegramLinked,
		Subject: userSubject(u.ID), Details: map[string]string{
			"telegramId": strconv.FormatInt(tg.ID, 10), "telegramUsername": tg.Username, "via": "invitation"}})
}
