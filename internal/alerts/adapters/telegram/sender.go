// Package telegram delivers the Alert Center's notifications (outbox topic telegram.send) with the Bot API
// sendMessage method. It runs only in the outbox dispatcher, never in an HTTP handler; the bot token is read from
// the vault per delivery and never reaches payloads, errors or logs.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
)

// DefaultAPIBase is the Bot API endpoint.
const DefaultAPIBase = "https://api.telegram.org"

// Telegram's documented limits: about one message per second to one chat and 30 per second overall. The pacer
// stays below them so a burst of notifications is spread out instead of being rejected with 429.
const (
	DefaultPerChat = time.Second
	DefaultGlobal  = 40 * time.Millisecond // 25 messages/s
)

// ErrNotConfigured: no bot token is stored yet (Telegram settings, KIN-39).
var ErrNotConfigured = errors.New("telegram bot is not configured")

// Bot gives the sender the bot token and the Mini App link of a start parameter.
type Bot interface {
	Token(ctx context.Context) (string, error) // ErrNotConfigured when unset
	// MiniAppLink is the t.me direct link opening the Mini App with startParam; ok is false while the Mini App
	// settings are missing (the message is then sent without a button).
	MiniAppLink(ctx context.Context, startParam string) (link string, ok bool, err error)
}

// Sender is the outbox handler of app.TopicTelegramSend.
type Sender struct {
	Bot     Bot
	Logger  *slog.Logger // optional
	APIBase string       // default DefaultAPIBase; tests point it to an httptest server
	Client  *http.Client // default a client with a 15s timeout
	PerChat time.Duration
	Global  time.Duration
	Now     func() time.Time
	// Sleep waits d or until ctx ends; tests replace it to observe the pacing without waiting.
	Sleep func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex
	next  time.Time
	chats map[int64]time.Time
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pace reserves the next send slot for chat and waits for it.
func (s *Sender) pace(ctx context.Context, chat int64) error {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	perChat, global := s.PerChat, s.Global
	if perChat <= 0 {
		perChat = DefaultPerChat
	}
	if global <= 0 {
		global = DefaultGlobal
	}
	s.mu.Lock()
	t := now()
	if s.chats == nil {
		s.chats = map[int64]time.Time{}
	}
	at := t
	if s.next.After(at) {
		at = s.next
	}
	if c := s.chats[chat]; c.After(at) {
		at = c
	}
	s.next = at.Add(global)
	s.chats[chat] = at.Add(perChat)
	if len(s.chats) > 1000 { // forget chats whose slot has passed
		for id, c := range s.chats {
			if !c.After(t) {
				delete(s.chats, id)
			}
		}
	}
	s.mu.Unlock()
	if d := at.Sub(t); d > 0 {
		wait := sleep
		if s.Sleep != nil {
			wait = s.Sleep
		}
		return wait(ctx, d)
	}
	return nil
}

type button struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type sendMessage struct {
	ChatID      int64  `json:"chat_id"`
	Text        string `json:"text"`
	ReplyMarkup *struct {
		InlineKeyboard [][]button `json:"inline_keyboard"`
	} `json:"reply_markup,omitempty"`
}

type apiResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// Deliver sends one notification. Without a bot token Telegram is off: the message is dropped (the alert or
// approval stays visible in the app) instead of piling up as dead letters. A chat that blocked the bot or any other
// 4xx rejection is permanent; 429 and 5xx are retried by the outbox with backoff.
func (s *Sender) Deliver(ctx context.Context, d messaging.Delivery) error {
	var p app.SendPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.ChatID == 0 || p.Text == "" {
		return messaging.Permanent(errors.New("telegram.send: bad payload"))
	}
	token, err := s.Bot.Token(ctx)
	if errors.Is(err, ErrNotConfigured) {
		if s.Logger != nil {
			s.Logger.Info("telegram notification skipped: the bot is not configured", "key", d.Key)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading bot token: %w", err)
	}
	msg := sendMessage{ChatID: p.ChatID, Text: p.Text}
	if p.StartParam != "" && p.Button != "" {
		link, ok, err := s.Bot.MiniAppLink(ctx, p.StartParam)
		if err != nil {
			return fmt.Errorf("building Mini App link: %w", err)
		}
		if ok {
			msg.ReplyMarkup = &struct {
				InlineKeyboard [][]button `json:"inline_keyboard"`
			}{InlineKeyboard: [][]button{{{Text: p.Button, URL: link}}}}
		}
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := s.pace(ctx, p.ChatID); err != nil {
		return err
	}
	return s.post(ctx, token, body)
}

func (s *Sender) post(ctx context.Context, token string, body []byte) error {
	base := s.APIBase
	if base == "" {
		base = DefaultAPIBase
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("telegram: building request failed") // the URL holds the token: never echo it
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) { // url.Error prints the URL, which holds the token
			return fmt.Errorf("telegram: request failed: %w", ue.Err)
		}
		return errors.New("telegram: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	var r apiResponse
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r)
	switch {
	case resp.StatusCode == http.StatusOK && r.OK:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("telegram: rate limited, retry after %ss", strconv.Itoa(r.Parameters.RetryAfter))
	case resp.StatusCode >= 500:
		return fmt.Errorf("telegram: HTTP %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound:
		return messaging.Permanent(fmt.Errorf("telegram: bot token rejected (HTTP %d)", resp.StatusCode))
	default:
		return messaging.Permanent(fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, truncate(r.Description)))
	}
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
