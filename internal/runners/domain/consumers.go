package domain

import (
	"cmp"
	"slices"
	"time"
)

// Space consumers (KIN-55, KB KIN-A-26 "Источники занимаемого места"): what takes the disk space of a runner,
// measured by the runner only when its operator allowed it, with I/O limits. Informational: it never triggers any
// cleanup.

// Consumer categories, in display order.
const (
	ConsumerRepos          = "repos"           // git mirrors in runner-data/repos
	ConsumerWorkspaces     = "workspaces"      // workspaces and git worktrees in runner-data/workspaces
	ConsumerSessions       = "sessions"        // CLI session storage in runner-data/sessions
	ConsumerSpool          = "spool"           // the event spool in runner-data/events
	ConsumerContainerCache = "container_cache" // container images, build cache and volumes (docker system df)
)

var consumerOrder = []string{ConsumerRepos, ConsumerWorkspaces, ConsumerSessions, ConsumerSpool, ConsumerContainerCache}

// MaxConsumers bounds one report.
const MaxConsumers = 8

// SpaceConsumer is the size of one category. Partial means the measurement stopped at its entry or time limit,
// so Bytes is a lower bound.
type SpaceConsumer struct {
	Category string
	Bytes    int64
	Entries  int64
	Partial  bool
}

// SpaceConsumers is one measurement of a machine, stamped with the Control Plane's clock when it arrived.
type SpaceConsumers struct {
	MeasuredAt time.Time
	Items      []SpaceConsumer
}

// NewSpaceConsumers validates a measurement and orders it by category.
func NewSpaceConsumers(items []SpaceConsumer, at time.Time) (SpaceConsumers, error) {
	if len(items) > MaxConsumers {
		return SpaceConsumers{}, &ValidationError{Field: "consumers", Code: "too_many"}
	}
	seen := map[string]bool{}
	for _, c := range items {
		if !slices.Contains(consumerOrder, c.Category) {
			return SpaceConsumers{}, &ValidationError{Field: "consumers", Code: "unknown_category"}
		}
		if c.Bytes < 0 || c.Entries < 0 {
			return SpaceConsumers{}, &ValidationError{Field: "consumers", Code: "negative"}
		}
		if seen[c.Category] {
			return SpaceConsumers{}, &ValidationError{Field: "consumers", Code: "duplicate_category"}
		}
		seen[c.Category] = true
	}
	out := slices.Clone(items)
	if out == nil {
		out = []SpaceConsumer{}
	}
	slices.SortFunc(out, func(a, b SpaceConsumer) int {
		return cmp.Compare(slices.Index(consumerOrder, a.Category), slices.Index(consumerOrder, b.Category))
	})
	return SpaceConsumers{MeasuredAt: at, Items: out}, nil
}
