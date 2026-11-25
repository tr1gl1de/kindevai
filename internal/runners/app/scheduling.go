package app

import (
	"context"
	"slices"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Candidate is a runner the workflow scheduler may assign a project's steps to now (KIN-23).
type Candidate struct {
	ID               string
	Tools            []string // names of the tools it reported (capabilities)
	DiskHeavyAllowed bool     // false while a volume of the runner is critical (KIN-20)
	StrictEgress     bool     // its strict egress sandbox verified (KIN-31)
}

// Candidates lists, by id, the runners that may receive the project's steps now: APPROVED and granted the project
// (a PENDING, disabled or revoked runner never is), with a live session (online) and online by their heartbeats
// (not UNREACHABLE, not never connected).
func (t *Telemetry) Candidates(ctx context.Context, projectID string, online func(runnerID string) bool) ([]Candidate, error) {
	rs, err := t.Runners.Runners(ctx)
	if err != nil {
		return nil, err
	}
	pres, err := t.Presence(ctx)
	if err != nil {
		return nil, err
	}
	vols, err := t.Repo.Volumes(ctx)
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, r := range rs {
		p, ok := pres[r.ID]
		if !r.MayRun(projectID) || !ok || p.Availability != domain.AvailabilityOnline || online == nil || !online(r.ID) {
			continue
		}
		c := Candidate{ID: r.ID, Tools: []string{}, DiskHeavyAllowed: !slices.ContainsFunc(vols[r.ID],
			func(v domain.Volume) bool { return !v.DiskHeavyAllowed() })}
		if p.Capabilities != nil {
			c.StrictEgress = p.Capabilities.StrictEgress
			for _, tool := range p.Capabilities.Tools {
				c.Tools = append(c.Tools, tool.Name)
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Candidate) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
