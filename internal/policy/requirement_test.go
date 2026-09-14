package policy_test

import (
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

func noSafety(s config.Settings) config.Settings {
	s.SafetyFactor = 1.0
	return s
}

func TestAnEmptyQueueRequiresNothing(t *testing.T) {
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{})

	got := policy.Required(state, noSafety(noColdstart(baseSettings())))

	if got.Executors != 0 {
		t.Errorf("Executors = %d, want 0", got.Executors)
	}
	if !got.Feasible() {
		t.Error("Feasible = false for an empty queue")
	}
}

// The whole point of searching is to find the *smallest* count that works.
// Over-provisioning by one executor per cycle is how a cloud bill triples.
func TestTheAnswerIsMinimal(t *testing.T) {
	settings := noSafety(noColdstart(baseSettings()))
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 400, OldestJobAge: 30 * time.Second, ArrivalRate: 2},
		50:  {Depth: 120, OldestJobAge: 2 * time.Minute, ArrivalRate: 0.5},
	})

	got := policy.Required(state, settings)

	if !got.Feasible() {
		t.Fatalf("Feasible = false, want a satisfiable requirement: %+v", got)
	}
	if policy.Simulate(state, ramp(state, got.Minimum, settings), settings).BreachExpected {
		t.Errorf("Minimum = %d still breaches, so it is not a solution", got.Minimum)
	}
	if got.Minimum == 0 {
		t.Fatal("Minimum = 0 for a saturated queue")
	}
	if !policy.Simulate(state, ramp(state, got.Minimum-1, settings), settings).BreachExpected {
		t.Errorf("Minimum = %d, but %d also avoids a breach, so it is not minimal",
			got.Minimum, got.Minimum-1)
	}
}

func TestSafetyFactorBuysHeadroomAboveTheMinimum(t *testing.T) {
	settings := noColdstart(baseSettings())
	settings.SafetyFactor = 1.5
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 400, OldestJobAge: 30 * time.Second, ArrivalRate: 2},
	})

	got := policy.Required(state, settings)

	// The throughput figure the requirement is computed from is itself an
	// estimate; the factor is the margin on that estimate.
	want := int(float64(got.Minimum)*1.5 + 0.999)
	if got.Executors != want {
		t.Errorf("Executors = %d, want ceil(%d * 1.5) = %d", got.Executors, got.Minimum, want)
	}
}

func TestSafetyFactorRoundsUpNeverDown(t *testing.T) {
	settings := noColdstart(baseSettings())
	settings.SafetyFactor = 1.01
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 400, OldestJobAge: 30 * time.Second, ArrivalRate: 2},
	})

	got := policy.Required(state, settings)

	if got.Executors <= got.Minimum {
		t.Errorf("Executors = %d with a 1.01 factor, want strictly more than the minimum %d",
			got.Executors, got.Minimum)
	}
}

func TestAnImpossibleWorkloadReportsTheCeilingAndSaysSo(t *testing.T) {
	settings := noSafety(noColdstart(baseSettings()))
	settings.LocalExecutorCap = 4
	settings.CloudExecutorCap = 6
	settings.MinLocalExecutors = 0

	// Arrivals far beyond what ten executors can ever serve.
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 5000, OldestJobAge: 50 * time.Second, ArrivalRate: 500},
	})

	got := policy.Required(state, settings)

	if got.Feasible() {
		t.Error("Feasible = true for a workload no permitted count can serve")
	}
	// Reporting the ceiling rather than giving up is what keeps the service
	// running flat out during an overload instead of idling through it.
	if got.Executors != 10 {
		t.Errorf("Executors = %d, want the combined ceiling of 10", got.Executors)
	}
	if !got.Projection.BreachExpected {
		t.Error("Projection.BreachExpected = false, want the unavoidable breach reported")
	}
}

func TestTheRequirementNeverExceedsTheCombinedCaps(t *testing.T) {
	settings := noSafety(noColdstart(baseSettings()))
	settings.LocalExecutorCap = 3
	settings.CloudExecutorCap = 2
	settings.MinLocalExecutors = 0
	settings.SafetyFactor = 4.0

	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 900, OldestJobAge: 55 * time.Second, ArrivalRate: 40},
	})

	got := policy.Required(state, settings)

	if got.Executors > 5 {
		t.Errorf("Executors = %d, want no more than the combined ceiling of 5", got.Executors)
	}
}

func TestTheProjectionDescribesTheCountActuallyChosen(t *testing.T) {
	settings := noColdstart(baseSettings())
	settings.SafetyFactor = 2.0
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 300, OldestJobAge: 20 * time.Second, ArrivalRate: 1},
	})

	got := policy.Required(state, settings)

	// Not the projection under Minimum: the operator is shown what the plan
	// they are actually getting is expected to do.
	if want := policy.Simulate(state, ramp(state, got.Executors, settings), settings); got.Projection != want {
		t.Errorf("Projection = %+v, want the projection under %d executors, %+v",
			got.Projection, got.Executors, want)
	}
}

func TestRequiredIsDeterministic(t *testing.T) {
	settings := noColdstart(baseSettings())
	state := stateWith(0.4, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 77, OldestJobAge: 14 * time.Second, ArrivalRate: 1.3},
		25:  {Depth: 950, OldestJobAge: 3 * time.Hour, ArrivalRate: 0.2},
	})

	first := policy.Required(state, settings)
	for i := 0; i < 20; i++ {
		if got := policy.Required(state, settings); got != first {
			t.Fatalf("Required() run %d = %+v, want the identical %+v", i, got, first)
		}
	}
}
