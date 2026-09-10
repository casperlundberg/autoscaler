package controller_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/controller"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/simulation"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

var start = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func settings() config.Settings {
	s := config.DefaultSettings()
	s.Horizon = 10 * time.Minute
	s.SimulationStep = 10 * time.Second
	s.Deadlines = map[domain.Priority]time.Duration{100: time.Minute}
	s.DefaultDeadline = time.Hour
	s.LocalExecutorCap = 10
	s.CloudExecutorCap = 20
	s.MinLocalExecutors = 0
	s.SafetyFactor = 1.0
	s.ScaleDownCooldown = 0
	s.MaxScaleUpStep = 100
	s.MaxScaleDownStep = 100
	s.CloudMinLifetime = 0
	return s
}

// harness wires the real controller to the real simulation adapter and a real
// registry. Nothing here is a mock: the point of the simulation adapter is
// that the whole path can be exercised for real without a cluster.
type harness struct {
	controller *controller.Controller
	registry   *registry.Registry
	simulator  *simulation.Adapter
}

func newHarness(t *testing.T, mode platform.Mode) *harness {
	t.Helper()

	simulator := simulation.New()
	platforms, err := platform.NewRegistry(simulator)
	if err != nil {
		t.Fatalf("platform.NewRegistry() = %v", err)
	}
	reg, err := registry.New(platforms, registry.NewMemoryStore())
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	target := platform.Target{
		ID: "run-1", Name: "Simulated run", Kind: platform.KindSimulation, Mode: mode,
		Config: map[string]string{"local_coldstart_seconds": "0", "cloud_coldstart_seconds": "0"},
	}
	if _, err := reg.Create(context.Background(), target, settings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	return &harness{
		controller: controller.New(platforms, reg),
		registry:   reg,
		simulator:  simulator,
	}
}

func busyWorkload() *platform.Workload {
	return &platform.Workload{
		Queues: map[domain.Priority]domain.QueueInfo{
			100: {Depth: 400, OldestJobAge: 50 * time.Second, ArrivalRate: 2},
		},
		ExecutorThroughput: 1,
	}
}

func TestADrivenCycleDecidesFromTheSuppliedWorkload(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	got, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}

	if got.Decision.Plan.Total() == 0 {
		t.Errorf("Plan = %+v, want capacity for a queue that is about to breach", got.Decision.Plan)
	}
	if got.Decision.Reason == "" {
		t.Error("Reason is empty")
	}
}

// Without this, a decision read back a week later cannot be explained: the
// settings it was taken under have moved on.
func TestEveryDecisionIsStampedWithTheSettingsItWasTakenUnder(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	got, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}
	if got.Decision.SettingsVersion != 1 {
		t.Errorf("SettingsVersion = %d, want 1", got.Decision.SettingsVersion)
	}

	if _, err := h.registry.ApplySettings("run-1", []byte(`{"local_executor_cap": 4}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	got, err = h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start.Add(time.Minute), Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("second Cycle() = %v", err)
	}
	if got.Decision.SettingsVersion != 2 {
		t.Errorf("SettingsVersion = %d, want 2", got.Decision.SettingsVersion)
	}
}

// The headline requirement: a settings change is in force on the very next
// cycle, with nothing restarted and nothing reloaded.
func TestASettingsChangeTakesEffectOnTheNextCycle(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	first, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}
	if first.Decision.Plan.LocalExecutors <= 2 {
		t.Fatalf("Plan = %+v, want the first cycle to want more than two executors",
			first.Decision.Plan)
	}

	if _, err := h.registry.ApplySettings("run-1",
		[]byte(`{"local_executor_cap": 2, "cloud_executor_cap": 0}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	second, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start.Add(time.Minute), Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("second Cycle() = %v", err)
	}
	if second.Decision.Plan.LocalExecutors > 2 || second.Decision.Plan.CloudExecutors != 0 {
		t.Errorf("Plan = %+v, want the new caps honoured immediately", second.Decision.Plan)
	}
}

func TestTheDecisionIsAppliedToThePlatform(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	got, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}

	if got.Applied == nil {
		t.Fatal("Applied = nil, want the plan enacted")
	}
	history := h.simulator.History("run-1")
	if len(history) != 1 || history[0].Plan != got.Decision.Plan {
		t.Errorf("the adapter was asked for %+v, want %+v", history, got.Decision.Plan)
	}
}

// Dry run is how a real target is introduced to production: decisions are
// computed and recorded so they can be read, and nothing is created.
func TestDryRunDecidesButNeverProvisions(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)
	if _, err := h.registry.ApplySettings("run-1", []byte(`{"dry_run": true}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	got, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}

	if got.Decision.Plan.Total() == 0 {
		t.Error("Plan is empty; dry run should still produce a real decision")
	}
	if got.Applied != nil {
		t.Errorf("Applied = %+v, want nothing enacted", got.Applied)
	}
	if len(h.simulator.History("run-1")) != 0 {
		t.Error("the platform was asked to change something during a dry run")
	}
}

func TestANoOpDecisionIsNotSentToThePlatform(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)
	quiet := &platform.Workload{ExecutorThroughput: 1}

	got, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: quiet,
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}

	if !got.Decision.IsNoop() {
		t.Fatalf("Decision = %+v, want a no-op", got.Decision)
	}
	// Re-applying an unchanged plan every fifteen seconds is a write per
	// target per cycle that says nothing.
	if len(h.simulator.History("run-1")) != 0 {
		t.Error("an unchanged plan was still pushed to the platform")
	}
}

func TestCapacityComesFromThePlatformNotFromTheCaller(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	if _, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	}); err != nil {
		t.Fatalf("first Cycle() = %v", err)
	}

	second, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start.Add(time.Minute), Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("second Cycle() = %v", err)
	}

	// The second cycle must see what the first one provisioned. A controller
	// that took the caller's word for current capacity would re-issue the same
	// scale-up forever.
	if second.Decision.Previous.Total() == 0 {
		t.Errorf("Previous = %+v, want the capacity the first cycle created",
			second.Decision.Previous)
	}
}

func TestHysteresisMemoryCarriesBetweenCycles(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)
	if _, err := h.registry.ApplySettings("run-1",
		[]byte(`{"scale_down_cooldown_seconds": 600}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	// Build capacity up, then let the queue empty.
	if _, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	}); err != nil {
		t.Fatalf("Cycle() = %v", err)
	}
	quiet := &platform.Workload{ExecutorThroughput: 1}
	if _, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start.Add(time.Minute), Workload: quiet,
	}); err != nil {
		t.Fatalf("second Cycle() = %v", err)
	}

	third, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start.Add(2 * time.Minute), Workload: quiet,
	})
	if err != nil {
		t.Fatalf("third Cycle() = %v", err)
	}
	// The scale-down on the second cycle started a ten-minute cooldown, and
	// the third cycle has to still be inside it.
	if !third.IsNoop() && !strings.Contains(third.Decision.Constraint, "cooldown") {
		t.Errorf("third cycle = %+v, want the cooldown from the second still in force",
			third.Decision)
	}
}

func TestADrivenTargetNeedsAWorkload(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	_, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{At: start})
	if err == nil {
		t.Fatal("Cycle() = nil error with no workload supplied")
	}
	if !strings.Contains(err.Error(), "workload") {
		t.Errorf("Cycle() = %q, want it to say what is missing", err)
	}
}

func TestAnUnknownTargetIsReported(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	if _, err := h.controller.Cycle(context.Background(), "nobody", controller.Input{At: start}); err == nil {
		t.Fatal("Cycle() = nil error for an unknown target")
	}
}

func TestTheCycleIsRecordedAgainstTheTarget(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	if _, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: start, Workload: busyWorkload(),
	}); err != nil {
		t.Fatalf("Cycle() = %v", err)
	}

	snapshot, err := h.registry.Get("run-1")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if snapshot.LastDecision == nil {
		t.Error("LastDecision = nil, want the cycle recorded for the status view")
	}
	if !snapshot.LastCycleAt.Equal(start) {
		t.Errorf("LastCycleAt = %v, want the cycle's own clock %v", snapshot.LastCycleAt, start)
	}
}

// A failing cycle has to be visible on the target, not only returned to
// whoever happened to call it.
func TestAFailedCycleIsRecordedToo(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)

	if _, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{At: start}); err == nil {
		t.Fatal("expected the cycle to fail")
	}

	snapshot, _ := h.registry.Get("run-1")
	if snapshot.LastError == "" {
		t.Error("LastError is empty, want the failure recorded against the target")
	}
}

func TestTheCyclesClockReachesTheAdapter(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)
	simulated := time.Date(2019, 5, 6, 7, 8, 9, 0, time.UTC)

	if _, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		At: simulated, Workload: busyWorkload(),
	}); err != nil {
		t.Fatalf("Cycle() = %v", err)
	}

	history := h.simulator.History("run-1")
	if len(history) != 1 || !history[0].At.Equal(simulated) {
		t.Errorf("the adapter recorded %+v, want the run's own clock %v", history, simulated)
	}
}

func TestWithNoTimestampTheCycleUsesTheWallClock(t *testing.T) {
	h := newHarness(t, platform.ModeDriven)
	before := time.Now().UTC().Add(-time.Second)

	got, err := h.controller.Cycle(context.Background(), "run-1", controller.Input{
		Workload: busyWorkload(),
	})
	if err != nil {
		t.Fatalf("Cycle() = %v", err)
	}
	if got.Decision.At.Before(before) {
		t.Errorf("Decision.At = %v, want roughly now", got.Decision.At)
	}
}
