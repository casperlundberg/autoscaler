package policy_test

import (
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

var noon = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// engineSettings is deliberately small and round so the expected plans in
// these tests can be worked out by hand.
func engineSettings() config.Settings {
	s := config.DefaultSettings()
	s.Horizon = 10 * time.Minute
	s.SimulationStep = 10 * time.Second
	s.Deadlines = map[domain.Priority]time.Duration{100: time.Minute}
	s.DefaultDeadline = time.Hour
	s.LocalExecutorCap = 10
	s.CloudExecutorCap = 20
	s.MinLocalExecutors = 1
	s.SafetyFactor = 1.0
	s.ScaleUpCooldown = 0
	s.ScaleDownCooldown = 0
	s.MaxScaleUpStep = 100
	s.MaxScaleDownStep = 100
	s.CloudMinLifetime = 0
	// These tests work plans out by hand, so capacity serves from the first
	// step unless a test says otherwise. The coldstart tests set their own.
	return noColdstart(s)
}

func observation(at time.Time, capacity domain.Capacity, queues map[domain.Priority]domain.QueueInfo) domain.SystemState {
	return domain.SystemState{
		Timestamp:          at,
		Queues:             queues,
		Capacity:           capacity,
		ExecutorThroughput: 1,
	}
}

func TestAQuietSystemHoldsAtTheLocalFloor(t *testing.T) {
	settings := engineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if got.Action != domain.ActionMaintain {
		t.Errorf("Action = %q, want maintain", got.Action)
	}
	if want := (domain.Plan{LocalExecutors: 1}); got.Plan != want {
		t.Errorf("Plan = %+v, want %+v", got.Plan, want)
	}
	if !got.IsNoop() {
		t.Error("IsNoop() = false, want true — nothing needs applying")
	}
}

func TestAPredictedBreachRaisesCapacity(t *testing.T) {
	settings := engineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 300, OldestJobAge: 40 * time.Second, ArrivalRate: 2},
	})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if got.Plan.Total() <= 1 {
		t.Fatalf("Plan = %+v, want more capacity than the single executor in place", got.Plan)
	}
	if got.Reason == "" {
		t.Error("Reason is empty; every decision has to say why it was taken")
	}
}

func TestOverflowBeyondTheLocalCapGoesToTheCloud(t *testing.T) {
	settings := engineSettings()
	// Far more work than ten local executors can serve inside a 60s deadline.
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 2000, OldestJobAge: 50 * time.Second, ArrivalRate: 5},
	})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if got.Plan.LocalExecutors != settings.LocalExecutorCap {
		t.Errorf("LocalExecutors = %d, want the local tier filled to its cap of %d first",
			got.Plan.LocalExecutors, settings.LocalExecutorCap)
	}
	if got.Plan.CloudExecutors == 0 {
		t.Error("CloudExecutors = 0, want the overflow to reach the cloud tier")
	}
	if got.Action != domain.ActionCloudBurst {
		t.Errorf("Action = %q, want cloud_burst", got.Action)
	}
}

func TestAScaleUpCooldownHoldsCapacityAndSaysSo(t *testing.T) {
	settings := engineSettings()
	settings.ScaleUpCooldown = 2 * time.Minute
	loop := domain.LoopState{LastScaleUp: noon.Add(-30 * time.Second)}

	state := observation(noon, domain.Capacity{LocalReady: 2}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 300, OldestJobAge: 40 * time.Second, ArrivalRate: 2},
	})

	got, _ := policy.Decide(state, loop, settings)

	if !got.IsNoop() {
		t.Errorf("Plan = %+v, want it held at the current %+v while the cooldown runs",
			got.Plan, got.Previous)
	}
	if !got.Constrained || !strings.Contains(got.Constraint, "cooldown") {
		t.Errorf("Constraint = %q (constrained=%v), want it to name the cooldown",
			got.Constraint, got.Constrained)
	}
}

func TestTheScaleUpCooldownDoesNotBlockAScaleDown(t *testing.T) {
	settings := engineSettings()
	settings.ScaleUpCooldown = time.Hour
	loop := domain.LoopState{LastScaleUp: noon.Add(-time.Second)}

	// Nothing to do, eight executors idle.
	state := observation(noon, domain.Capacity{LocalReady: 8}, map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, loop, settings)

	if got.Plan.LocalExecutors >= 8 {
		t.Errorf("Plan = %+v, want a reduction: the scale-up cooldown governs "+
			"increases only", got.Plan)
	}
}

func TestAScaleDownCooldownKeepsIdleCapacity(t *testing.T) {
	settings := engineSettings()
	settings.ScaleDownCooldown = 10 * time.Minute
	loop := domain.LoopState{LastScaleDown: noon.Add(-time.Minute)}

	state := observation(noon, domain.Capacity{LocalReady: 8}, map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, loop, settings)

	if !got.IsNoop() {
		t.Errorf("Plan = %+v, want it held at %+v while the scale-down cooldown runs",
			got.Plan, got.Previous)
	}
}

func TestScaleUpIsLimitedToOneStepPerCycle(t *testing.T) {
	settings := engineSettings()
	settings.MaxScaleUpStep = 3
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 2000, OldestJobAge: 50 * time.Second, ArrivalRate: 5},
	})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	localDelta, cloudDelta := got.Delta()
	if localDelta > 3 {
		t.Errorf("local delta = %d, want no more than the step limit of 3", localDelta)
	}
	if cloudDelta > 3 {
		t.Errorf("cloud delta = %d, want no more than the step limit of 3", cloudDelta)
	}
	if !got.Constrained {
		t.Error("Constrained = false, want the step limit recorded")
	}
}

func TestScaleDownIsLimitedToOneStepPerCycle(t *testing.T) {
	settings := engineSettings()
	settings.MaxScaleDownStep = 2
	state := observation(noon, domain.Capacity{LocalReady: 10, CloudReady: 5},
		map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	localDelta, cloudDelta := got.Delta()
	if localDelta < -2 {
		t.Errorf("local delta = %d, want no steeper than the step limit of -2", localDelta)
	}
	if cloudDelta < -2 {
		t.Errorf("cloud delta = %d, want no steeper than the step limit of -2", cloudDelta)
	}
}

// Cloud capacity is paid for the moment it is requested. Giving it back during
// a brief lull and buying it again minutes later is the most expensive
// possible way to run a burst.
func TestCloudCapacityIsHeldForItsMinimumLifetime(t *testing.T) {
	settings := engineSettings()
	settings.CloudMinLifetime = 10 * time.Minute
	loop := domain.LoopState{CloudSince: noon.Add(-2 * time.Minute)}

	state := observation(noon, domain.Capacity{LocalReady: 10, CloudReady: 6},
		map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, loop, settings)

	if got.Plan.CloudExecutors != 6 {
		t.Errorf("CloudExecutors = %d, want the 6 already paid for to be kept", got.Plan.CloudExecutors)
	}
	if !strings.Contains(got.Constraint, "lifetime") {
		t.Errorf("Constraint = %q, want it to name the cloud minimum lifetime", got.Constraint)
	}
}

func TestCloudCapacityIsReleasedOnceItsLifetimeHasElapsed(t *testing.T) {
	settings := engineSettings()
	settings.CloudMinLifetime = 10 * time.Minute
	loop := domain.LoopState{CloudSince: noon.Add(-30 * time.Minute)}

	state := observation(noon, domain.Capacity{LocalReady: 10, CloudReady: 6},
		map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, loop, settings)

	if got.Plan.CloudExecutors >= 6 {
		t.Errorf("CloudExecutors = %d, want the idle cloud tier released", got.Plan.CloudExecutors)
	}
}

func TestPendingExecutorsCountAsCapacityAlreadyRequested(t *testing.T) {
	settings := engineSettings()
	// Four ready and six still starting: the platform already holds ten.
	state := observation(noon, domain.Capacity{LocalReady: 4, LocalPending: 6},
		map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if want := (domain.Plan{LocalExecutors: 10}); got.Previous != want {
		t.Errorf("Previous = %+v, want %+v — pending executors are already "+
			"requested capacity, not capacity still to be asked for", got.Previous, want)
	}
}

func TestAnUnusableObservationHoldsTheCurrentPlan(t *testing.T) {
	settings := engineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 4}, map[domain.Priority]domain.QueueInfo{})
	state.ExecutorThroughput = 0 // never a real reading

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if !got.IsNoop() {
		t.Errorf("Plan = %+v, want capacity left exactly as it is when the "+
			"observation cannot be trusted", got.Plan)
	}
	if !strings.Contains(got.Reason, "executor_throughput") {
		t.Errorf("Reason = %q, want it to name the field that made the observation unusable", got.Reason)
	}
	if !got.Constrained {
		t.Error("Constrained = false, want the held plan flagged")
	}
}

func TestTheDecisionNeverExceedsEitherCap(t *testing.T) {
	settings := engineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 100000, OldestJobAge: 59 * time.Second, ArrivalRate: 900},
	})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if got.Plan.LocalExecutors > settings.LocalExecutorCap {
		t.Errorf("LocalExecutors = %d, want at most %d", got.Plan.LocalExecutors, settings.LocalExecutorCap)
	}
	if got.Plan.CloudExecutors > settings.CloudExecutorCap {
		t.Errorf("CloudExecutors = %d, want at most %d", got.Plan.CloudExecutors, settings.CloudExecutorCap)
	}
}

func TestTheLocalFloorSurvivesAStepLimitedRetreat(t *testing.T) {
	settings := engineSettings()
	settings.MinLocalExecutors = 3
	settings.MaxScaleDownStep = 100
	state := observation(noon, domain.Capacity{LocalReady: 10}, map[domain.Priority]domain.QueueInfo{})

	got, _ := policy.Decide(state, domain.LoopState{}, settings)

	if got.Plan.LocalExecutors < 3 {
		t.Errorf("LocalExecutors = %d, want the floor of 3 respected", got.Plan.LocalExecutors)
	}
}

func TestLoopStateRecordsWhenCapacityMoved(t *testing.T) {
	settings := engineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 300, OldestJobAge: 40 * time.Second, ArrivalRate: 2},
	})

	_, next := policy.Decide(state, domain.LoopState{}, settings)

	if !next.LastScaleUp.Equal(noon) {
		t.Errorf("LastScaleUp = %v, want the observation's own timestamp %v", next.LastScaleUp, noon)
	}
}

func TestLoopStateIsUntouchedWhenNothingMoves(t *testing.T) {
	settings := engineSettings()
	before := domain.LoopState{LastScaleUp: noon.Add(-time.Hour), LastScaleDown: noon.Add(-time.Hour)}
	state := observation(noon, domain.Capacity{LocalReady: 1}, map[domain.Priority]domain.QueueInfo{})

	got, next := policy.Decide(state, before, settings)

	if !got.IsNoop() {
		t.Fatalf("expected a no-op decision, got %+v", got.Plan)
	}
	if next != before {
		t.Errorf("LoopState = %+v, want it unchanged at %+v: a cycle that changed "+
			"nothing must not restart the cooldown clocks", next, before)
	}
}

func TestCloudSinceIsStampedWhenTheCloudTierGrows(t *testing.T) {
	settings := engineSettings()
	state := observation(noon, domain.Capacity{LocalReady: 10}, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 2000, OldestJobAge: 50 * time.Second, ArrivalRate: 5},
	})

	_, next := policy.Decide(state, domain.LoopState{}, settings)

	if !next.CloudSince.Equal(noon) {
		t.Errorf("CloudSince = %v, want %v — the clock starts when the cloud tier grows",
			next.CloudSince, noon)
	}
}

func TestCloudSinceClearsWhenTheCloudTierIsEmptied(t *testing.T) {
	settings := engineSettings()
	settings.MaxScaleDownStep = 100
	loop := domain.LoopState{CloudSince: noon.Add(-time.Hour)}
	state := observation(noon, domain.Capacity{LocalReady: 2, CloudReady: 4},
		map[domain.Priority]domain.QueueInfo{})

	got, next := policy.Decide(state, loop, settings)

	if got.Plan.CloudExecutors != 0 {
		t.Fatalf("CloudExecutors = %d, want the tier emptied", got.Plan.CloudExecutors)
	}
	if !next.CloudSince.IsZero() {
		t.Errorf("CloudSince = %v, want it cleared once no cloud capacity is held", next.CloudSince)
	}
}

func TestDecideIsDeterministic(t *testing.T) {
	settings := engineSettings()
	loop := domain.LoopState{LastScaleUp: noon.Add(-3 * time.Minute)}
	state := observation(noon, domain.Capacity{LocalReady: 3, CloudReady: 2, LocalPending: 1},
		map[domain.Priority]domain.QueueInfo{
			100: {Depth: 143, OldestJobAge: 21 * time.Second, ArrivalRate: 1.7},
			50:  {Depth: 62, OldestJobAge: 4 * time.Minute, ArrivalRate: 0.4},
		})

	firstDecision, firstLoop := policy.Decide(state, loop, settings)
	for i := 0; i < 20; i++ {
		gotDecision, gotLoop := policy.Decide(state, loop, settings)
		if gotDecision != firstDecision || gotLoop != firstLoop {
			t.Fatalf("Decide() run %d = (%+v, %+v), want the identical (%+v, %+v)",
				i, gotDecision, gotLoop, firstDecision, firstLoop)
		}
	}
}
