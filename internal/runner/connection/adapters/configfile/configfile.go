// Package configfile keeps the runner's registration in <data-dir>/config.json (mode 0600): the Control Plane
// URL, the runner id and the enrolled key fingerprint. The key itself lives in its own file.
package configfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

const maxConfig = 64 << 10

// Store is the config file at Path.
type Store struct {
	Path string
}

var _ app.EnrollmentStore = (*Store)(nil)

type file struct {
	Server      string `json:"server"`
	RunnerID    string `json:"runnerId"`
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"`
}

// Load returns the saved registration, or a zero Enrollment when the file does not exist.
func (s *Store) Load(_ context.Context) (domain.Enrollment, error) {
	b, err := secfile.ReadPrivate(s.Path, maxConfig)
	if errors.Is(err, fs.ErrNotExist) {
		return domain.Enrollment{}, nil
	}
	if err != nil {
		return domain.Enrollment{}, fmt.Errorf("runner config: %w", err)
	}
	var f file
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return domain.Enrollment{}, fmt.Errorf("runner config %s: %w", s.Path, err)
	}
	srv, err := domain.ParseServerURL(f.Server)
	if err != nil {
		return domain.Enrollment{}, fmt.Errorf("runner config %s: server: %w", s.Path, err)
	}
	return domain.Enrollment{Server: srv, RunnerID: f.RunnerID, Fingerprint: f.Fingerprint, Status: f.Status}, nil
}

// Save atomically replaces the file.
func (s *Store) Save(_ context.Context, e domain.Enrollment) error {
	b, err := json.MarshalIndent(file{Server: e.Server.String(), RunnerID: e.RunnerID, Fingerprint: e.Fingerprint,
		Status: e.Status}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding runner config: %w", err)
	}
	return secfile.WriteReplace(s.Path, append(b, '\n'))
}
