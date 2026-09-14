package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

// Coldstart in the decision engine.
//
// The setting existed, and said in its own comment that it is "what makes
// acting early rational", but nothing in policy read it: Simulate served the
// whole candidate count from the first step, and Capacity.AsPlan folded
// pending executors into the count the projection ran against. So the engine
// answered a question nobody asks — what count would avoid this breach if
// capacity were instant — and answered it most confidently in the one
// situation where it is least true, a cold pool during a ramp.
//
// These tests pin the three things that changed: capacity ramps, an
// unavoidable breach is told apart from an overload, and the reasoning says
// which it was.

func coldEngineSettings() config.Settings {
	s := engineSettings()
	s.LocalColdstart = 2 * time.Minute
	s.CloudColdstart = 5 * time.Minute
	return s
}

// The one that matters for the cloud bill. A cold pool facing a deadline
// shorter than its coldstart cannot avoid the breach at any executor count —
// and the old answer to "no count avoids it" was to run flat out at both caps.
// That would spend the whole cloud budget every time a quiet pool woke up, on
// a breach that was already committed before the decision was taken.
func TestAnUnavoidableBreachAsksForWhatTheQueueNeeds_NotTheCeiling(t *testing.T) {
	settings := noSafety(coldSettings())
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		// 55s old against a 60s deadline, with nothing able to start for two
		// minutes. No count avoids this.
		100: {Depth: 20, OldestJobAge: 55 * time.Second, ArrivalRate: 0.1},
	})

	got := policy.Required(state, settings)

	if got.Outcome != policy.Unavoidable {
		t.Fatalf("Outcome = %v, want unavoidable: the breach lands inside the "+
			"coldstart, so no executor count avoids it", got.Outcome)
	}
	if got.Executors >= settings.LocalExecutorCap+settings.CloudExecutorCap {
		t.Errorf("Executors = %d, want well under the combined ceiling of %d: "+
			"provisioning everything cannot un-commit a breach and bills for the attempt",
			got.Executors, settings.LocalExecutorCap+settings.CloudExecutorCap)
	}
	// The count asked for is the steady-state need, and it is enough to hold
	// the queue once it has started.
	if policy.Simulate(state, policy.Instant(got.Executors), settings).BreachExpected {
		t.Errorf("Executors = %d does not hold the queue even serving from now, "+
			"so it is not the steady-state requirement", got.Executors)
	}
	// And the operator is still told a breach is coming.
	if !got.Projection.BreachExpected {
		t.Error("Projection.BreachExpected = false, want the breach reported: " +
			"it is unavoidable, not absent")
	}
}

// The same queue, with room between the deadline and the coldstart, is an
// ordinary achievable requirement again.
func TestABreachBeyondTheColdstartIsStillAvoidable(t *testing.T) {
	settings := noSafety(coldSettings())
	settings.Deadlines = map[domain.Priority]time.Duration{100: 4 * time.Minute}
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 20, OldestJobAge: 55 * time.Second, ArrivalRate: 0.1},
	})

	got := policy.Required(state, settings)

	if got.Outcome != policy.Achievable {
		t.Fatalf("Outcome = %v, want achievable: local capacity arrives at %s, "+
			"well inside a %s deadline", got.Outcome, settings.LocalColdstart,
			settings.Deadlines[100])
	}
	if got.Executors == 0 {
		t.Error("Executors = 0, but this queue breaches if nothing is provisioned")
	}
}

// Coldstart must not swallow a real overload. When arrivals outrun both caps
// serving from the first instant, capacity is the binding constraint and
// running flat out remains the right answer.
func TestAGenuineOverloadStillRunsFlatOut(t *testing.T) {
	settings := noSafety(coldSettings())
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 5000, OldestJobAge: 50 * time.Second, ArrivalRate: 500},
	})

	got := policy.Required(state, settings)

	ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap
	if got.Outcome != policy.Overloaded {
		t.Fatalf("Outcome = %v, want overloaded: 500 jobs/s against a ceiling of %d",
			got.Outcome, ceiling)
	}
	if got.Executors != ceiling {
		t.Errorf("Executors = %d, want the ceiling of %d", got.Executors, ceiling)
	}
}

// The defect this whole change is about, stated as one comparison: the same
// number of executors, ready in one case and starting in the other, must not
// produce the same projection.
func TestTenExecutorsStartingAreNotTenExecutorsWorking(t *testing.T) {
	settings := coldSettings()
	queues := map[domain.Priority]domain.QueueInfo{
		100: {Depth: 400, OldestJobAge: 40 * time.Second, ArrivalRate: 1},
	}

	warm := stateWith(1, queues)
	warm.Capacity = domain.Capacity{LocalReady: 10}
	cold := stateWith(1, queues)
	cold.Capacity = domain.Capacity{LocalPending: 10}

	plan := domain.Plan{LocalExecutors: 10}
	warmProjection := policy.Simulate(warm, policy.AvailabilityOf(plan, warm.Capacity, settings), settings)
	coldProjection := policy.Simulate(cold, policy.AvailabilityOf(plan, cold.Capacity, settings), settings)

	if warmProjection.BreachExpected {
		t.Fatalf("ten ready executors breach: %+v — pick a fixture where they do not",
			warmProjection)
	}
	if !coldProjection.BreachExpected {
		t.Error("ten executors that cannot take a job for two minutes are projected " +
			"to keep the queue inside its deadline, which is the optimism this " +
			"change exists to remove")
	}
}

// A warm floor is worth paying for, and now the engine can show why rather
// than only assert it in a comment.
func TestAWarmFloorAvoidsABreachAColdPoolCannot(t *testing.T) {
	settings := noSafety(coldSettings())
	settings.MinLocalExecutors = 2
	queues := map[domain.Priority]domain.QueueInfo{
		100: {Depth: 15, OldestJobAge: 45 * time.Second, ArrivalRate: 0.2},
	}

	warm := stateWith(1, queues)
	warm.Capacity = domain.Capacity{LocalReady: 2}
	cold := stateWith(1, queues)

	if got := policy.Required(warm, settings); got.Outcome != policy.Achievable {
		t.Errorf("with the floor already warm, Outcome = %v, want achievable", got.Outcome)
	}
	if got := policy.Required(cold, settings); got.Outcome != policy.Unavoidable {
		t.Errorf("from cold, Outcome = %v, want unavoidable — that difference is "+
			"what the floor buys", got.Outcome)
	}
}

// Every decision carries the reasoning that produced it, so when the answer is
// "nothing could have avoided this" the sentence has to say so. An operator
// reading a run log otherwise sees a breach next to a modest executor count
// and concludes the engine under-provisioned.
func TestTheReasoningSaysWhenABreachCouldNotHaveBeenAvoided(t *testing.T) {
	settings := coldEngineSettings()
	state := observation(noon, domain.Capacity{}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 20, OldestJobAge: 55 * time.Second, ArrivalRate: 0.1},
	})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if !strings.Contains(got.Reason, "no executor count avoids it") {
		t.Errorf("Reason = %q, want it to say the breach was unavoidable", got.Reason)
	}
	if !strings.Contains(got.Reason, "coldstart") {
		t.Errorf("Reason = %q, want it to name coldstart as the reason", got.Reason)
	}
}

// The same number means different things depending on how much of it is
// working, so the sentence explaining a decision has to distinguish them.
func TestTheReasoningSaysHowMuchCapacityIsStillStarting(t *testing.T) {
	settings := coldEngineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 1, LocalPending: 6},
		map[domain.Priority]domain.QueueInfo{
			50: {Depth: 3, OldestJobAge: 10 * time.Second, ArrivalRate: 0.1},
		})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if !strings.Contains(got.Reason, "6 still starting") {
		t.Errorf("Reason = %q, want it to say how many of the executors cannot "+
			"work yet", got.Reason)
	}
}

// Coldstart is a setting, so switching it off has to restore the old
// behaviour exactly. This is what lets the rest of the suite reason about the
// search without the ramp in the way, and it is a real guarantee: a platform
// whose executors really do start instantly should not pay for a model of
// delay it does not have.
func TestAZeroColdstartMakesCapacityImmediate(t *testing.T) {
	settings := noColdstart(coldSettings())
	state := stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 200, OldestJobAge: 30 * time.Second, ArrivalRate: 1},
	})

	plan := domain.Plan{LocalExecutors: 4, CloudExecutors: 2}
	got := policy.AvailabilityOf(plan, domain.Capacity{}, settings)

	if got.Serving(0) != 6 {
		t.Errorf("Serving(0) = %d, want all 6 immediately", got.Serving(0))
	}
	ramped := policy.Simulate(state, got, settings)
	instant := policy.Simulate(state, policy.Instant(6), settings)
	if ramped != instant {
		t.Errorf("a zero coldstart projected %+v, want the same as instant capacity %+v",
			ramped, instant)
	}
}

// Found by platform-experiments/experiments/004-coldstart-and-the-cloud-bill.
//
// An unavoidable breach reported beside a large fleet that is entirely still
// starting reads as nonsense: there is obviously plenty of capacity, so why is
// the engine giving up? The answer is that none of it can work yet, and that
// is the one fact the sentence was leaving out — on the branch where it
// matters most, because "no executor count avoids it" is precisely the claim
// an operator will want to argue with.
func TestAnUnavoidableBreachSaysHowMuchCapacityIsAlreadyStarting(t *testing.T) {
	settings := coldEngineSettings()
	state := observation(noon, domain.Capacity{LocalPending: 40},
		map[domain.Priority]domain.QueueInfo{
			100: {Depth: 300, OldestJobAge: 55 * time.Second, ArrivalRate: 1},
		})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if !strings.Contains(got.Reason, "no executor count avoids it") {
		t.Fatalf("Reason = %q, want the unavoidable branch for this fixture", got.Reason)
	}
	if !strings.Contains(got.Reason, "40 still starting") {
		t.Errorf("Reason = %q, want it to say that all 40 executors are still "+
			"starting — without that, giving up next to a fleet of 40 looks like "+
			"a bug rather than an explanation", got.Reason)
	}
}
