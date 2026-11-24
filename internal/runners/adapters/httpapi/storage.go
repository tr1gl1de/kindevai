package httpapi

import (
	"context"
	"net/http"
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
		mv := MachineView{ID: m.ID, Kind: m.Kind, Name: m.Name, Status: string(m.Status), Availability: string(m.Availability),
			LastHeartbeatAt: m.LastHeartbeatAt, DiskHeavyAllowed: m.DiskHeavyAllowed, Volumes: make([]VolumeView, 0, len(m.Volumes))}
		for _, v := range m.Volumes {
			mv.Volumes = append(mv.Volumes, VolumeView{Role: v.Role, MountOrVolumeID: v.ID, Path: v.Path, TotalBytes: v.TotalBytes,
				AvailableBytes: v.AvailableBytes, FreeBytes: v.FreeBytes, UsedPercent: v.UsedPercent, Level: string(v.Level),
				DiskHeavyAllowed: v.DiskHeavyAllowed(), Stale: v.Stale, LastReportedAt: v.ReportedAt,
				Forecast7d: forecastView(v.Forecast7), Forecast30d: forecastView(v.Forecast30)})
		}
		out.Machines = append(out.Machines, mv)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
