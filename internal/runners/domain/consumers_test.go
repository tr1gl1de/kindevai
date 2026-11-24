package domain_test

import (
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

func TestSpaceConsumersAreValidatedAndOrderedByCategory(t *testing.T) {
	t.Parallel()
	got, err := domain.NewSpaceConsumers([]domain.SpaceConsumer{
		{Category: domain.ConsumerSpool, Bytes: 10, Entries: 2},
		{Category: domain.ConsumerContainerCache, Bytes: 5 << 30},
		{Category: domain.ConsumerWorkspaces, Bytes: 7 << 30, Entries: 90_000, Partial: true},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got.MeasuredAt != t0 || len(got.Items) != 3 {
		t.Fatalf("got %+v", got)
	}
	want := []string{domain.ConsumerWorkspaces, domain.ConsumerSpool, domain.ConsumerContainerCache}
	for i, c := range got.Items {
		if c.Category != want[i] {
			t.Fatalf("order: got %v at %d, want %v", c.Category, i, want[i])
		}
	}
}

func TestSpaceConsumersRejectInvalidReports(t *testing.T) {
	t.Parallel()
	many := make([]domain.SpaceConsumer, domain.MaxConsumers+1)
	for i := range many {
		many[i] = domain.SpaceConsumer{Category: domain.ConsumerRepos}
	}
	cases := []struct {
		name string
		in   []domain.SpaceConsumer
		code string
	}{
		{"unknown category", []domain.SpaceConsumer{{Category: "home"}}, "unknown_category"},
		{"negative bytes", []domain.SpaceConsumer{{Category: domain.ConsumerRepos, Bytes: -1}}, "negative"},
		{"negative entries", []domain.SpaceConsumer{{Category: domain.ConsumerRepos, Entries: -1}}, "negative"},
		{"duplicate", []domain.SpaceConsumer{{Category: domain.ConsumerRepos}, {Category: domain.ConsumerRepos}}, "duplicate_category"},
		{"too many", many, "too_many"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := domain.NewSpaceConsumers(c.in, t0)
			var ve *domain.ValidationError
			if !errors.As(err, &ve) || ve.Code != c.code {
				t.Fatalf("got %v, want code %s", err, c.code)
			}
		})
	}
}
