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

// List returns every record; an unreadable one is skipped with an error only if nothing else can be read.
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
		b, err := os.ReadFile(filepath.Join(d.Path, e.Name()))
		if err != nil {
			return nil, err
		}
		var r domain.Record
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("attempt record %s: %w", e.Name(), err)
		}
		out = append(out, r)
	}
	return out, nil
}
