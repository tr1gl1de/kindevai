package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
)

var t0 = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

func occ(sev domain.Severity) domain.Occurrence {
	return domain.Occurrence{Kind: domain.KindRunnerUnreachable, Severity: sev, DedupKey: "runner/r1/unreachable",
		Subject: "r1", Details: map[string]string{"runnerId": "r1"}}
}

func TestNewAlertValidates(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		o     domain.Occurrence
		field string
	}{
		"no kind":       {domain.Occurrence{Severity: domain.SeverityWarning, DedupKey: "k"}, "kind"},
		"bad severity":  {domain.Occurrence{Kind: domain.KindStorageLevel, Severity: "loud", DedupKey: "k"}, "severity"},
		"no dedup key":  {domain.Occurrence{Kind: domain.KindStorageLevel, Severity: domain.SeverityWarning}, "dedupKey"},
		"long dedupKey": {domain.Occurrence{Kind: domain.KindStorageLevel, Severity: domain.SeverityWarning, DedupKey: string(make([]byte, 300))}, "dedupKey"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := domain.NewAlert("a1", c.o, t0)
			var ve *domain.ValidationError
			if !errors.As(err, &ve) || ve.Field != c.field {
				t.Fatalf("err = %v, want validation of %s", err, c.field)
			}
		})
	}
	a, err := domain.NewAlert("a1", occ(domain.SeverityCritical), t0)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != domain.StateOpen || a.Occurrences != 1 || !a.FirstSeenAt.Equal(t0) || !a.Active() {
		t.Fatalf("new alert = %+v", a)
	}
}

// AC: OPEN → ACKNOWLEDGED/DISMISSED/RESOLVED; closed alerts do not change again.
func TestAlertTransitions(t *testing.T) {
	t.Parallel()
	type step func(*domain.Alert) error
	ack := func(a *domain.Alert) error { return a.Acknowledge("u1", t0.Add(time.Minute)) }
	dismiss := func(a *domain.Alert) error { return a.Dismiss("u1", t0.Add(time.Minute)) }
	resolve := func(a *domain.Alert) error { return a.Resolve("u1", t0.Add(time.Minute)) }
	cases := []struct {
		name  string
		steps []step
		want  domain.State
		fails bool
	}{
		{"ack", []step{ack}, domain.StateAcknowledged, false},
		{"dismiss", []step{dismiss}, domain.StateDismissed, false},
		{"resolve", []step{resolve}, domain.StateResolved, false},
		{"ack then resolve", []step{ack, resolve}, domain.StateResolved, false},
		{"ack then dismiss", []step{ack, dismiss}, domain.StateDismissed, false},
		{"ack twice", []step{ack, ack}, domain.StateAcknowledged, true},
		{"resolve then ack", []step{resolve, ack}, domain.StateResolved, true},
		{"dismiss then resolve", []step{dismiss, resolve}, domain.StateDismissed, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a, _ := domain.NewAlert("a1", occ(domain.SeverityWarning), t0)
			var err error
			for _, s := range c.steps {
				err = s(&a)
			}
			if c.fails != errors.Is(err, domain.ErrInvalidTransition) {
				t.Fatalf("err = %v, fails = %v", err, c.fails)
			}
			if a.State != c.want {
				t.Fatalf("state = %s, want %s", a.State, c.want)
			}
		})
	}
}

// AC: dedup/grouping — a repeat of an active alert counts as an occurrence; a higher severity escalates it and
// re-opens an acknowledged alert.
func TestAlertRecur(t *testing.T) {
	t.Parallel()
	a, _ := domain.NewAlert("a1", occ(domain.SeverityWarning), t0)
	if esc := a.Recur(occ(domain.SeverityWarning), t0.Add(time.Minute)); esc {
		t.Fatal("same severity escalated")
	}
	if a.Occurrences != 2 || !a.LastSeenAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after recur = %+v", a)
	}
	if err := a.Acknowledge("u1", t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if esc := a.Recur(occ(domain.SeverityCritical), t0.Add(3*time.Minute)); !esc {
		t.Fatal("higher severity did not escalate")
	}
	if a.Severity != domain.SeverityCritical || a.State != domain.StateOpen || a.AckedBy != "" {
		t.Fatalf("escalated = %+v", a)
	}
	if esc := a.Recur(occ(domain.SeverityWarning), t0.Add(4*time.Minute)); esc || a.Severity != domain.SeverityCritical {
		t.Fatalf("lower severity changed the alert: %+v", a)
	}
}

func TestPreferences(t *testing.T) {
	t.Parallel()
	p := domain.DefaultPreferences("u1")
	for _, ty := range domain.NotificationTypes {
		if !p.Wants(ty) {
			t.Fatalf("default does not want %s", ty)
		}
	}
	if p.Language != domain.LangEN {
		t.Fatalf("default language %q", p.Language)
	}
	p, err := domain.NewPreferences("u1", "ru", []domain.NotificationType{domain.NotifyAlertRaised})
	if err != nil {
		t.Fatal(err)
	}
	if p.Wants(domain.NotifyAlertRaised) || !p.Wants(domain.NotifyApprovalRequested) || p.Language != domain.LangRU {
		t.Fatalf("prefs = %+v", p)
	}
	var ve *domain.ValidationError
	if _, err := domain.NewPreferences("u1", "de", nil); !errors.As(err, &ve) || ve.Field != "language" {
		t.Fatalf("language err = %v", err)
	}
	if _, err := domain.NewPreferences("u1", "en", []domain.NotificationType{"spam"}); !errors.As(err, &ve) || ve.Field != "disabled" {
		t.Fatalf("type err = %v", err)
	}
}
