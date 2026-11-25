package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func TestAvailabilityAfterThreeMissedHeartbeatsIsUnreachable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		last *time.Time
		now  time.Time
		want domain.Availability
	}{
		{"never sent a heartbeat", nil, t0, domain.AvailabilityNever},
		{"fresh heartbeat", ptr(t0), t0.Add(14 * time.Second), domain.AvailabilityOnline},
		{"two missed heartbeats", ptr(t0), t0.Add(44 * time.Second), domain.AvailabilityOnline},
		{"three missed heartbeats", ptr(t0), t0.Add(45 * time.Second), domain.AvailabilityUnreachable},
		{"long gone", ptr(t0), t0.Add(24 * time.Hour), domain.AvailabilityUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := domain.AvailabilityAt(c.last, c.now); got != c.want {
				t.Fatalf("AvailabilityAt = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNewCapabilitiesValidatesAndSortsTools(t *testing.T) {
	t.Parallel()
	c, err := domain.NewCapabilities("linux", "amd64", true, false, []domain.Tool{
		{Name: "git", Version: "git version 2.43.0"}, {Name: "claude", Version: "1.0.3 (Claude Code)"},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tools[0].Name != "claude" || c.Tools[1].Name != "git" || !c.WSL || c.ReportedAt != t0 {
		t.Fatalf("capabilities = %+v", c)
	}
	if _, ok := c.Tool("codex"); ok {
		t.Fatal("codex reported as found")
	}

	bad := []struct {
		name  string
		os    string
		tools []domain.Tool
	}{
		{"bad os", "Linux;", nil},
		{"bad tool name", "linux", []domain.Tool{{Name: "rm -rf"}}},
		{"control chars in version", "linux", []domain.Tool{{Name: "git", Version: "2.0\x1b[31m"}}},
		{"long version", "linux", []domain.Tool{{Name: "git", Version: strings.Repeat("v", 129)}}},
		{"duplicate tool", "linux", []domain.Tool{{Name: "git"}, {Name: "git"}}},
		{"too many tools", "linux", make([]domain.Tool, 17)},
	}
	for _, b := range bad {
		if _, err := domain.NewCapabilities(b.os, "amd64", false, false, b.tools, t0); err == nil {
			t.Errorf("%s: accepted", b.name)
		}
	}
}

func vol(total, avail int64) domain.Volume {
	return domain.Volume{ID: "/", Role: "workspaces", Path: "/data", TotalBytes: total, AvailableBytes: avail,
		FreeBytes: avail, ReportedAt: t0}
}

const gib = int64(1) << 30

func TestVolumeLevelsUsePercentAndAbsoluteHeadroom(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		v         domain.Volume
		level     domain.Level
		diskHeavy bool
	}{
		{"half full", vol(100*gib, 50*gib), domain.LevelOK, true},
		{"74%", vol(100*gib, 26*gib), domain.LevelOK, true},
		{"75% warning", vol(100*gib, 25*gib), domain.LevelWarning, true},
		{"85% cleanup recommended", vol(100*gib, 15*gib), domain.LevelHigh, true},
		{"95% critical", vol(100*gib, 5*gib), domain.LevelCritical, false},
		{"under 2 GiB available on a small disk", vol(4*gib, 2*gib-1), domain.LevelCritical, false},
		{"exactly 2 GiB available", vol(4*gib, 2*gib), domain.LevelOK, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.v.Level(); got != c.level {
				t.Fatalf("Level = %q (used %.1f%%), want %q", got, c.v.UsedPercent(), c.level)
			}
			if got := c.v.DiskHeavyAllowed(); got != c.diskHeavy {
				t.Fatalf("DiskHeavyAllowed = %v, want %v", got, c.diskHeavy)
			}
		})
	}
	if got := vol(200, 50).UsedPercent(); got != 75 {
		t.Fatalf("UsedPercent = %v, want 75", got)
	}
}

func TestNewVolumeRejectsInconsistentMetrics(t *testing.T) {
	t.Parallel()
	bad := []domain.Volume{
		{ID: "/", Role: "workspaces", TotalBytes: 0},
		{ID: "/", Role: "workspaces", TotalBytes: 10, AvailableBytes: 11, FreeBytes: 11},
		{ID: "/", Role: "workspaces", TotalBytes: 10, AvailableBytes: -1},
		{ID: "/", Role: "workspaces", TotalBytes: 10, AvailableBytes: 5, FreeBytes: 11},
		{ID: "", Role: "workspaces", TotalBytes: 10},
		{ID: "/", Role: "nope", TotalBytes: 10},
		{ID: "/\n", Role: "workspaces", TotalBytes: 10},
	}
	for i, v := range bad {
		if _, err := domain.NewVolume(v.ID, v.Role, v.Path, v.TotalBytes, v.AvailableBytes, v.FreeBytes, t0); err == nil {
			t.Errorf("case %d accepted: %+v", i, v)
		}
	}
	v, err := domain.NewVolume("/mnt/c", "windows_host", "/mnt/c", 10, 4, 5, t0)
	if err != nil || v.ID != "/mnt/c" || v.ReportedAt != t0 {
		t.Fatalf("NewVolume = %+v, %v", v, err)
	}
}

func TestVolumeStaleWhenMachineOfflineOrReportOld(t *testing.T) {
	t.Parallel()
	v := vol(10, 5)
	if v.Stale(t0.Add(14*time.Minute), true) {
		t.Fatal("fresh report of an online machine is stale")
	}
	if !v.Stale(t0.Add(time.Minute), false) {
		t.Fatal("offline machine's metrics are not stale")
	}
	if !v.Stale(t0.Add(16*time.Minute), true) {
		t.Fatal("old report is not stale")
	}
}

// day builds one hourly sample per day: used bytes on day i.
func days(total int64, used ...int64) []domain.Sample {
	out := make([]domain.Sample, 0, len(used))
	for i, u := range used {
		out = append(out, domain.Sample{Hour: t0.AddDate(0, 0, i-len(used)+1).Truncate(time.Hour), TotalBytes: total, UsedBytes: u})
	}
	return out
}

func TestForecastMedianDailyGrowth(t *testing.T) {
	t.Parallel()
	now := t0.Add(time.Hour)
	t.Run("fewer than three days of points: insufficient data", func(t *testing.T) {
		t.Parallel()
		f := domain.ForecastFrom(days(100*gib, 10*gib, 20*gib), now, 7)
		if f.State != domain.ForecastInsufficientData || f.DaysTo85 != nil {
			t.Fatalf("forecast = %+v", f)
		}
	})
	t.Run("no growth: forecast not needed", func(t *testing.T) {
		t.Parallel()
		f := domain.ForecastFrom(days(100*gib, 50*gib, 50*gib, 49*gib, 50*gib), now, 7)
		if f.State != domain.ForecastNotNeeded || f.GrowthBytesPerDay > 0 {
			t.Fatalf("forecast = %+v", f)
		}
	})
	t.Run("steady growth: days to 85% and 90%", func(t *testing.T) {
		t.Parallel()
		// growth 1,1,10 GiB/day: the median (1 GiB) ignores the one-off spike.
		f := domain.ForecastFrom(days(100*gib, 60*gib, 61*gib, 62*gib, 72*gib), now, 7)
		if f.State != domain.ForecastOK || f.GrowthBytesPerDay != gib {
			t.Fatalf("forecast = %+v", f)
		}
		if f.DaysTo85 == nil || *f.DaysTo85 != 13 || f.DaysTo90 == nil || *f.DaysTo90 != 18 {
			t.Fatalf("days to 85/90 = %v/%v, want 13/18", deref(f.DaysTo85), deref(f.DaysTo90))
		}
	})
	t.Run("threshold already reached", func(t *testing.T) {
		t.Parallel()
		f := domain.ForecastFrom(days(100*gib, 86*gib, 87*gib, 88*gib), now, 7)
		if f.State != domain.ForecastOK || *f.DaysTo85 != 0 || *f.DaysTo90 != 2 {
			t.Fatalf("forecast = %+v (%v/%v)", f, deref(f.DaysTo85), deref(f.DaysTo90))
		}
	})
	t.Run("points outside the window are ignored", func(t *testing.T) {
		t.Parallel()
		s := days(100*gib, 10*gib, 20*gib, 30*gib, 40*gib, 50*gib, 60*gib, 70*gib, 80*gib, 80*gib, 80*gib)
		if f := domain.ForecastFrom(s, now, 3); f.State != domain.ForecastNotNeeded {
			t.Fatalf("3-day window = %+v", f)
		}
		if f := domain.ForecastFrom(s, now, 30); f.State != domain.ForecastOK {
			t.Fatalf("30-day window = %+v", f)
		}
	})
	t.Run("last sample of a day wins", func(t *testing.T) {
		t.Parallel()
		s := days(100*gib, 10*gib, 11*gib, 12*gib)
		s = append(s, domain.Sample{Hour: s[2].Hour.Add(-3 * time.Hour), TotalBytes: 100 * gib, UsedBytes: 99 * gib})
		if f := domain.ForecastFrom(s, now, 7); f.GrowthBytesPerDay != gib {
			t.Fatalf("forecast = %+v", f)
		}
	})
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestLevelRaised(t *testing.T) {
	t.Parallel()
	if !domain.LevelHigh.Above(domain.LevelWarning) || domain.LevelOK.Above(domain.LevelOK) ||
		domain.LevelWarning.Above(domain.LevelCritical) || !domain.LevelWarning.Above("") {
		t.Fatal("level order")
	}
	var ve *domain.ValidationError
	if _, err := domain.NewVolume("/", "x", "", 1, 0, 0, t0); !errors.As(err, &ve) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
}

// KIN-60: a volume counts as recovered (its alert resolves) only below RecoveredPercent, under the warning
// threshold, so a disk hovering around 75% does not flap.
func TestVolumeRecoveredHasHysteresis(t *testing.T) {
	t.Parallel()
	const g = int64(1) << 30
	for _, c := range []struct {
		avail int64
		want  bool
	}{{20 * g, false}, {28 * g, false}, {31 * g, true}, {60 * g, true}} {
		if got := vol(100*g, c.avail).Recovered(); got != c.want {
			t.Errorf("avail %d GiB: Recovered = %v", c.avail/g, got)
		}
	}
	if got := vol(2*g, 1*g).Recovered(); got {
		t.Error("a small disk under the absolute headroom is not recovered")
	}
}

// KIN-76: a runner may declare a parallel-step limit (0 = none), bounded.
func TestCapabilitiesMaxParallelSteps(t *testing.T) {
	t.Parallel()
	c, err := domain.NewCapabilities("linux", "amd64", false, false, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, domain.MaxParallelSteps} {
		if got, err := c.WithMaxParallelSteps(n); err != nil || got.MaxParallelSteps != n {
			t.Errorf("%d: %+v %v", n, got, err)
		}
	}
	for _, n := range []int{-1, domain.MaxParallelSteps + 1} {
		if _, err := c.WithMaxParallelSteps(n); err == nil {
			t.Errorf("%d accepted", n)
		}
	}
}
