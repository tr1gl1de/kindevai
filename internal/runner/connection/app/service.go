// Package app holds the runner's connection use cases: enrolling with a Control Plane and loading the identity
// the daemon connects with.
package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

// ErrNoKey means the runner has no key yet; only enrollment creates one.
var ErrNoKey = errors.New("runner has no key yet; run `ai-runner enroll --server <url>` first")

// ErrEnrolledElsewhere means the runner already belongs to another Control Plane.
var ErrEnrolledElsewhere = errors.New("runner is already enrolled with another Control Plane; use a separate --data-dir for it")

// KeyStore persists the runner's private key.
type KeyStore interface {
	// LoadOrCreate returns the stored identity, or generates and stores one when none exists (created = true).
	// It never replaces an existing key, even an unreadable one.
	LoadOrCreate(ctx context.Context) (id domain.Identity, created bool, err error)
	// Load returns the stored identity, or ErrNoKey.
	Load(ctx context.Context) (domain.Identity, error)
}

// EnrollmentStore persists the runner's registration (config file).
type EnrollmentStore interface {
	Load(ctx context.Context) (domain.Enrollment, error) // zero Enrollment when none
	Save(ctx context.Context, e domain.Enrollment) error
}

// EnrollRequest is what the runner sends to register.
type EnrollRequest struct {
	Token     domain.Token
	PublicKey string // standard base64 of the raw Ed25519 public key
	Host      domain.Host
}

// EnrollResult is the Control Plane's answer.
type EnrollResult struct {
	RunnerID    string
	Status      string
	Fingerprint string
}

// ControlPlane is the Control Plane's enrollment API. Implementations map its errors onto domain errors.
type ControlPlane interface {
	Enroll(ctx context.Context, server domain.ServerURL, req EnrollRequest) (EnrollResult, error)
}

// Service runs the connection use cases.
type Service struct {
	Keys         KeyStore
	Enrollments  EnrollmentStore
	ControlPlane ControlPlane
	Host         domain.Host
}

// Enrolled is the outcome of Enroll.
type Enrolled struct {
	Enrollment domain.Enrollment
	KeyCreated bool
}

// Enroll registers the runner's key with the Control Plane at server using a one-time token. A retry with the
// same token reuses the same key, so an interrupted enrollment can be repeated safely.
func (s *Service) Enroll(ctx context.Context, server string, token domain.Token) (Enrolled, error) {
	srv, err := domain.ParseServerURL(server)
	if err != nil {
		return Enrolled{}, err
	}
	prev, err := s.Enrollments.Load(ctx)
	if err != nil {
		return Enrolled{}, err
	}
	if prev.Enrolled() && prev.Server != srv {
		return Enrolled{}, fmt.Errorf("%w (%s)", ErrEnrolledElsewhere, prev.Server)
	}
	id, created, err := s.Keys.LoadOrCreate(ctx)
	if err != nil {
		return Enrolled{}, err
	}
	res, err := s.ControlPlane.Enroll(ctx, srv, EnrollRequest{Token: token, PublicKey: id.PublicKeyBase64(), Host: s.Host})
	if err != nil {
		return Enrolled{KeyCreated: created}, err
	}
	if res.Fingerprint != id.Fingerprint() {
		return Enrolled{KeyCreated: created}, domain.ErrFingerprintMismatch
	}
	e := domain.Enrollment{Server: srv, RunnerID: res.RunnerID, Fingerprint: res.Fingerprint, Status: res.Status}
	if err := s.Enrollments.Save(ctx, e); err != nil {
		return Enrolled{KeyCreated: created}, fmt.Errorf("the Control Plane registered runner %s (fingerprint %s), but "+
			"saving the registration failed; fix the cause and run enroll again with the same token while it is valid: %w",
			e.RunnerID, e.Fingerprint, err)
	}
	return Enrolled{Enrollment: e, KeyCreated: created}, nil
}

// Fingerprint is the fingerprint of the stored key, for comparing with the runner's card in the Dashboard.
func (s *Service) Fingerprint(ctx context.Context) (string, error) {
	id, err := s.Keys.Load(ctx)
	if err != nil {
		return "", err
	}
	return id.Fingerprint(), nil
}

// Ready returns the registration and identity the daemon connects with, or domain.ErrNotEnrolled.
func (s *Service) Ready(ctx context.Context) (domain.Enrollment, domain.Identity, error) {
	e, err := s.Enrollments.Load(ctx)
	if err != nil {
		return domain.Enrollment{}, domain.Identity{}, err
	}
	if !e.Enrolled() {
		return domain.Enrollment{}, domain.Identity{}, domain.ErrNotEnrolled
	}
	id, err := s.Keys.Load(ctx)
	if errors.Is(err, ErrNoKey) {
		return domain.Enrollment{}, domain.Identity{}, fmt.Errorf("runner is enrolled but its key is missing: %w", err)
	}
	if err != nil {
		return domain.Enrollment{}, domain.Identity{}, err
	}
	if id.Fingerprint() != e.Fingerprint {
		return domain.Enrollment{}, domain.Identity{}, fmt.Errorf("runner key does not match the enrolled fingerprint %s: %w",
			e.Fingerprint, domain.ErrFingerprintMismatch)
	}
	return e, id, nil
}
