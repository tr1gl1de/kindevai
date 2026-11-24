package domain

import (
	"errors"
	"testing"
	"time"
)

func TestDefaultRetentionIsValid(t *testing.T) {
	t.Parallel()
	p := DefaultRetention()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Output != 14*Day || p.Events != 30*Day || p.MaxOutputBytes != 1<<30 {
		t.Fatalf("defaults %+v", p)
	}
}

func TestRetentionValidate(t *testing.T) {
	t.Parallel()
	ok := DefaultRetention()
	for name, mut := range map[string]func(*RetentionPolicy){
		"output under a day":         func(p *RetentionPolicy) { p.Output = time.Hour },
		"events shorter than output": func(p *RetentionPolicy) { p.Events = p.Output - Day },
		"limit below one attempt":    func(p *RetentionPolicy) { p.MaxOutputBytes = MaxAttemptOutput - 1 },
	} {
		p := ok
		mut(&p)
		if err := p.Validate(); !errors.Is(err, ErrInvalidRetention) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOutputLimitVerdict(t *testing.T) {
	t.Parallel()
	l := OutputLimits{Attempt: 100, Total: 1000}
	for _, c := range []struct {
		attempt, total, n int64
		want              OutputVerdict
	}{
		{0, 0, 100, OutputStored},
		{50, 900, 50, OutputStored},
		{50, 0, 51, OutputOverAttempt},
		{0, 950, 51, OutputOverTotal},
		{90, 990, 20, OutputOverAttempt}, // the attempt's own cap is reported first
	} {
		if got := l.Verdict(c.attempt, c.total, c.n); got != c.want {
			t.Errorf("Verdict(%d, %d, %d) = %v, want %v", c.attempt, c.total, c.n, got, c.want)
		}
	}
	if OutputOverAttempt.Reason() != "output_limit" || OutputOverTotal.Reason() != "output_storage_limit" || OutputStored.Reason() != "" {
		t.Fatal("reasons")
	}
}

func TestExpired(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !Expired(time.Time{}, cutoff) || !Expired(cutoff.Add(-time.Second), cutoff) || Expired(cutoff, cutoff) {
		t.Fatal("expired")
	}
}
