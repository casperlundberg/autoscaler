package policy_test

import (
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

// baseSettings keeps the simulation cheap and the arithmetic easy to follow:
// a 10-minute horizon walked in 10-second steps.
func baseSettings() config.Settings {
	s := config.DefaultSettings()
	s.Horizon = 10 * time.Minute
	s.SimulationStep = 10 * time.Second
	s.Deadlines = map[domain.Priority]time.Duration{
		100: 60 * time.Second,
		50:  5 * time.Minute,
	}
	s.DefaultDeadline = time.Hour
	return s
}

func stateWith(throughput float64, queues map[domain.Priority]domain.QueueInfo) domain.SystemState {
	return domain.SystemState{
		Timestamp:          time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Queues:             queues,
		ExecutorThroughput: throughput,
	}
}

func TestAnEmptyQueueNeverBreaches(t *testing.T) {
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{})

	got := policy.Simulate(state, 1, baseSettings())

	if got.BreachExpected {
		t.Errorf("BreachExpected = true on an empty queue: %+v", got)
	}
	if got.PeakQueueDepth != 0 {
		t.Errorf("PeakQueueDepth = %d, want 0", got.PeakQueueDepth)
	}
}

func TestAmpleCapacityDrainsTheQueueWithoutBreaching(t *testing.T) {
	// 20 jobs, no arrivals, 10 executors at 1 job/s clears them in ~2s.
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 20, OldestJobAge: 5 * time.Second},
	})

	got := policy.Simulate(state, 10, baseSettings())

	if got.BreachExpected {
		t.Errorf("BreachExpected = true with ample capacity: %+v", got)
	}
	if got.DrainedAt == 0 {
		t.Error("DrainedAt = 0, want the queue to have been reported as cleared")
	}
	if got.DrainedAt > 30*time.Second {
		t.Errorf("DrainedAt = %v, want the queue cleared within a few steps", got.DrainedAt)
	}
}

func TestWithNoExecutorsTheOldestJobAgesPastItsDeadline(t *testing.T) {
	// P100's deadline is 60s and its oldest job has already waited 40s, so it
	// breaches 20s from now.
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 5, OldestJobAge: 40 * time.Second},
	})

	got := policy.Simulate(state, 0, baseSettings())

	if !got.BreachExpected {
		t.Fatalf("BreachExpected = false with zero executors: %+v", got)
	}
	if got.FirstBreachPriority != 100 {
		t.Errorf("FirstBreachPriority = %d, want 100", got.FirstBreachPriority)
	}
	if got.FirstBreachIn < 10*time.Second || got.FirstBreachIn > 30*time.Second {
		t.Errorf("FirstBreachIn = %v, want roughly 20s", got.FirstBreachIn)
	}
}

// Strict priority is the whole reason a low level can breach while a high one
// is comfortable: leftover capacity is all a low level ever gets.
func TestHighPriorityWorkStarvesLowPriorityWork(t *testing.T) {
	settings := baseSettings()
	// P100 arrives at exactly the rate one executor can serve, so it consumes
	// all capacity forever and P50 receives nothing.
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 10, OldestJobAge: time.Second, ArrivalRate: 1.0},
		50:  {Depth: 30, OldestJobAge: 4 * time.Minute},
	})

	got := policy.Simulate(state, 1, settings)

	if !got.BreachExpected {
		t.Fatalf("BreachExpected = false, want the starved level to breach: %+v", got)
	}
	if got.FirstBreachPriority != 50 {
		t.Errorf("FirstBreachPriority = %d, want 50 — the level receiving no capacity", got.FirstBreachPriority)
	}
}

func TestArrivalsOutpacingServiceGrowTheQueue(t *testing.T) {
	// 2 jobs/s arriving, 1 job/s served: the backlog grows for the whole run.
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 10, ArrivalRate: 2.0},
	})

	got := policy.Simulate(state, 1, baseSettings())

	if got.PeakQueueDepth <= 10 {
		t.Errorf("PeakQueueDepth = %d, want it to grow beyond the starting depth of 10", got.PeakQueueDepth)
	}
	if got.DrainedAt != 0 {
		t.Errorf("DrainedAt = %v, want 0 — the queue never clears", got.DrainedAt)
	}
	if !got.BreachExpected {
		t.Error("BreachExpected = false, want a breach from the unbounded backlog")
	}
}

func TestTheEarliestBreachIsTheOneReported(t *testing.T) {
	settings := baseSettings()
	settings.Deadlines = map[domain.Priority]time.Duration{
		100: 5 * time.Minute, // plenty of headroom left
		50:  time.Minute,     // breaches almost immediately
	}
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 20, OldestJobAge: 30 * time.Second, ArrivalRate: 1.0},
		50:  {Depth: 20, OldestJobAge: 55 * time.Second},
	})

	got := policy.Simulate(state, 1, settings)

	if !got.BreachExpected {
		t.Fatal("BreachExpected = false, want a breach")
	}
	if got.FirstBreachPriority != 50 {
		t.Errorf("FirstBreachPriority = %d, want 50 — it breaches first in time, "+
			"even though 100 is the more urgent level", got.FirstBreachPriority)
	}
}

func TestAPriorityWithNoWaitingJobsCannotBreach(t *testing.T) {
	settings := baseSettings()
	settings.Deadlines = map[domain.Priority]time.Duration{100: time.Nanosecond}

	// Depth 0 with a stale age reading: there is no job to miss a deadline.
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 0, OldestJobAge: time.Hour},
	})

	got := policy.Simulate(state, 1, settings)

	if got.BreachExpected {
		t.Errorf("BreachExpected = true for an empty level: %+v", got)
	}
}

func TestAnUnconfiguredPriorityUsesTheDefaultDeadline(t *testing.T) {
	settings := baseSettings()
	settings.Deadlines = map[domain.Priority]time.Duration{}
	settings.DefaultDeadline = 30 * time.Second

	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		7: {Depth: 5, OldestJobAge: 25 * time.Second},
	})

	got := policy.Simulate(state, 0, settings)

	if !got.BreachExpected || got.FirstBreachPriority != 7 {
		t.Errorf("Simulate() = %+v, want a breach at the unconfigured level 7 "+
			"using the default deadline", got)
	}
}

// Every decision has to be reproducible from its observation, or a replayed
// run cannot be used to explain a live one.
func TestSimulateIsDeterministic(t *testing.T) {
	settings := baseSettings()
	state := stateWith(0.7, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 31, OldestJobAge: 12 * time.Second, ArrivalRate: 0.9},
		50:  {Depth: 17, OldestJobAge: 90 * time.Second, ArrivalRate: 0.3},
		25:  {Depth: 200, OldestJobAge: 40 * time.Minute, ArrivalRate: 0.1},
	})

	first := policy.Simulate(state, 3, settings)
	for i := 0; i < 25; i++ {
		if got := policy.Simulate(state, 3, settings); got != first {
			t.Fatalf("Simulate() run %d = %+v, want the identical %+v", i, got, first)
		}
	}
}

func TestSimulateNeverReportsABreachBeyondTheHorizon(t *testing.T) {
	settings := baseSettings()
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 1000, OldestJobAge: 0, ArrivalRate: 5},
	})

	got := policy.Simulate(state, 0, settings)

	if got.FirstBreachIn > settings.Horizon {
		t.Errorf("FirstBreachIn = %v, want no more than the horizon %v",
			got.FirstBreachIn, settings.Horizon)
	}
}

func TestSimulateDoesNotMutateTheObservation(t *testing.T) {
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 40, OldestJobAge: 5 * time.Second, ArrivalRate: 1},
	})

	policy.Simulate(state, 2, baseSettings())

	if got := state.Queues[100]; got.Depth != 40 || got.OldestJobAge != 5*time.Second {
		t.Errorf("observation was mutated: %+v", got)
	}
}
