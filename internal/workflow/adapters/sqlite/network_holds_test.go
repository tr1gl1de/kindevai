package sqlite_test

import (
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// KIN-81: a step's network holds in a row (its retry backoff) survive a save and load.
func TestNetworkHoldsAreStored(t *testing.T) {
	t.Parallel()
	d, _ := testDB(t)
	ctx := t.Context()
	svc := service(d, func() time.Time { return t0 })
	repo := sqlite.New(d)
	_, e := running(t, svc)
	back := e.StepByKey("backend").ID
	if _, err := svc.Claim(ctx, back, "r1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.HoldStepForNetwork(ctx, back, "r1", 1, "strict egress sandbox not verified", t0); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Execution(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st := got.Step(back); !st.WaitingForNetwork() || st.NetworkHolds != 1 {
		t.Fatalf("step %+v", st)
	}
	if got.Step(back).NetworkRetryDue(t0.Add(domain.NetworkRetryDelay - time.Second)) {
		t.Fatal("retry due before the delay")
	}
}
