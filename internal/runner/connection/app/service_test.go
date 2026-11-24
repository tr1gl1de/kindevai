package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

type fakeKeys struct {
	id      *domain.Identity
	created int
	err     error
}

func (f *fakeKeys) LoadOrCreate(context.Context) (domain.Identity, bool, error) {
	if f.err != nil {
		return domain.Identity{}, false, f.err
	}
	if f.id != nil {
		return *f.id, false, nil
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	id, _ := domain.NewIdentity(priv)
	f.id = &id
	f.created++
	return id, true, nil
}

func (f *fakeKeys) Load(context.Context) (domain.Identity, error) {
	if f.err != nil {
		return domain.Identity{}, f.err
	}
	if f.id == nil {
		return domain.Identity{}, ErrNoKey
	}
	return *f.id, nil
}

type fakeStore struct {
	e       domain.Enrollment
	saves   int
	saveErr error
}

func (f *fakeStore) Load(context.Context) (domain.Enrollment, error) { return f.e, nil }
func (f *fakeStore) Save(_ context.Context, e domain.Enrollment) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.e = e
	f.saves++
	return nil
}

type fakeCP struct {
	got    EnrollRequest
	server domain.ServerURL
	calls  int
	err    error
	fp     string // overrides the echoed fingerprint
	keys   *fakeKeys
}

func (f *fakeCP) Enroll(_ context.Context, s domain.ServerURL, r EnrollRequest) (EnrollResult, error) {
	f.calls++
	f.got, f.server = r, s
	if f.err != nil {
		return EnrollResult{}, f.err
	}
	fp := f.keys.id.Fingerprint() // the real Control Plane derives it from r.PublicKey
	if f.fp != "" {
		fp = f.fp
	}
	return EnrollResult{RunnerID: "r1", Status: "pending", Fingerprint: fp}, nil
}

var host = domain.Host{Hostname: "wsl-box", OS: "linux", Arch: "amd64", Version: "0.1.0"}

func newSvc() (*Service, *fakeKeys, *fakeStore, *fakeCP) {
	k, s := &fakeKeys{}, &fakeStore{}
	cp := &fakeCP{keys: k}
	return &Service{Keys: k, Enrollments: s, ControlPlane: cp, Host: host}, k, s, cp
}

func token(t *testing.T) domain.Token {
	t.Helper()
	tok, err := domain.ParseToken("tok-123")
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestEnrollCreatesKeyRegistersAndSaves(t *testing.T) {
	t.Parallel()
	svc, keys, store, cp := newSvc()
	res, err := svc.Enroll(t.Context(), "https://cp.example.com/", token(t))
	if err != nil {
		t.Fatal(err)
	}
	if !res.KeyCreated || keys.created != 1 {
		t.Fatalf("key created = %v (%d)", res.KeyCreated, keys.created)
	}
	if cp.got.Token.Reveal() != "tok-123" || cp.got.PublicKey != keys.id.PublicKeyBase64() || cp.got.Host != host {
		t.Fatalf("request = %+v", cp.got)
	}
	if cp.server.String() != "https://cp.example.com" {
		t.Fatalf("server = %s", cp.server)
	}
	want := domain.Enrollment{Server: cp.server, RunnerID: "r1", Fingerprint: keys.id.Fingerprint(), Status: "pending"}
	if res.Enrollment != want || store.e != want {
		t.Fatalf("enrollment = %+v, stored %+v", res.Enrollment, store.e)
	}
}

// A retry (e.g. after a dropped connection) reuses the stored key; it is never regenerated.
func TestEnrollRetryReusesKey(t *testing.T) {
	t.Parallel()
	svc, keys, _, _ := newSvc()
	first, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t))
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t))
	if err != nil {
		t.Fatal(err)
	}
	if keys.created != 1 || second.KeyCreated || first.Enrollment.Fingerprint != second.Enrollment.Fingerprint {
		t.Fatalf("created %d keys, second created=%v", keys.created, second.KeyCreated)
	}
}

func TestEnrollRejectsPlainHTTPBeforeAnything(t *testing.T) {
	t.Parallel()
	svc, keys, store, cp := newSvc()
	_, err := svc.Enroll(t.Context(), "http://cp.example.com", token(t))
	if !errors.Is(err, domain.ErrPlainHTTP) {
		t.Fatalf("err = %v", err)
	}
	if keys.created != 0 || cp.calls != 0 || store.saves != 0 {
		t.Fatal("side effects on a rejected URL")
	}
}

func TestEnrollRefusesOtherServerWhenEnrolled(t *testing.T) {
	t.Parallel()
	svc, _, store, cp := newSvc()
	srv, _ := domain.ParseServerURL("https://a.example.com")
	store.e = domain.Enrollment{Server: srv, RunnerID: "r0"}
	if _, err := svc.Enroll(t.Context(), "https://b.example.com", token(t)); !errors.Is(err, ErrEnrolledElsewhere) {
		t.Fatalf("err = %v", err)
	}
	if cp.calls != 0 {
		t.Fatal("called the other Control Plane")
	}
}

func TestEnrollDoesNotSaveOnFailure(t *testing.T) {
	t.Parallel()
	svc, _, store, cp := newSvc()
	cp.err = domain.ErrTokenExpired
	if _, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t)); !errors.Is(err, domain.ErrTokenExpired) {
		t.Fatalf("err = %v", err)
	}
	if store.saves != 0 {
		t.Fatal("saved a failed enrollment")
	}
}

// The server must have registered exactly our key; anything else is not trusted.
func TestEnrollRejectsFingerprintMismatch(t *testing.T) {
	t.Parallel()
	svc, _, store, cp := newSvc()
	cp.fp = "SHA256:someoneelse"
	if _, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t)); !errors.Is(err, domain.ErrFingerprintMismatch) {
		t.Fatalf("err = %v", err)
	}
	if store.saves != 0 {
		t.Fatal("saved an untrusted enrollment")
	}
}

func TestEnrollKeyStoreErrorStops(t *testing.T) {
	t.Parallel()
	svc, keys, _, cp := newSvc()
	keys.err = errors.New("key file is readable by others")
	if _, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t)); err == nil || cp.calls != 0 {
		t.Fatalf("err = %v, calls = %d", err, cp.calls)
	}
}

func TestFingerprint(t *testing.T) {
	t.Parallel()
	svc, keys, _, _ := newSvc()
	if _, err := svc.Fingerprint(t.Context()); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	if _, _, err := keys.LoadOrCreate(t.Context()); err != nil {
		t.Fatal(err)
	}
	fp, err := svc.Fingerprint(t.Context())
	if err != nil || fp != keys.id.Fingerprint() {
		t.Fatalf("fingerprint = %s, %v", fp, err)
	}
	if keys.created != 1 {
		t.Fatal("Fingerprint created a key")
	}
}

func TestReady(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newSvc()
	if _, _, err := svc.Ready(t.Context()); !errors.Is(err, domain.ErrNotEnrolled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t)); err != nil {
		t.Fatal(err)
	}
	e, id, err := svc.Ready(t.Context())
	if err != nil || e.RunnerID != "r1" || id.Fingerprint() != e.Fingerprint {
		t.Fatalf("ready = %+v, %v", e, err)
	}
}

// The server consumed the token; if saving fails the operator still learns the runner id to recover.
func TestEnrollSaveFailureNamesRegisteredRunner(t *testing.T) {
	t.Parallel()
	svc, _, store, _ := newSvc()
	store.saveErr = errors.New("disk full")
	_, err := svc.Enroll(t.Context(), "https://cp.example.com", token(t))
	if err == nil || !strings.Contains(err.Error(), "r1") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
}
