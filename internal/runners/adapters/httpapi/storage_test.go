package httpapi

import (
	"net/http"
	"testing"
	"time"

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
	if err := e.tel.Heartbeat(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c, _ := domain.NewCapabilities("linux", "amd64", true, false, []domain.Tool{{Name: "claude", Version: "1.0.3"}}, now)
	if err := e.tel.Capabilities(t.Context(), id, c); err != nil {
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
	_ = e.tel.Heartbeat(t.Context(), id)
	v, _ := domain.NewVolume("/", domain.RoleWorkspaces, "/srv/runner", 100<<30, 4<<30, 5<<30, now)
	if err := e.tel.Storage(t.Context(), id, []domain.Volume{v}); err != nil {
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
