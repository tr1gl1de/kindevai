package app

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Alert hooks for the Alert Center (KIN-40). The Control Plane enqueues them in the same transaction as the state
// change; until KIN-40 registers a handler they wait in the outbox's dead-letter view and can be retried then.
const (
	AdapterAlerts          = "alerts"
	TopicRunnerUnreachable = "alerts.runner_unreachable"
	TopicRunnerRecovered   = "alerts.runner_recovered"
	TopicStorageLevel      = "alerts.storage_level"
	TopicStorageRecovered  = "alerts.storage_recovered" // KIN-60: the volume is back under the recovery threshold
)

// Storage history: hourly aggregates kept HistoryTTL, at most HistoryMaxRows rows in total (enough for 8 volumes
// of 100 machines at a 30-day TTL with room to spare; the oldest rows go first when it is exceeded).
const (
	HistoryTTL     = 30 * 24 * time.Hour
	HistoryMaxRows = 200_000
)

// Minimum spacing of accepted telemetry per runner: a runner sending faster is not disconnected, the extra
// messages are dropped so it cannot turn the gateway into a write amplifier.
const (
	minHeartbeatGap = 5 * time.Second
	minReportGap    = 30 * time.Second
)

// TelemetryRepo stores runner liveness, capabilities and machines' storage metrics. Every method runs on the
// caller's transaction (db.Conn).
type TelemetryRepo interface {
	// RecordHeartbeat stores the heartbeat time (also the runner's last_seen_at), clears the unreachable mark and
	// returns when the runner had been marked unreachable (nil if it was not).
	RecordHeartbeat(ctx context.Context, runnerID string, at time.Time) (unreachableSince *time.Time, err error)
	// MarkUnreachable marks every approved runner whose last heartbeat is at or before cutoff and that is not marked
	// yet, and returns their ids.
	MarkUnreachable(ctx context.Context, cutoff, now time.Time) ([]string, error)
	SaveCapabilities(ctx context.Context, runnerID string, c domain.Capabilities) error
	Telemetry(ctx context.Context) (map[string]domain.RunnerTelemetry, error)
	// ReplaceVolumes stores a machine's current volumes (replacing the previous set), adds them to the hourly
	// history and returns the previous set.
	ReplaceVolumes(ctx context.Context, machineID string, vs []domain.Volume) ([]domain.Volume, error)
	Volumes(ctx context.Context) (map[string][]domain.Volume, error)
	// Samples returns the history since the given time: machine → volume role → hourly samples.
	Samples(ctx context.Context, since time.Time) (map[string]map[string][]domain.Sample, error)
	// MachineSamples returns one machine's history since the given time: volume role → hourly samples.
	MachineSamples(ctx context.Context, machineID string, since time.Time) (map[string][]domain.Sample, error)
	// PurgeSamples deletes history older than before and then the oldest rows beyond maxRows.
	PurgeSamples(ctx context.Context, before time.Time, maxRows int) (int64, error)
	// ReplaceSpaceConsumers stores a machine's latest space-consumer measurement (KIN-55).
	ReplaceSpaceConsumers(ctx context.Context, machineID string, m domain.SpaceConsumers) error
	// SpaceConsumers returns a machine's latest measurement, nil if it was never measured.
	SpaceConsumers(ctx context.Context, machineID string) (*domain.SpaceConsumers, error)
}

// TelemetryRunners lists the enrolled runners (names and lifecycle status for the overview).
type TelemetryRunners interface {
	Runners(ctx context.Context) ([]domain.Runner, error)
}

// Enqueuer is the durable outbox; Enqueue joins the caller's transaction.
type Enqueuer interface {
	Enqueue(ctx context.Context, m messaging.Message) error
}

// Telemetry implements runner heartbeat, capabilities and storage reporting, the unreachable sweep and the
// Storage Overview (KIN-20).
type Telemetry struct {
	Repo    TelemetryRepo
	Runners TelemetryRunners
	Tx      TxRunner
	Outbox  Enqueuer
	Now     func() time.Time
	Logger  *slog.Logger

	mu      sync.Mutex
	last    map[string]time.Time // "<kind>/<machine>/<session>" → last stored message
	started time.Time            // set by Run: the unreachable sweep waits StartupGrace after it
}

// StartupGrace: after the Control Plane starts, runners need up to the longest reconnect backoff (1 min) plus a
// heartbeat to come back; until then the sweep marks nobody unreachable, so a restart does not raise an alert storm.
const StartupGrace = domain.UnreachableAfter + time.Minute

func (t *Telemetry) logger() *slog.Logger {
	if t.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return t.Logger
}

// throttled reports whether a message of this kind from this connection arrives within gap of the last stored
// one. Keyed by session, so a fresh connection's first reports are never dropped.
func (t *Telemetry) throttled(kind, machine, session string, gap time.Duration, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	prev, ok := t.last[kind+"/"+machine+"/"+session]
	return ok && now.Sub(prev) < gap
}

// stored records a successfully stored message; old entries (ended sessions) are pruned as the map grows.
func (t *Telemetry) stored(kind, machine, session string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if len(t.last) >= 4096 {
		for k, at := range t.last {
			if now.Sub(at) > 10*time.Minute {
				delete(t.last, k)
			}
		}
	}
	t.last[kind+"/"+machine+"/"+session] = now
}

// guarded runs store unless throttled and records it only when it succeeded.
func (t *Telemetry) guarded(kind, machine, session string, gap time.Duration, now time.Time, store func() error) error {
	if t.throttled(kind, machine, session, gap, now) {
		return nil
	}
	if err := store(); err != nil {
		return err
	}
	t.stored(kind, machine, session, now)
	return nil
}

func (t *Telemetry) alert(ctx context.Context, topic, key string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := t.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}

func unixNano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// Heartbeat records a runner.heartbeat of an authorized runner session, stamped with the Control Plane's clock.
func (t *Telemetry) Heartbeat(ctx context.Context, runnerID, session string) error {
	now := t.Now()
	return t.guarded("heartbeat", runnerID, session, minHeartbeatGap, now, func() error { return t.heartbeat(ctx, runnerID, now) })
}

func (t *Telemetry) heartbeat(ctx context.Context, runnerID string, now time.Time) error {
	return t.Tx.InTx(ctx, func(ctx context.Context) error {
		since, err := t.Repo.RecordHeartbeat(ctx, runnerID, now)
		if err != nil || since == nil {
			return err
		}
		return t.alert(ctx, TopicRunnerRecovered, "runner/"+runnerID+"/recovered/"+unixNano(*since),
			map[string]any{"runnerId": runnerID, "unreachableSince": since.UTC(), "recoveredAt": now.UTC()})
	})
}

// Sweep marks runners that missed domain.MissedHeartbeats heartbeats UNREACHABLE and raises one alert each
// (not during StartupGrace after Run started).
func (t *Telemetry) Sweep(ctx context.Context) error {
	now := t.Now()
	t.mu.Lock()
	started := t.started
	t.mu.Unlock()
	if !started.IsZero() && now.Sub(started) < StartupGrace {
		return nil
	}
	return t.Tx.InTx(ctx, func(ctx context.Context) error {
		ids, err := t.Repo.MarkUnreachable(ctx, now.Add(-domain.UnreachableAfter), now)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := t.alert(ctx, TopicRunnerUnreachable, "runner/"+id+"/unreachable/"+unixNano(now),
				map[string]any{"runnerId": id, "since": now.UTC(), "missedHeartbeats": domain.MissedHeartbeats}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Capabilities stores the capability report of a runner session.
func (t *Telemetry) Capabilities(ctx context.Context, runnerID, session string, c domain.Capabilities) error {
	return t.guarded("capabilities", runnerID, session, minReportGap, t.Now(), func() error {
		return t.Repo.SaveCapabilities(ctx, runnerID, c)
	})
}

// Storage stores a machine's volume metrics (a runner's report or the Control Plane's own) and raises an alert
// when a volume's level rises to warning or above, and a recovery event when it falls back under
// domain.RecoveredPercent. session identifies the reporting connection (LocalSession for
// the Control Plane's own metrics).
func (t *Telemetry) Storage(ctx context.Context, machineID, session string, vs []domain.Volume) error {
	if len(vs) > domain.MaxVolumes {
		return &domain.ValidationError{Field: "volumes", Code: "too_many"}
	}
	roles := map[string]bool{}
	for _, v := range vs {
		if roles[v.Role] {
			return &domain.ValidationError{Field: "volumes", Code: "duplicate_role"}
		}
		roles[v.Role] = true
	}
	return t.guarded("storage", machineID, session, minReportGap, t.Now(), func() error { return t.storage(ctx, machineID, vs) })
}

// LocalSession is the session of the Control Plane's own storage metrics.
const LocalSession = "local"

func (t *Telemetry) storage(ctx context.Context, machineID string, vs []domain.Volume) error {
	return t.Tx.InTx(ctx, func(ctx context.Context) error {
		prev, err := t.Repo.ReplaceVolumes(ctx, machineID, vs)
		if err != nil {
			return err
		}
		for _, v := range vs {
			was, wasRecovered := domain.Level(""), true
			if i := slices.IndexFunc(prev, func(p domain.Volume) bool { return p.Role == v.Role }); i >= 0 {
				was, wasRecovered = prev[i].Level(), prev[i].Recovered()
			}
			if v.Recovered() && !wasRecovered {
				key := "storage/" + machineID + "/" + v.Role + "/recovered/" + unixNano(v.ReportedAt)
				if err := t.alert(ctx, TopicStorageRecovered, key, map[string]any{"machineId": machineID, "role": v.Role,
					"volumeId": v.ID, "usedPercent": v.UsedPercent()}); err != nil {
					return err
				}
				continue
			}
			lvl := v.Level()
			if lvl == domain.LevelOK || !lvl.Above(was) {
				continue
			}
			// One alert per rise: falling back and rising again later is a new event.
			key := "storage/" + machineID + "/" + v.Role + "/" + string(lvl) + "/" + unixNano(v.ReportedAt)
			if err := t.alert(ctx, TopicStorageLevel, key, map[string]any{"machineId": machineID, "role": v.Role,
				"volumeId": v.ID, "level": lvl, "usedPercent": v.UsedPercent(), "availableBytes": v.AvailableBytes,
				"totalBytes": v.TotalBytes, "diskHeavyAllowed": v.DiskHeavyAllowed()}); err != nil {
				return err
			}
		}
		return nil
	})
}

// DiskHeavyAllowed is the scheduler's flag (KIN-23): false while a volume of the runner is critical (≥95% or
// under 2 GiB available) by its last report. Without any metrics yet the runner is not blocked. It does not look at
// liveness: the scheduler combines it with Presence (an unreachable runner gets no work anyway).
func (t *Telemetry) DiskHeavyAllowed(ctx context.Context, runnerID string) (bool, error) {
	all, err := t.Repo.Volumes(ctx)
	if err != nil {
		return false, err
	}
	return !slices.ContainsFunc(all[runnerID], func(v domain.Volume) bool { return !v.DiskHeavyAllowed() }), nil
}

// Presence is a runner's liveness and capabilities as shown with the runner.
type Presence struct {
	Availability    domain.Availability
	LastHeartbeatAt *time.Time
	Capabilities    *domain.Capabilities
}

// Presence returns the liveness of every runner that ever sent telemetry.
func (t *Telemetry) Presence(ctx context.Context) (map[string]Presence, error) {
	tel, err := t.Repo.Telemetry(ctx)
	if err != nil {
		return nil, err
	}
	now := t.Now()
	out := make(map[string]Presence, len(tel))
	for id, x := range tel {
		out[id] = presence(x, now)
	}
	return out, nil
}

func presence(x domain.RunnerTelemetry, now time.Time) Presence {
	a := domain.AvailabilityAt(x.LastHeartbeatAt, now)
	if x.UnreachableSince != nil {
		a = domain.AvailabilityUnreachable
	}
	return Presence{Availability: a, LastHeartbeatAt: x.LastHeartbeatAt, Capabilities: x.Capabilities}
}

// Machine kinds of the Storage Overview.
const (
	MachineControlPlane = "control_plane"
	MachineRunner       = "runner"
)

// VolumeOverview is a volume with its level, staleness and forecasts over 7 and 30 days.
type VolumeOverview struct {
	domain.Volume
	UsedPercent float64
	Level       domain.Level
	Stale       bool
	Forecast7   domain.Forecast
	Forecast30  domain.Forecast
}

// MachineOverview is one row of the Storage Overview.
type MachineOverview struct {
	ID               string
	Kind             string
	Name             string        // the runner's hostname; empty for the Control Plane
	Status           domain.Status // runners only
	Availability     domain.Availability
	LastHeartbeatAt  *time.Time
	DiskHeavyAllowed bool
	Volumes          []VolumeOverview
}

// Overview is the Storage Overview: the Control Plane first, then approved and disabled runners by name.
type Overview struct {
	GeneratedAt time.Time
	Machines    []MachineOverview
}

// Overview builds the Storage Overview (Owner only).
func (t *Telemetry) Overview(ctx context.Context, actor auth.Principal) (Overview, error) {
	if err := ownerOnly(actor); err != nil {
		return Overview{}, err
	}
	ov, _, err := t.overview(ctx, "")
	return ov, err
}

// overview builds the overview of every machine, or of machine only (then it also returns only's 30-day history,
// so one machine's drill-down never loads every machine's history).
func (t *Telemetry) overview(ctx context.Context, only string) (Overview, map[string][]domain.Sample, error) {
	now := t.Now()
	runners, err := t.Runners.Runners(ctx)
	if err != nil {
		return Overview{}, nil, err
	}
	tel, err := t.Repo.Telemetry(ctx)
	if err != nil {
		return Overview{}, nil, err
	}
	vols, err := t.Repo.Volumes(ctx)
	if err != nil {
		return Overview{}, nil, err
	}
	since := now.AddDate(0, 0, -30)
	var hist map[string]map[string][]domain.Sample
	if only == "" {
		hist, err = t.Repo.Samples(ctx, since)
	} else {
		var h map[string][]domain.Sample
		h, err = t.Repo.MachineSamples(ctx, only, since)
		hist = map[string]map[string][]domain.Sample{only: h}
	}
	if err != nil {
		return Overview{}, nil, err
	}
	build := func(m MachineOverview) MachineOverview {
		online := m.Availability == domain.AvailabilityOnline
		m.DiskHeavyAllowed, m.Volumes = true, []VolumeOverview{}
		for _, v := range vols[m.ID] {
			s := hist[m.ID][v.Role]
			m.Volumes = append(m.Volumes, VolumeOverview{Volume: v, UsedPercent: v.UsedPercent(), Level: v.Level(),
				Stale: v.Stale(now, online), Forecast7: domain.ForecastFrom(s, now, 7), Forecast30: domain.ForecastFrom(s, now, 30)})
			m.DiskHeavyAllowed = m.DiskHeavyAllowed && v.DiskHeavyAllowed()
		}
		slices.SortFunc(m.Volumes, func(a, b VolumeOverview) int { return cmp.Compare(roleOrder(a.Role), roleOrder(b.Role)) })
		return m
	}
	out := Overview{GeneratedAt: now, Machines: []MachineOverview{}}
	if only == "" || only == domain.ControlPlaneMachine {
		out.Machines = append(out.Machines, build(MachineOverview{ID: domain.ControlPlaneMachine, Kind: MachineControlPlane,
			Availability: domain.AvailabilityOnline}))
	}
	var rs []MachineOverview
	for _, r := range runners {
		if r.Status != domain.StatusApproved && r.Status != domain.StatusDisabled || only != "" && r.ID != only {
			continue
		}
		p := presence(tel[r.ID], now)
		rs = append(rs, build(MachineOverview{ID: r.ID, Kind: MachineRunner, Name: r.Metadata.Hostname, Status: r.Status,
			Availability: p.Availability, LastHeartbeatAt: p.LastHeartbeatAt}))
	}
	slices.SortFunc(rs, func(a, b MachineOverview) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	out.Machines = append(out.Machines, rs...)
	return out, hist[only], nil
}

// SpaceConsumers stores a runner's space-consumer measurement (KIN-55). The runner measures only when its
// operator allowed it; the Control Plane never asks for one. Stamped with the Control Plane's clock.
func (t *Telemetry) SpaceConsumers(ctx context.Context, machineID, session string, items []domain.SpaceConsumer) error {
	now := t.Now()
	m, err := domain.NewSpaceConsumers(items, now)
	if err != nil {
		return err
	}
	// Not throttled: a runner measures at most every few hours, and a dropped measurement would be lost for that long.
	return t.Tx.InTx(ctx, func(ctx context.Context) error { return t.Repo.ReplaceSpaceConsumers(ctx, machineID, m) })
}

// HistoryDays are the history windows of the Storage page.
var HistoryDays = []int{7, 30}

// MachineDetail is Infrastructure → Storage for one machine: its overview row (all volumes with both forecasts),
// the hourly history of the window and the latest space-consumer measurement (nil if never measured).
type MachineDetail struct {
	MachineOverview
	Days      int
	History   map[string][]domain.Sample // volume role → hourly samples, oldest first
	Consumers *domain.SpaceConsumers
}

// Machine builds the Storage drill-down of one machine of the overview (Owner only); days is 7 or 30.
func (t *Telemetry) Machine(ctx context.Context, actor auth.Principal, machineID string, days int) (MachineDetail, error) {
	if err := ownerOnly(actor); err != nil {
		return MachineDetail{}, err
	}
	if !slices.Contains(HistoryDays, days) {
		return MachineDetail{}, &domain.ValidationError{Field: "days", Code: "unsupported"}
	}
	if machineID == "" {
		return MachineDetail{}, domain.ErrNotFound
	}
	ov, hist, err := t.overview(ctx, machineID)
	if err != nil {
		return MachineDetail{}, err
	}
	if len(ov.Machines) != 1 {
		return MachineDetail{}, domain.ErrNotFound
	}
	from := ov.GeneratedAt.AddDate(0, 0, -days).Truncate(time.Hour)
	h := make(map[string][]domain.Sample, len(hist))
	for role, ss := range hist {
		h[role] = slices.DeleteFunc(slices.Clone(ss), func(s domain.Sample) bool { return s.Hour.Before(from) })
	}
	c, err := t.Repo.SpaceConsumers(ctx, machineID)
	if err != nil {
		return MachineDetail{}, err
	}
	return MachineDetail{MachineOverview: ov.Machines[0], Days: days, History: h, Consumers: c}, nil
}

// roleOrder lists the working volume (data, workspaces) before the WSL2 host volume.
func roleOrder(role string) int {
	if role == domain.RoleWindowsHost {
		return 1
	}
	return 0
}

// Purge applies the history TTL and row limit.
func (t *Telemetry) Purge(ctx context.Context) error {
	n, err := t.Repo.PurgeSamples(ctx, t.Now().Add(-HistoryTTL), HistoryMaxRows)
	if err != nil {
		return err
	}
	if n > 0 {
		t.logger().Info("storage history purged", "rows", n)
	}
	return nil
}

// LocalProbe measures the Control Plane's own volumes.
type LocalProbe func() ([]domain.Volume, error)

// Run is the background loop until ctx ends: the unreachable sweep every heartbeat interval, the Control Plane's
// own storage every 5 minutes (and at start) and the history purge hourly.
func (t *Telemetry) Run(ctx context.Context, probe LocalProbe) {
	collect := func() {
		vs, err := probe()
		if err == nil {
			err = t.Storage(ctx, domain.ControlPlaneMachine, LocalSession, vs)
		}
		if err != nil && ctx.Err() == nil {
			t.logger().Warn("collecting control plane storage metrics", "err", err)
		}
	}
	t.mu.Lock()
	t.started = t.Now()
	t.mu.Unlock()
	if probe != nil {
		collect()
	}
	sweep := time.NewTicker(domain.HeartbeatInterval)
	storage := time.NewTicker(5 * time.Minute)
	purge := time.NewTicker(time.Hour)
	defer sweep.Stop()
	defer storage.Stop()
	defer purge.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			err = t.Sweep(ctx)
		case <-storage.C:
			if probe != nil {
				collect()
			}
		case <-purge.C:
			err = t.Purge(ctx)
		}
		if err != nil && ctx.Err() == nil {
			t.logger().Warn("runner telemetry maintenance", "err", err)
		}
	}
}
