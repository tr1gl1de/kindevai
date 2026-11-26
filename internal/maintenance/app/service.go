// Package app holds the maintenance mode use cases (KIN-41).
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/maintenance/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

// Audit event kinds of the maintenance context.
const (
	KindEnabled  = "maintenance.enabled"
	KindDisabled = "maintenance.disabled"
)

// Store persists the mode.
type Store interface {
	Load(ctx context.Context) (domain.Mode, error)
	Save(ctx context.Context, m domain.Mode) error
}

// AuditLog records audit events in the caller's transaction.
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// TxRunner runs fn in one transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Drain counts the steps claimed by or running on runners (the work maintenance mode waits for).
type Drain interface {
	ActiveSteps(ctx context.Context) (int, error)
}

// Status is the mode plus the drain progress.
type Status struct {
	domain.Mode
	ActiveAttempts int
}

// Service switches maintenance mode. The Control Plane is a single instance (KIN-41 lock), so the mode is cached
// in memory after Load and every read (scheduler pass, read-only check) is free.
type Service struct {
	Store Store
	Audit AuditLog
	Tx    TxRunner
	Drain Drain // optional
	Now   func() time.Time

	setMu sync.Mutex // serialises Set; mu guards only the cached mode, so readers never wait for a transaction
	mu    sync.RWMutex
	mode  domain.Mode
}

// Load reads the persisted mode; call it once at startup.
func (s *Service) Load(ctx context.Context) error {
	m, err := s.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("loading maintenance mode: %w", err)
	}
	s.mu.Lock()
	s.mode = m
	s.mu.Unlock()
	return nil
}

// Mode is the current mode.
func (s *Service) Mode() domain.Mode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// SchedulingPaused implements the workflow scheduler's Pause.
func (s *Service) SchedulingPaused(context.Context) (bool, error) { return s.Mode().Enabled, nil }

// Status returns the mode and how many attempts still run.
func (s *Service) Status(ctx context.Context) (Status, error) {
	st := Status{Mode: s.Mode()}
	if s.Drain != nil {
		n, err := s.Drain.ActiveSteps(ctx)
		if err != nil {
			return Status{}, err
		}
		st.ActiveAttempts = n
	}
	return st, nil
}

// Set switches the mode on (with reason) or off. Only the Owner may; a change is audited. Setting the current
// state again changes nothing (no audit event).
func (s *Service) Set(ctx context.Context, p auth.Principal, enabled bool, reason string) (Status, error) {
	if !p.Owner {
		return Status{}, domain.ErrForbidden
	}
	next := domain.Disabled()
	if enabled {
		m, err := domain.Enable(s.Now(), p.Login, reason)
		if err != nil {
			return Status{}, err
		}
		next = m
	}
	s.setMu.Lock()
	defer s.setMu.Unlock()
	cur := s.Mode()
	if cur.Enabled == next.Enabled && (!enabled || cur.Reason == next.Reason) {
		return s.Status(ctx)
	}
	if enabled && cur.Enabled { // only the reason changes: keep who and since
		next.Since, next.By = cur.Since, cur.By
	}
	kind, details := KindDisabled, map[string]string{}
	if enabled {
		kind, details = KindEnabled, map[string]string{"reason": next.Reason}
	}
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Store.Save(ctx, next); err != nil {
			return err
		}
		if cur.Enabled == next.Enabled {
			return nil
		}
		return s.Audit.Record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: kind, Subject: "platform",
			Details: details})
	})
	if err != nil {
		return Status{}, fmt.Errorf("switching maintenance mode: %w", err)
	}
	s.mu.Lock()
	s.mode = next
	s.mu.Unlock()
	return s.Status(ctx)
}
