// Package journal keeps the runner's on-disk record of running attempts (KIN-23) under <data-dir>/attempts: one
// JSON file per attempt (written atomically) next to the attempt's working directory.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Dir is the journal directory.
type Dir struct{ Path string }

var _ app.Journal = Dir{}

var stepRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (d Dir) name(step string, fence int64) (string, error) {
	if !stepRe.MatchString(step) || fence < 1 {
		return "", fmt.Errorf("invalid attempt %q/%d", step, fence)
	}
	return step + "-" + strconv.FormatInt(fence, 10), nil
}

// Prepare creates the attempt's working directory.
func (d Dir) Prepare(step string, fence int64) (string, error) {
	n, err := d.name(step, fence)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(d.Path, n)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Save writes the record atomically and durably.
func (d Dir) Save(r domain.Record) error {
	n, err := d.name(r.Step, r.Fence)
	if err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(d.Path, n+".json.tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d.Path, n+".json"))
}

// Delete removes the record and the working directory.
func (d Dir) Delete(step string, fence int64) error {
	n, err := d.name(step, fence)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(d.Path, n+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.RemoveAll(filepath.Join(d.Path, n))
}

// TombstoneTTL is how long a finished attempt's tombstone is kept: as long as the Control Plane keeps
// acknowledged commands (it never re-sends an older one).
const TombstoneTTL = 7 * 24 * time.Hour

// Finish removes the record and the working directory, leaves a tombstone and prunes expired tombstones.
func (d Dir) Finish(step string, fence int64) error {
	n, err := d.name(step, fence)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d.Path, n+".done"), nil, 0o600); err != nil {
		return err
	}
	if err := d.Delete(step, fence); err != nil {
		return err
	}
	ents, err := os.ReadDir(d.Path)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".done") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > TombstoneTTL {
			_ = os.Remove(filepath.Join(d.Path, e.Name()))
		}
	}
	return nil
}

// Finished reports a tombstone of the attempt.
func (d Dir) Finished(step string, fence int64) (bool, error) {
	n, err := d.name(step, fence)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(d.Path, n+".done"))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, err
}

// List returns every readable record. An unreadable one (e.g. truncated by a full disk) is moved aside as
// *.corrupt, so one bad file cannot stop the runner from taking or recovering any attempt.
func (d Dir) List() ([]domain.Record, error) {
	ents, err := os.ReadDir(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []domain.Record
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(d.Path, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r domain.Record
		if json.Unmarshal(b, &r) != nil || !stepRe.MatchString(r.Step) || r.Fence < 1 {
			_ = os.Rename(p, p+".corrupt")
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
