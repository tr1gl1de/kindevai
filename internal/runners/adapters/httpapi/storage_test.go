package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

func (e *env) approvedRunner(t *testing.T) string {
	t.Helper()
	w := e.do(t, "", "POST", "/api/runners/enroll", enrollBody(e.token(t), newKey(t)))
	id := decode[EnrollResult](t, w).RunnerID
	if w := e.do(t, "owner", "POST", "/api/runners/"+id+"/approve", ProjectsRequest{ProjectIDs: []string{"p1"}}); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	return id
}

func TestRunnerViewShowsAvailabilityLastHeartbeatAndCapabilities(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	id := e.approvedRunner(t)
	if v := decode[RunnerView](t, e.do(t, "owner", "GET", "/api/runners/"+id, nil)); v.Availability != "never_connected" ||
		v.LastHeartbeatAt != nil || v.Capabilities != nil {
		t.Fatalf("before telemetry = %+v", v)
	}
	now := *e.now.Load()
	if err := e.tel.Heartbeat(t.Context(), id, "s1"); err != nil {
		t.Fatal(err)
	}
	c, _ := domain.NewCapabilities("linux", "amd64", true, false, []domain.Tool{{Name: "claude", Version: "1.0.3"}}, now)
	if err := e.tel.Capabilities(t.Context(), id, "s1", c); err != nil {
		t.Fatal(err)
	}
	v := decode[RunnerView](t, e.do(t, "owner", "GET", "/api/runners/"+id, nil))
	if v.Availability != "online" || v.LastHeartbeatAt == nil || !v.LastHeartbeatAt.Equal(now) || v.LastSeenAt == nil {
		t.Fatalf("after heartbeat = %+v", v)
	}
	if v.Capabilities == nil || !v.Capabilities.WSL || len(v.Capabilities.Tools) != 1 || v.Capabilities.Tools[0].Version != "1.0.3" {
		t.Fatalf("capabilities = %+v", v.Capabilities)
	}
	later := now.Add(time.Minute)
	e.now.Store(&later)
	list := decode[[]RunnerView](t, e.do(t, "owner", "GET", "/api/runners", nil))
	if len(list) != 1 || list[0].Availability != "unreachable" {
		t.Fatalf("list after 60s without heartbeat = %+v", list)
	}
}

func TestStorageOverviewOwnerOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	id := e.approvedRunner(t)
	now := *e.now.Load()
	_ = e.tel.Heartbeat(t.Context(), id, "s1")
	v, _ := domain.NewVolume("/", domain.RoleWorkspaces, "/srv/runner", 100<<30, 4<<30, 5<<30, now)
	if err := e.tel.Storage(t.Context(), id, "s1", []domain.Volume{v}); err != nil {
		t.Fatal(err)
	}
	if w := e.do(t, "bob", "GET", "/api/storage/overview", nil); w.Code != http.StatusForbidden {
		t.Fatalf("member = %d", w.Code)
	}
	if w := e.do(t, "", "GET", "/api/storage/overview", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", w.Code)
	}
	w := e.do(t, "owner", "GET", "/api/storage/overview", nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("overview = %d %s", w.Code, w.Body)
	}
	ov := decode[StorageOverview](t, w)
	if len(ov.Machines) != 2 || ov.Machines[0].Kind != "control_plane" || ov.Machines[0].Volumes == nil {
		t.Fatalf("overview = %+v", ov)
	}
	r := ov.Machines[1]
	if r.ID != id || r.Name != "build-box" || r.Availability != "online" || r.DiskHeavyAllowed || len(r.Volumes) != 1 {
		t.Fatalf("runner row = %+v", r)
	}
	vol := r.Volumes[0]
	if vol.Level != "critical" || vol.UsedPercent != 96 || vol.Stale || vol.MountOrVolumeID != "/" || !vol.LastReportedAt.Equal(now) ||
		vol.Forecast7d.State != "insufficient_data" || vol.Forecast30d.WindowDays != 30 {
		t.Fatalf("volume = %+v", vol)
	}
	if ov.Thresholds.WarningPercent != 75 || ov.Thresholds.MinHeadroomBytes != 2<<30 {
		t.Fatalf("thresholds = %+v", ov.Thresholds)
	}
}

func TestStorageMachineDetailOwnerOnlyWithHistoryAndConsumers(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	id := e.approvedRunner(t)
	now := *e.now.Load()
	_ = e.tel.Heartbeat(t.Context(), id, "s1")
	v, _ := domain.NewVolume("/", domain.RoleWorkspaces, "/srv/runner", 100<<30, 40<<30, 41<<30, now)
	if err := e.tel.Storage(t.Context(), id, "s1", []domain.Volume{v}); err != nil {
		t.Fatal(err)
	}
	if err := e.tel.SpaceConsumers(t.Context(), id, "s1", []domain.SpaceConsumer{
		{Category: domain.ConsumerWorkspaces, Bytes: 3 << 30, Entries: 1200, Partial: true}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/storage/machines/" + id
	if w := e.do(t, "bob", "GET", path, nil); w.Code != http.StatusForbidden {
		t.Fatalf("member = %d", w.Code)
	}
	if w := e.do(t, "owner", "GET", "/api/storage/machines/nope", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown = %d", w.Code)
	}
	for _, q := range []string{"?days=14", "?days=x"} {
		if w := e.do(t, "owner", "GET", path+q, nil); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s = %d", q, w.Code)
		}
	}
	w := e.do(t, "owner", "GET", path+"?days=30", nil)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("detail = %d %s", w.Code, w.Body)
	}
	d := decode[StorageMachineDetail](t, w)
	if d.ID != id || d.Kind != "runner" || d.Days != 30 || len(d.Volumes) != 1 || d.Volumes[0].Level != "ok" {
		t.Fatalf("detail = %+v", d)
	}
	h := d.History[domain.RoleWorkspaces]
	if len(h) != 1 || h[0].UsedBytes != 60<<30 || h[0].TotalBytes != 100<<30 || !h[0].At.Equal(now.Truncate(time.Hour)) {
		t.Fatalf("history = %+v", d.History)
	}
	if d.Consumers == nil || !d.Consumers.MeasuredAt.Equal(now) || len(d.Consumers.Items) != 1 ||
		d.Consumers.Items[0] != (ConsumerView{Category: "workspaces", Bytes: 3 << 30, Entries: 1200, Partial: true}) {
		t.Fatalf("consumers = %+v", d.Consumers)
	}
	// Default window is 7 days; the Control Plane has no consumers (null, not missing).
	cp := e.do(t, "owner", "GET", "/api/storage/machines/control-plane", nil)
	if cp.Code != http.StatusOK || decode[StorageMachineDetail](t, cp).Days != 7 || !strings.Contains(cp.Body.String(), `"consumers":null`) {
		t.Fatalf("control plane = %d %s", cp.Code, cp.Body)
	}
}

// KIN-59: the Storage Overview carries the Control Plane's stored runner output against its limit, the retention
// settings and the output recently dropped because the storage was full.
func TestStorageOverviewShowsRunnerOutputStorage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 100)
	id := e.approvedRunner(t)
	now := *e.now.Load()
	repo := sqlite.New(e.db)
	ov := decode[StorageOverview](t, e.do(t, "owner", "GET", "/api/storage/overview", nil))
	if o := ov.RunnerOutput; o == nil || o.UsedBytes != 0 || o.LimitBytes != 100<<20 || o.State != "ok" || o.OutputRetentionDays != 3 ||
		o.EventRetentionDays != 7 || o.DroppedEvents != 0 || o.LastDroppedAt != nil || o.Drops == nil || len(o.Drops) != 0 {
		t.Fatalf("empty output storage = %+v", o)
	}
	limits := domain.OutputLimits{Attempt: domain.MaxAttemptOutput, Total: 100 << 20}
	if v, err := repo.AddOutput(t.Context(), "s1", 1, 95<<20-domain.MaxAttemptOutput*5, limits, now); err != nil || v != domain.OutputStored {
		t.Fatal(v, err)
	}
	for i, at := range []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Minute)} {
		if err := repo.RecordEvent(t.Context(), domain.AttemptEvent{RunnerID: id, Step: "s2", Fence: 4, AttemptSeq: int64(i + 1),
			Kind: "execution.output", Outcome: domain.OutcomeApplied, Reason: "output_storage_limit", RunnerAt: at, ReceivedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	ov = decode[StorageOverview](t, e.do(t, "owner", "GET", "/api/storage/overview", nil))
	o := ov.RunnerOutput
	if o == nil || o.UsedBytes != 95<<20-domain.MaxAttemptOutput*5 || o.State != "full" || o.DroppedEvents != 2 ||
		o.LastDroppedAt == nil || !o.LastDroppedAt.Equal(now.Add(-time.Minute)) || o.DropsWindowHours != 24 {
		t.Fatalf("full output storage = %+v", o)
	}
	if len(o.Drops) != 1 || o.Drops[0] != (OutputDropView{RunnerID: id, StepID: "s2", Fence: 4, Events: 2, LastAt: now.Add(-time.Minute)}) {
		t.Fatalf("drops = %+v", o.Drops)
	}
	if w := e.do(t, "bob", "GET", "/api/storage/overview", nil); w.Code != http.StatusForbidden {
		t.Fatalf("member = %d", w.Code)
	}
}
