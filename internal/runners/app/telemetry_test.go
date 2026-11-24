package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// memTelemetry is an in-memory TelemetryRepo.
type memTelemetry struct {
	mu      sync.Mutex
	hb      map[string]time.Time
	down    map[string]time.Time
	caps    map[string]domain.Capabilities
	vols    map[string][]domain.Volume
	samples map[string]map[string][]domain.Sample
	status  map[string]domain.Status
}

func newMemTelemetry() *memTelemetry {
	return &memTelemetry{hb: map[string]time.Time{}, down: map[string]time.Time{}, caps: map[string]domain.Capabilities{},
		vols: map[string][]domain.Volume{}, samples: map[string]map[string][]domain.Sample{}, status: map[string]domain.Status{}}
}

func (m *memTelemetry) RecordHeartbeat(_ context.Context, id string, at time.Time) (*time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hb[id] = at
	if d, ok := m.down[id]; ok {
		delete(m.down, id)
		return &d, nil
	}
	return nil, nil
}

func (m *memTelemetry) MarkUnreachable(_ context.Context, cutoff, now time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for id, at := range m.hb {
		if _, down := m.down[id]; !down && !at.After(cutoff) && m.status[id] == domain.StatusApproved {
			m.down[id] = now
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (m *memTelemetry) SaveCapabilities(_ context.Context, id string, c domain.Capabilities) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.caps[id] = c
	return nil
}

func (m *memTelemetry) Telemetry(context.Context) (map[string]domain.RunnerTelemetry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]domain.RunnerTelemetry{}
	for id, at := range m.hb {
		x := out[id]
		x.LastHeartbeatAt = &at
		if d, ok := m.down[id]; ok {
			x.UnreachableSince = &d
		}
		out[id] = x
	}
	for id, c := range m.caps {
		x := out[id]
		x.Capabilities = &c
		out[id] = x
	}
	return out, nil
}

func (m *memTelemetry) ReplaceVolumes(_ context.Context, machine string, vs []domain.Volume) ([]domain.Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.vols[machine]
	m.vols[machine] = vs
	if m.samples[machine] == nil {
		m.samples[machine] = map[string][]domain.Sample{}
	}
	for _, v := range vs {
		m.samples[machine][v.Role] = append(m.samples[machine][v.Role], domain.Sample{Hour: v.ReportedAt.Truncate(time.Hour),
			TotalBytes: v.TotalBytes, UsedBytes: v.UsedBytes()})
	}
	return prev, nil
}

func (m *memTelemetry) Volumes(context.Context) (map[string][]domain.Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneMap(m.vols), nil
}

func cloneMap[V any](in map[string]V) map[string]V {
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (m *memTelemetry) Samples(_ context.Context, _ time.Time) (map[string]map[string][]domain.Sample, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneMap(m.samples), nil
}

func (m *memTelemetry) PurgeSamples(context.Context, time.Time, int) (int64, error) { return 0, nil }

type memOutbox struct {
	mu   sync.Mutex
	msgs []messaging.Message
}

func (o *memOutbox) Enqueue(_ context.Context, m messaging.Message) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if slices.ContainsFunc(o.msgs, func(x messaging.Message) bool { return x.Key == m.Key }) {
		return nil
	}
	o.msgs = append(o.msgs, m)
	return nil
}

func (o *memOutbox) topics() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, m := range o.msgs {
		out = append(out, m.Topic)
	}
	return out
}

type telFixture struct {
	*fixture
	tel    *Telemetry
	trepo  *memTelemetry
	outbox *memOutbox
	clock  *clock
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTelFixture(t *testing.T) *telFixture {
	t.Helper()
	f := newFixture(t)
	c := &clock{now: f.now}
	tf := &telFixture{fixture: f, trepo: newMemTelemetry(), outbox: &memOutbox{}, clock: c}
	tf.tel = &Telemetry{Repo: tf.trepo, Runners: f.repo, Tx: fakeTx{}, Outbox: tf.outbox, Now: c.Now}
	return tf
}

// approved enrolls and approves a runner.
func (f *telFixture) approved(t *testing.T, key string) domain.Runner {
	t.Helper()
	r := f.enrolled(t, key)
	v, err := f.svc.Approve(t.Context(), owner, r.ID, []string{"p1"})
	if err != nil {
		t.Fatal(err)
	}
	f.trepo.mu.Lock()
	f.trepo.status[r.ID] = domain.StatusApproved
	f.trepo.mu.Unlock()
	return v.Runner
}

func TestHeartbeatsKeepRunnerOnlineAndThreeMissedMakeItUnreachable(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	r := f.approved(t, keyA)
	if err := f.tel.Heartbeat(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	p, err := f.tel.Presence(t.Context())
	if err != nil || p[r.ID].Availability != domain.AvailabilityOnline || p[r.ID].LastHeartbeatAt == nil {
		t.Fatalf("presence after heartbeat = %+v, %v", p[r.ID], err)
	}

	f.clock.add(44 * time.Second)
	if err := f.tel.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.outbox.topics()) != 0 {
		t.Fatalf("alerts after two missed heartbeats: %v", f.outbox.topics())
	}

	f.clock.add(time.Second)
	if err := f.tel.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.tel.Sweep(t.Context()); err != nil { // a second sweep does not alert again
		t.Fatal(err)
	}
	p, _ = f.tel.Presence(t.Context())
	if p[r.ID].Availability != domain.AvailabilityUnreachable {
		t.Fatalf("availability after 3 missed heartbeats = %q", p[r.ID].Availability)
	}
	if got := f.outbox.topics(); !slices.Equal(got, []string{TopicRunnerUnreachable}) {
		t.Fatalf("alerts = %v", got)
	}
	m := f.outbox.msgs[0]
	var payload map[string]any
	if err := json.Unmarshal(m.Payload, &payload); err != nil || payload["runnerId"] != r.ID || m.Adapter != AdapterAlerts {
		t.Fatalf("alert = %+v (%s)", m, m.Payload)
	}

	f.clock.add(time.Minute)
	if err := f.tel.Heartbeat(t.Context(), r.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.outbox.topics(); !slices.Equal(got, []string{TopicRunnerUnreachable, TopicRunnerRecovered}) {
		t.Fatalf("alerts after recovery = %v", got)
	}
	p, _ = f.tel.Presence(t.Context())
	if p[r.ID].Availability != domain.AvailabilityOnline {
		t.Fatalf("availability after recovery = %q", p[r.ID].Availability)
	}
}

func TestHeartbeatFloodIsThrottled(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	r := f.approved(t, keyA)
	_ = f.tel.Heartbeat(t.Context(), r.ID)
	first := f.trepo.hb[r.ID]
	f.clock.add(time.Second)
	_ = f.tel.Heartbeat(t.Context(), r.ID)
	if !f.trepo.hb[r.ID].Equal(first) {
		t.Fatal("a heartbeat 1s after the previous one was written")
	}
	f.clock.add(5 * time.Second)
	_ = f.tel.Heartbeat(t.Context(), r.ID)
	if f.trepo.hb[r.ID].Equal(first) {
		t.Fatal("a heartbeat 6s after the previous one was dropped")
	}
}

func TestCapabilitiesAreStoredAndShown(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	r := f.approved(t, keyA)
	c, _ := domain.NewCapabilities("linux", "amd64", false, false, []domain.Tool{{Name: "claude", Version: "1.0.3"}}, f.clock.Now())
	if err := f.tel.Capabilities(t.Context(), r.ID, c); err != nil {
		t.Fatal(err)
	}
	p, _ := f.tel.Presence(t.Context())
	if p[r.ID].Capabilities == nil || p[r.ID].Capabilities.Tools[0].Name != "claude" {
		t.Fatalf("capabilities = %+v", p[r.ID].Capabilities)
	}
}

func volume(t *testing.T, role string, total, avail int64, at time.Time) domain.Volume {
	t.Helper()
	v, err := domain.NewVolume("/", role, "/data", total, avail, avail, at)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

const gib = int64(1) << 30

func TestStorageLevelRiseAlertsOncePerLevelAndCriticalBlocksDiskHeavy(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	r := f.approved(t, keyA)
	ctx := t.Context()
	if ok, err := f.tel.DiskHeavyAllowed(ctx, r.ID); err != nil || !ok {
		t.Fatalf("no metrics yet: DiskHeavyAllowed = %v, %v", ok, err)
	}
	report := func(avail int64) {
		t.Helper()
		f.clock.add(time.Minute)
		if err := f.tel.Storage(ctx, r.ID, []domain.Volume{volume(t, domain.RoleWorkspaces, 100*gib, avail, f.clock.Now())}); err != nil {
			t.Fatal(err)
		}
	}
	report(50 * gib)
	report(20 * gib) // 80%: warning
	report(20 * gib) // still warning: no new alert
	report(4 * gib)  // 96%: critical
	if got := f.outbox.topics(); !slices.Equal(got, []string{TopicStorageLevel, TopicStorageLevel}) {
		t.Fatalf("alerts = %v", got)
	}
	if ok, _ := f.tel.DiskHeavyAllowed(ctx, r.ID); ok {
		t.Fatal("critical runner allowed disk-heavy steps")
	}
	report(50 * gib)
	if ok, _ := f.tel.DiskHeavyAllowed(ctx, r.ID); !ok {
		t.Fatal("cleaned-up runner still blocked")
	}
}

func TestStorageReportsAreThrottledAndBounded(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	r := f.approved(t, keyA)
	v := volume(t, domain.RoleWorkspaces, 100*gib, 50*gib, f.clock.Now())
	if err := f.tel.Storage(t.Context(), r.ID, []domain.Volume{v, v}); err == nil {
		t.Fatal("duplicate volume roles accepted")
	}
	if err := f.tel.Storage(t.Context(), r.ID, []domain.Volume{v}); err != nil {
		t.Fatal(err)
	}
	v.AvailableBytes = gib
	if err := f.tel.Storage(t.Context(), r.ID, []domain.Volume{v}); err != nil {
		t.Fatal(err)
	}
	if got := f.trepo.vols[r.ID][0].AvailableBytes; got != 50*gib {
		t.Fatalf("a report right after the previous one was stored (available %d)", got)
	}
}

func TestStorageOverviewOwnerOnlyWithControlPlaneStaleAndForecast(t *testing.T) {
	t.Parallel()
	f := newTelFixture(t)
	ctx := t.Context()
	online := f.approved(t, keyA)
	offline := f.approved(t, keyB)
	f.enrolled(t, keyC) // pending: not part of the overview

	if _, err := f.tel.Overview(ctx, member); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("member overview = %v", err)
	}
	// Four days of history for the online runner: +1 GiB a day.
	for d := 3; d >= 0; d-- {
		at := f.clock.Now().AddDate(0, 0, -d)
		_, _ = f.trepo.ReplaceVolumes(ctx, online.ID, []domain.Volume{volume(t, domain.RoleWorkspaces, 100*gib, (50-int64(3-d))*gib, at)})
	}
	_ = f.tel.Heartbeat(ctx, offline.ID)
	_, _ = f.trepo.ReplaceVolumes(ctx, offline.ID, []domain.Volume{volume(t, domain.RoleWorkspaces, 100*gib, 10*gib, f.clock.Now())})
	if err := f.tel.Storage(ctx, domain.ControlPlaneMachine, []domain.Volume{volume(t, domain.RoleData, 20*gib, 15*gib, f.clock.Now())}); err != nil {
		t.Fatal(err)
	}
	f.clock.add(time.Minute)
	f.trepo.mu.Lock()
	f.trepo.hb[offline.ID] = f.clock.Now().Add(-time.Hour)
	f.trepo.mu.Unlock()
	_ = f.tel.Heartbeat(ctx, online.ID)

	ov, err := f.tel.Overview(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Machines) != 3 || ov.Machines[0].Kind != MachineControlPlane {
		t.Fatalf("machines = %+v", ov.Machines)
	}
	byID := map[string]MachineOverview{}
	for _, m := range ov.Machines {
		byID[m.ID] = m
	}
	cp := byID[domain.ControlPlaneMachine]
	if len(cp.Volumes) != 1 || cp.Volumes[0].Stale || cp.Volumes[0].Level != domain.LevelOK {
		t.Fatalf("control plane = %+v", cp)
	}
	on := byID[online.ID]
	if on.Name != "build-box" || on.Availability != domain.AvailabilityOnline || on.Volumes[0].Stale {
		t.Fatalf("online runner = %+v", on)
	}
	if fc := on.Volumes[0].Forecast7; fc.State != domain.ForecastOK || fc.GrowthBytesPerDay != gib {
		t.Fatalf("forecast = %+v", fc)
	}
	off := byID[offline.ID]
	if off.Availability != domain.AvailabilityUnreachable || !off.Volumes[0].Stale || off.Volumes[0].Level != domain.LevelHigh {
		t.Fatalf("offline runner = %+v", off)
	}
	if off.Volumes[0].Forecast7.State != domain.ForecastInsufficientData {
		t.Fatalf("offline forecast = %+v", off.Volumes[0].Forecast7)
	}
}

const keyC = "KCkqKywtLi8wMTIzNDU2Nzg5Ojs8PT4/QEFCQ0RFRkc="
