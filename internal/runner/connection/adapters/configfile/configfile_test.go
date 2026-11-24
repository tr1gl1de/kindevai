package configfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/secfile"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	s := &Store{Path: filepath.Join(t.TempDir(), "config.json")}
	e, err := s.Load(t.Context())
	if err != nil || e.Enrolled() {
		t.Fatalf("missing config: %+v, %v", e, err)
	}
	srv, _ := domain.ParseServerURL("https://cp.example.com/kin")
	want := domain.Enrollment{Server: srv, RunnerID: "r1", Fingerprint: "SHA256:abc", Status: "pending"}
	if err := s.Save(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(s.Path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	got, err := s.Load(t.Context())
	if err != nil || got != want {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestLoadRejectsBadConfig(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		mode os.FileMode
		want error
	}{
		"world readable": {`{"server":"https://cp.example.com","runnerId":"r1"}`, 0o644, secfile.ErrInsecure},
		"plain http":     {`{"server":"http://cp.example.com","runnerId":"r1"}`, 0o600, domain.ErrPlainHTTP},
		"unknown field":  {`{"server":"https://cp.example.com","insecure":true}`, 0o600, nil},
		"garbage":        {`{`, 0o600, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_ = os.Chmod(p, tc.mode)
			_, err := (&Store{Path: p}).Load(t.Context())
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
