package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Runner telemetry in the API (KIN-20): availability and capabilities on RunnerView, and the Storage Overview.

// ToolView is a CLI the runner found.
type ToolView struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CapabilitiesView is a runner's last capability report (self-reported, informational).
type CapabilitiesView struct {
	OS           string     `json:"os"`
	Arch         string     `json:"arch"`
	WSL          bool       `json:"wsl"`
	StrictEgress bool       `json:"strictEgress"`
	Tools        []ToolView `json:"tools"`
	ReportedAt   time.Time  `json:"reportedAt"`
}

func capabilitiesView(c *domain.Capabilities) *CapabilitiesView {
	if c == nil {
		return nil
	}
	v := &CapabilitiesView{OS: c.OS, Arch: c.Arch, WSL: c.WSL, StrictEgress: c.StrictEgress, ReportedAt: c.ReportedAt,
		Tools: make([]ToolView, 0, len(c.Tools))}
	for _, t := range c.Tools {
		v.Tools = append(v.Tools, ToolView(t))
	}
	return v
}

// presence loads the runners' liveness; without a telemetry service every runner is "never connected".
func (h *Handler) presence(ctx context.Context) map[string]app.Presence {
	if h.Telemetry == nil {
		return nil
	}
	p, err := h.Telemetry.Presence(ctx)
	if err != nil { // the runner list stays usable without liveness
		h.logger().Error("loading runner presence", "err", err)
		return nil
	}
	return p
}

func withPresence(v RunnerView, p map[string]app.Presence) RunnerView {
	x, ok := p[v.ID]
	v.Availability = string(domain.AvailabilityNever)
	if ok {
		v.Availability, v.LastHeartbeatAt, v.Capabilities = string(x.Availability), x.LastHeartbeatAt, capabilitiesView(x.Capabilities)
	}
	return v
}

// ForecastView is a fill-up forecast: state insufficient_data, not_needed or ok (then days to 85% and 90%).
type ForecastView struct {
	State             string `json:"state"`
	WindowDays        int    `json:"windowDays"`
	GrowthBytesPerDay int64  `json:"growthBytesPerDay"`
	DaysTo85          *int   `json:"daysTo85"`
	DaysTo90          *int   `json:"daysTo90"`
}

// VolumeView carries the KIN-A-26 metrics of one volume.
type VolumeView struct {
	Role             string       `json:"role"`
	MountOrVolumeID  string       `json:"mountOrVolumeId"`
	Path             string       `json:"path"`
	TotalBytes       int64        `json:"totalBytes"`
	AvailableBytes   int64        `json:"availableBytes"`
	FreeBytes        int64        `json:"freeBytes"`
	UsedPercent      float64      `json:"usedPercent"`
	Level            string       `json:"level"`
	DiskHeavyAllowed bool         `json:"diskHeavyAllowed"`
	Stale            bool         `json:"stale"`
	LastReportedAt   time.Time    `json:"lastReportedAt"`
	Forecast7d       ForecastView `json:"forecast7d"`
	Forecast30d      ForecastView `json:"forecast30d"`
}

// MachineView is one row of the Storage Overview.
type MachineView struct {
	ID               string       `json:"id"`
	Kind             string       `json:"kind"` // control_plane or runner
	Name             string       `json:"name"`
	Status           string       `json:"status,omitempty"`
	Availability     string       `json:"availability"`
	LastHeartbeatAt  *time.Time   `json:"lastHeartbeatAt"`
	DiskHeavyAllowed bool         `json:"diskHeavyAllowed"`
	Volumes          []VolumeView `json:"volumes"`
}

// ThresholdsView tells the UI where the levels start.
type ThresholdsView struct {
	WarningPercent   int   `json:"warningPercent"`
	HighPercent      int   `json:"highPercent"`
	CriticalPercent  int   `json:"criticalPercent"`
	MinHeadroomBytes int64 `json:"minHeadroomBytes"`
}

// StorageOverview is GET /api/storage/overview.
type StorageOverview struct {
	GeneratedAt time.Time      `json:"generatedAt"`
	Thresholds  ThresholdsView `json:"thresholds"`
	Machines    []MachineView  `json:"machines"`
}

func forecastView(f domain.Forecast) ForecastView {
	return ForecastView{State: string(f.State), WindowDays: f.WindowDays, GrowthBytesPerDay: f.GrowthBytesPerDay,
		DaysTo85: f.DaysTo85, DaysTo90: f.DaysTo90}
}

func (h *Handler) storageOverview(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	ov, err := h.Telemetry.Overview(r.Context(), p)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := StorageOverview{GeneratedAt: ov.GeneratedAt, Machines: make([]MachineView, 0, len(ov.Machines)),
		Thresholds: ThresholdsView{WarningPercent: domain.WarningPercent, HighPercent: domain.HighPercent,
			CriticalPercent: domain.CriticalPercent, MinHeadroomBytes: domain.MinHeadroomBytes}}
	for _, m := range ov.Machines {
		out.Machines = append(out.Machines, machineView(m))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func machineView(m app.MachineOverview) MachineView {
	mv := MachineView{ID: m.ID, Kind: m.Kind, Name: m.Name, Status: string(m.Status), Availability: string(m.Availability),
		LastHeartbeatAt: m.LastHeartbeatAt, DiskHeavyAllowed: m.DiskHeavyAllowed, Volumes: make([]VolumeView, 0, len(m.Volumes))}
	for _, v := range m.Volumes {
		mv.Volumes = append(mv.Volumes, VolumeView{Role: v.Role, MountOrVolumeID: v.ID, Path: v.Path, TotalBytes: v.TotalBytes,
			AvailableBytes: v.AvailableBytes, FreeBytes: v.FreeBytes, UsedPercent: v.UsedPercent, Level: string(v.Level),
			DiskHeavyAllowed: v.DiskHeavyAllowed(), Stale: v.Stale, LastReportedAt: v.ReportedAt,
			Forecast7d: forecastView(v.Forecast7), Forecast30d: forecastView(v.Forecast30)})
	}
	return mv
}

// SampleView is one hourly point of the storage history.
type SampleView struct {
	At         time.Time `json:"at"`
	TotalBytes int64     `json:"totalBytes"`
	UsedBytes  int64     `json:"usedBytes"`
}

// ConsumerView is the size of one space-consumer category (KIN-55).
type ConsumerView struct {
	Category string `json:"category"`
	Bytes    int64  `json:"bytes"`
	Entries  int64  `json:"entries"`
	Partial  bool   `json:"partial"`
}

// ConsumersView is a machine's latest space-consumer measurement.
type ConsumersView struct {
	MeasuredAt time.Time      `json:"measuredAt"`
	Items      []ConsumerView `json:"items"`
}

// StorageMachineDetail is GET /api/storage/machines/{id}?days=7|30 (Infrastructure → Storage, KIN-55).
type StorageMachineDetail struct {
	MachineView
	Days      int                     `json:"days"`
	History   map[string][]SampleView `json:"history"` // volume role → hourly samples, oldest first
	Consumers *ConsumersView          `json:"consumers"`
}

func (h *Handler) storageMachine(w http.ResponseWriter, r *http.Request) {
	p, ok := principal(w, r)
	if !ok {
		return
	}
	days := 7
	if q := r.URL.Query().Get("days"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil {
			h.fail(w, &domain.ValidationError{Field: "days", Code: "unsupported"})
			return
		}
		days = n
	}
	m, err := h.Telemetry.Machine(r.Context(), p, r.PathValue("id"), days)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := StorageMachineDetail{MachineView: machineView(m.MachineOverview), Days: m.Days,
		History: make(map[string][]SampleView, len(m.History))}
	for role, ss := range m.History {
		vs := make([]SampleView, 0, len(ss))
		for _, s := range ss {
			vs = append(vs, SampleView{At: s.Hour, TotalBytes: s.TotalBytes, UsedBytes: s.UsedBytes})
		}
		out.History[role] = vs
	}
	if c := m.Consumers; c != nil {
		out.Consumers = &ConsumersView{MeasuredAt: c.MeasuredAt, Items: make([]ConsumerView, 0, len(c.Items))}
		for _, x := range c.Items {
			out.Consumers.Items = append(out.Consumers.Items, ConsumerView(x))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
