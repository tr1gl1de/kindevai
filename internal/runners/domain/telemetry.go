package domain

import (
	"cmp"
	"math"
	"regexp"
	"slices"
	"time"
)

// Runner telemetry (KIN-20, KB KIN-A-18/KIN-A-26): liveness from heartbeats, reported capabilities and disk
// metrics with their thresholds and fill-up forecast.

// HeartbeatInterval and MissedHeartbeats: a runner whose last heartbeat is UnreachableAfter old is UNREACHABLE.
// UNREACHABLE means "state unknown", not "stopped": its work must be reconciled before anything is retried.
const (
	HeartbeatInterval = 15 * time.Second
	MissedHeartbeats  = 3
	UnreachableAfter  = MissedHeartbeats * HeartbeatInterval
)

// Availability is a runner's liveness derived from its last heartbeat (independent of its lifecycle Status).
type Availability string

const (
	AvailabilityNever       Availability = "never_connected" // no heartbeat received yet
	AvailabilityOnline      Availability = "online"
	AvailabilityUnreachable Availability = "unreachable" // MissedHeartbeats heartbeats missed
)

// AvailabilityAt derives the availability at now from the last heartbeat time.
func AvailabilityAt(lastHeartbeat *time.Time, now time.Time) Availability {
	switch {
	case lastHeartbeat == nil:
		return AvailabilityNever
	case now.Sub(*lastHeartbeat) >= UnreachableAfter:
		return AvailabilityUnreachable
	}
	return AvailabilityOnline
}

// Tool is a CLI the runner found, with its reported version (informational).
type Tool struct {
	Name    string
	Version string
}

// Capabilities is what a runner reported it can do. It is self-reported and informational: it never grants
// anything (KIN-A-17); the scheduler may only use it to skip runners that cannot run a step.
type Capabilities struct {
	OS           string
	Arch         string
	WSL          bool
	StrictEgress bool
	Tools        []Tool // sorted by name, unique
	// MaxParallelSteps is the parallel-step limit the runner declared (KIN-76): it only ever restricts the runner
	// itself; 0 = none.
	MaxParallelSteps int
	ReportedAt       time.Time
}

// MaxParallelSteps bounds a declared parallel-step limit.
const MaxParallelSteps = 64

// WithMaxParallelSteps returns c with the runner's declared parallel-step limit n (0..MaxParallelSteps).
func (c Capabilities) WithMaxParallelSteps(n int) (Capabilities, error) {
	if n < 0 || n > MaxParallelSteps {
		return Capabilities{}, &ValidationError{Field: "maxParallelSteps", Code: "out_of_range"}
	}
	c.MaxParallelSteps = n
	return c, nil
}

// MaxTools bounds the reported tool list.
const MaxTools = 16

var toolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// NewCapabilities validates a runner's capability report.
func NewCapabilities(os, arch string, wsl, strictEgress bool, tools []Tool, at time.Time) (Capabilities, error) {
	if !platformRe.MatchString(os) {
		return Capabilities{}, &ValidationError{Field: "os", Code: "os_format"}
	}
	if !platformRe.MatchString(arch) {
		return Capabilities{}, &ValidationError{Field: "arch", Code: "arch_format"}
	}
	if len(tools) > MaxTools {
		return Capabilities{}, &ValidationError{Field: "tools", Code: "too_many"}
	}
	ts := slices.Clone(tools)
	for _, t := range ts {
		if !toolNameRe.MatchString(t.Name) {
			return Capabilities{}, &ValidationError{Field: "tools", Code: "tool_name_format"}
		}
		if !printable(t.Version, 128) {
			return Capabilities{}, &ValidationError{Field: "tools", Code: "tool_version_format"}
		}
	}
	slices.SortFunc(ts, func(a, b Tool) int { return cmp.Compare(a.Name, b.Name) })
	if len(slices.CompactFunc(slices.Clone(ts), func(a, b Tool) bool { return a.Name == b.Name })) != len(ts) {
		return Capabilities{}, &ValidationError{Field: "tools", Code: "duplicate_tool"}
	}
	if ts == nil {
		ts = []Tool{}
	}
	return Capabilities{OS: os, Arch: arch, WSL: wsl, StrictEgress: strictEgress, Tools: ts, ReportedAt: at}, nil
}

// Tool returns the named tool if the runner reported it.
func (c Capabilities) Tool(name string) (Tool, bool) {
	i := slices.IndexFunc(c.Tools, func(t Tool) bool { return t.Name == name })
	if i < 0 {
		return Tool{}, false
	}
	return c.Tools[i], true
}

// ControlPlaneMachine is the machine id of the Control Plane's own storage metrics (runner ids never contain
// a hyphen-separated lowercase word, so it cannot collide).
const ControlPlaneMachine = "control-plane"

// Volume roles.
const (
	RoleWorkspaces  = "workspaces"   // runner: the filesystem holding runner-data
	RoleWindowsHost = "windows_host" // runner on WSL2: the Windows volume holding ext4.vhdx
	RoleData        = "data"         // Control Plane: the filesystem holding its data directory
)

// MaxVolumes bounds the volumes one machine reports.
const MaxVolumes = 8

// Storage thresholds (KIN-A-26): percentages of used space and an absolute headroom.
const (
	WarningPercent  = 75 // warning
	HighPercent     = 85 // cleanup recommended
	CriticalPercent = 95 // critical: no new disk-heavy steps
	// RecoveredPercent (KIN-60): a volume's alert resolves itself only once it is back under this, 5 points below
	// the warning threshold, so a disk hovering around 75% does not raise and resolve alerts on every report.
	RecoveredPercent = 70
	// MinHeadroomBytes: less available space than this is critical whatever the percentage.
	MinHeadroomBytes = int64(2) << 30
	// StorageStaleAfter: metrics older than this (three missed reports at the 5-minute cadence) are stale.
	StorageStaleAfter = 15 * time.Minute
)

// Level is the storage alert level of a volume.
type Level string

const (
	LevelOK       Level = "ok"
	LevelWarning  Level = "warning"
	LevelHigh     Level = "high"
	LevelCritical Level = "critical"
)

func (l Level) rank() int {
	return slices.Index([]Level{LevelOK, LevelWarning, LevelHigh, LevelCritical}, l)
}

// Above reports whether l is more severe than o (an unknown or empty level ranks lowest).
func (l Level) Above(o Level) bool { return l.rank() > o.rank() }

// Volume is one filesystem's metrics as last reported by a machine.
type Volume struct {
	ID             string // mount point (mount_or_volume_id)
	Role           string
	Path           string // the directory measured
	TotalBytes     int64
	AvailableBytes int64 // writable by an unprivileged process
	FreeBytes      int64 // including reserved blocks
	ReportedAt     time.Time
}

// NewVolume validates reported metrics.
func NewVolume(id, role, path string, total, available, free int64, at time.Time) (Volume, error) {
	switch {
	case id == "" || !printable(id, 256):
		return Volume{}, &ValidationError{Field: "volumes", Code: "volume_id_format"}
	case role != RoleWorkspaces && role != RoleWindowsHost && role != RoleData:
		return Volume{}, &ValidationError{Field: "volumes", Code: "volume_role"}
	case !printable(path, 512):
		return Volume{}, &ValidationError{Field: "volumes", Code: "volume_path_format"}
	case total <= 0 || available < 0 || free < 0 || available > total || free > total:
		return Volume{}, &ValidationError{Field: "volumes", Code: "volume_metrics"}
	}
	return Volume{ID: id, Role: role, Path: path, TotalBytes: total, AvailableBytes: available, FreeBytes: free,
		ReportedAt: at}, nil
}

// UsedBytes is the space not available to an unprivileged writer (reserved blocks count as used).
func (v Volume) UsedBytes() int64 { return v.TotalBytes - v.AvailableBytes }

// UsedPercent is UsedBytes as a percentage of the total (disk_used_percent).
func (v Volume) UsedPercent() float64 {
	if v.TotalBytes <= 0 {
		return 0
	}
	return float64(v.UsedBytes()) * 100 / float64(v.TotalBytes)
}

// Level applies the thresholds; the absolute headroom makes a small disk critical before 95%.
func (v Volume) Level() Level {
	p := v.UsedPercent()
	switch {
	case p >= CriticalPercent || v.AvailableBytes < MinHeadroomBytes:
		return LevelCritical
	case p >= HighPercent:
		return LevelHigh
	case p >= WarningPercent:
		return LevelWarning
	}
	return LevelOK
}

// Recovered reports whether the volume is clear of every level with the hysteresis margin: its level alert
// resolves itself when it gets here.
func (v Volume) Recovered() bool { return v.Level() == LevelOK && v.UsedPercent() < RecoveredPercent }

// DiskHeavyAllowed is false at the critical level: the scheduler must not start disk-heavy steps there.
func (v Volume) DiskHeavyAllowed() bool { return v.Level() != LevelCritical }

// Stale reports whether the metrics may no longer describe the disk: the machine is offline or the report is old.
func (v Volume) Stale(now time.Time, online bool) bool {
	return !online || now.Sub(v.ReportedAt) > StorageStaleAfter
}

// Sample is one hourly aggregate of a volume's history (the last report of that hour).
type Sample struct {
	Hour       time.Time // truncated to the hour, UTC
	TotalBytes int64
	UsedBytes  int64
}

// ForecastState says whether a fill-up forecast could be made.
type ForecastState string

const (
	ForecastInsufficientData ForecastState = "insufficient_data" // fewer than MinForecastDays days of points
	ForecastNotNeeded        ForecastState = "not_needed"        // median growth is zero or negative
	ForecastOK               ForecastState = "ok"
)

// MinForecastDays: a forecast needs points on at least this many distinct days.
const MinForecastDays = 3

// Forecast estimates when a volume reaches 85% and 90% used. Days are rounded up; 0 means already reached.
type Forecast struct {
	State             ForecastState
	WindowDays        int
	GrowthBytesPerDay int64
	DaysTo85          *int
	DaysTo90          *int
}

// ForecastFrom applies the KIN-20 model to the samples of the last windowDays days: the last sample of each UTC
// day is that day's point, the growth is the median of the day-to-day changes (per day), so one-off spikes and
// cleanups do not dominate; the latest point is the starting level. Samples may be unordered.
func ForecastFrom(samples []Sample, now time.Time, windowDays int) Forecast {
	f := Forecast{State: ForecastInsufficientData, WindowDays: windowDays}
	from := now.AddDate(0, 0, -windowDays)
	byDay := map[time.Time]Sample{}
	for _, s := range samples {
		if s.Hour.Before(from) || s.Hour.After(now) || s.TotalBytes <= 0 {
			continue
		}
		d := s.Hour.UTC().Truncate(24 * time.Hour)
		if cur, ok := byDay[d]; !ok || s.Hour.After(cur.Hour) {
			byDay[d] = s
		}
	}
	if len(byDay) < MinForecastDays {
		return f
	}
	keys := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		keys = append(keys, d)
	}
	slices.SortFunc(keys, func(a, b time.Time) int { return a.Compare(b) })
	growth := make([]float64, 0, len(keys)-1)
	for i := 1; i < len(keys); i++ {
		gap := keys[i].Sub(keys[i-1]).Hours() / 24
		growth = append(growth, float64(byDay[keys[i]].UsedBytes-byDay[keys[i-1]].UsedBytes)/gap)
	}
	slices.Sort(growth)
	n := len(growth)
	med := growth[n/2]
	if n%2 == 0 {
		med = (growth[n/2-1] + growth[n/2]) / 2
	}
	f.GrowthBytesPerDay = int64(math.Round(med))
	if f.GrowthBytesPerDay <= 0 {
		f.State = ForecastNotNeeded
		return f
	}
	last := byDay[keys[len(keys)-1]]
	daysTo := func(percent float64) *int {
		left := percent/100*float64(last.TotalBytes) - float64(last.UsedBytes)
		d := 0
		if left > 0 {
			d = int(math.Ceil(left / med))
		}
		return &d
	}
	f.State, f.DaysTo85, f.DaysTo90 = ForecastOK, daysTo(HighPercent), daysTo(90)
	return f
}

// RunnerTelemetry is what the Control Plane last heard from a runner.
type RunnerTelemetry struct {
	LastHeartbeatAt  *time.Time
	UnreachableSince *time.Time // set when the sweep found MissedHeartbeats heartbeats missing; cleared by a heartbeat
	Capabilities     *Capabilities
}
