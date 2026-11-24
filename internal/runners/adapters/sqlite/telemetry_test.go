package sqlite_test

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

func runnerWith(t *testing.T, r *sqlite.Repository, id string, approve bool) {
	t.Helper()
	ctx := t.Context()
	tok, _, err := domain.NewEnrollmentToken(rand.Reader, "tok-"+id, "owner1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	k, _ := newKey(t)
	m, _ := domain.NewMetadata("host-"+id, "linux", "amd64", "")
	x := domain.Enroll(id, tok, k, m, t0)
	if err := r.CreateRunner(ctx, x); err != nil {
		t.Fatal(err)
	}
	if approve {
		if err := x.Approve([]string{"p1"}, "owner1", t0); err != nil {
			t.Fatal(err)
		}
		if err := r.SaveRunner(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHeartbeatUnreachableAndCapabilitiesRoundTrip(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	runnerWith(t, r, "r1", true)
	runnerWith(t, r, "r2", false) // pending: never marked unreachable

	for _, id := range []string{"r1", "r2"} {
		if since, err := r.RecordHeartbeat(ctx, id, t0); err != nil || since != nil {
			t.Fatalf("first heartbeat of %s = %v, %v", id, since, err)
		}
	}
	x, _ := r.Runner(ctx, "r1")
	if x.LastSeenAt == nil || !x.LastSeenAt.Equal(t0) {
		t.Fatalf("last seen = %v", x.LastSeenAt)
	}
	if ids, err := r.MarkUnreachable(ctx, t0.Add(-time.Second), t0.Add(time.Minute)); err != nil || len(ids) != 0 {
		t.Fatalf("fresh heartbeat marked: %v, %v", ids, err)
	}
	down := t0.Add(time.Minute)
	ids, err := r.MarkUnreachable(ctx, t0, down)
	if err != nil || len(ids) != 1 || ids[0] != "r1" {
		t.Fatalf("MarkUnreachable = %v, %v", ids, err)
	}
	if ids, _ := r.MarkUnreachable(ctx, t0, down); len(ids) != 0 {
		t.Fatalf("marked twice: %v", ids)
	}
	tel, err := r.Telemetry(ctx)
	if err != nil || tel["r1"].UnreachableSince == nil || !tel["r1"].UnreachableSince.Equal(down) {
		t.Fatalf("telemetry = %+v, %v", tel["r1"], err)
	}
	since, err := r.RecordHeartbeat(ctx, "r1", down.Add(time.Second))
	if err != nil || since == nil || !since.Equal(down) {
		t.Fatalf("recovering heartbeat = %v, %v", since, err)
	}
	tel, _ = r.Telemetry(ctx)
	if tel["r1"].UnreachableSince != nil {
		t.Fatal("unreachable mark not cleared")
	}

	c, _ := domain.NewCapabilities("linux", "arm64", true, false, []domain.Tool{{Name: "codex", Version: "0.5"}, {Name: "git", Version: "2.43"}}, t0)
	if err := r.SaveCapabilities(ctx, "r1", c); err != nil {
		t.Fatal(err)
	}
	tel, _ = r.Telemetry(ctx)
	got := tel["r1"].Capabilities
	if got == nil || got.Arch != "arm64" || !got.WSL || len(got.Tools) != 2 || got.Tools[1].Version != "2.43" || !got.ReportedAt.Equal(t0) {
		t.Fatalf("capabilities = %+v", got)
	}
	if tel["r1"].LastHeartbeatAt == nil {
		t.Fatal("capabilities overwrote the heartbeat")
	}
	if err := r.SaveCapabilities(ctx, "missing", c); err == nil {
		t.Fatal("capabilities of an unknown runner stored")
	}
}

func mustVolume(t *testing.T, role string, total, avail int64, at time.Time) domain.Volume {
	t.Helper()
	v, err := domain.NewVolume("/", role, "/srv", total, avail, avail, at)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVolumesReplaceAndHourlyHistoryWithPurge(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	m := domain.ControlPlaneMachine

	prev, err := r.ReplaceVolumes(ctx, m, []domain.Volume{mustVolume(t, domain.RoleData, 100, 60, t0), mustVolume(t, domain.RoleWindowsHost, 50, 10, t0)})
	if err != nil || len(prev) != 0 {
		t.Fatalf("first report: %v, %v", prev, err)
	}
	// Same hour: the history keeps one row with the last values; a volume that disappeared is dropped.
	prev, err = r.ReplaceVolumes(ctx, m, []domain.Volume{mustVolume(t, domain.RoleData, 100, 50, t0.Add(20*time.Minute))})
	if err != nil || len(prev) != 2 {
		t.Fatalf("second report prev = %v, %v", prev, err)
	}
	vols, _ := r.Volumes(ctx)
	if len(vols[m]) != 1 || vols[m][0].AvailableBytes != 50 || !vols[m][0].ReportedAt.Equal(t0.Add(20*time.Minute)) {
		t.Fatalf("volumes = %+v", vols[m])
	}
	_, _ = r.ReplaceVolumes(ctx, m, []domain.Volume{mustVolume(t, domain.RoleData, 100, 40, t0.Add(25*time.Hour))})
	s, err := r.Samples(ctx, t0.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	data := s[m][domain.RoleData]
	if len(data) != 2 || data[0].UsedBytes != 50 || !data[0].Hour.Equal(t0) || data[1].UsedBytes != 60 {
		t.Fatalf("history = %+v", data)
	}
	if len(s[m][domain.RoleWindowsHost]) != 1 {
		t.Fatalf("windows host history = %+v", s[m][domain.RoleWindowsHost])
	}

	n, err := r.PurgeSamples(ctx, t0.Add(time.Hour), 100) // TTL drops the two rows of t0
	if err != nil || n != 2 {
		t.Fatalf("TTL purge = %d, %v", n, err)
	}
	for h := range 5 {
		_, _ = r.ReplaceVolumes(ctx, "r9", []domain.Volume{mustVolume(t, domain.RoleWorkspaces, 100, 50, t0.Add(time.Duration(30+h)*time.Hour))})
	}
	n, err = r.PurgeSamples(ctx, t0, 3) // row limit keeps the newest 3 of 6
	if err != nil || n != 3 {
		t.Fatalf("row-limit purge = %d, %v", n, err)
	}
	var left int
	var oldest string
	if err := d.QueryRowContext(ctx, "SELECT count(*), min(hour) FROM storage_history").Scan(&left, &oldest); err != nil {
		t.Fatal(err)
	}
	if left != 3 || oldest != t0.Add(32*time.Hour).Format(time.RFC3339) {
		t.Fatalf("left %d rows, oldest %s", left, oldest)
	}
}

var _ app.TelemetryRepo = (*sqlite.Repository)(nil)

func TestSpaceConsumersReplaceAndMachineHistory(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	if got, err := r.SpaceConsumers(ctx, "r1"); err != nil || got != nil {
		t.Fatalf("never measured = %v, %v", got, err)
	}
	first, _ := domain.NewSpaceConsumers([]domain.SpaceConsumer{{Category: domain.ConsumerRepos, Bytes: 1}}, t0)
	if err := r.ReplaceSpaceConsumers(ctx, "r1", first); err != nil {
		t.Fatal(err)
	}
	second, _ := domain.NewSpaceConsumers([]domain.SpaceConsumer{
		{Category: domain.ConsumerWorkspaces, Bytes: 7 << 30, Entries: 12, Partial: true},
		{Category: domain.ConsumerSpool, Bytes: 4096, Entries: 3},
	}, t0.Add(time.Hour))
	if err := r.ReplaceSpaceConsumers(ctx, "r1", second); err != nil {
		t.Fatal(err)
	}
	got, err := r.SpaceConsumers(ctx, "r1")
	if err != nil || got == nil {
		t.Fatalf("got %v, %v", got, err)
	}
	if !got.MeasuredAt.Equal(second.MeasuredAt) || len(got.Items) != 2 || got.Items[0] != second.Items[0] || got.Items[1] != second.Items[1] {
		t.Fatalf("got %+v, want %+v", *got, second)
	}

	// The machine's own history only, oldest first.
	for i, m := range []string{"r1", "r2", "r1"} {
		if _, err := r.ReplaceVolumes(ctx, m, []domain.Volume{mustVolume(t, domain.RoleWorkspaces, 100, 50, t0.Add(time.Duration(i)*time.Hour))}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := r.MachineSamples(ctx, "r1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if s := h[domain.RoleWorkspaces]; len(h) != 1 || len(s) != 2 || !s[0].Hour.Equal(t0) || !s[1].Hour.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("history = %+v", h)
	}
}
