package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/controller"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/simulation"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

// seeing is a platform that reports its own queue, so a target on it is
// allowed to run autonomously. It wraps the simulation adapter so the
// provisioning side stays real.
type seeing struct{ *simulation.Adapter }

func (s seeing) Kind() platform.Kind { return "seeing" }

func (s seeing) Schema() platform.Schema {
	schema := s.Adapter.Schema()
	schema.Kind = "seeing"
	schema.SeesWorkload = true
	return schema
}

func (s seeing) Observe(ctx context.Context, target platform.Target) (platform.Observation, error) {
	observation, err := s.Adapter.Observe(ctx, target)
	if err != nil {
		return platform.Observation{}, err
	}
	observation.Workload = busyWorkload()
	return observation, nil
}

func runnerHarness(t *testing.T, mode platform.Mode) (*controller.Runner, *registry.Registry, *simulation.Adapter) {
	t.Helper()

	simulator := simulation.New()
	platforms, err := platform.NewRegistry(seeing{simulator})
	if err != nil {
		t.Fatalf("platform.NewRegistry() = %v", err)
	}
	reg, err := registry.New(platforms, registry.NewMemoryStore())
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	s := settings()
	s.DecisionInterval = time.Minute
	target := platform.Target{
		ID: "storhall", Kind: "seeing", Mode: mode,
		Config: map[string]string{"local_coldstart_seconds": "0", "cloud_coldstart_seconds": "0"},
	}
	if _, err := reg.Create(context.Background(), target, s); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	return controller.NewRunner(controller.New(platforms, reg), reg), reg, simulator
}

func TestAnAutonomousTargetIsCycledWithoutBeingAsked(t *testing.T) {
	runner, reg, _ := runnerHarness(t, platform.ModeAutonomous)

	runner.RunOnce(context.Background(), start)
	runner.Wait()

	snapshot, _ := reg.Get("storhall")
	if snapshot.LastDecision == nil {
		t.Error("LastDecision = nil, want the runner to have cycled the target")
	}
}

// A driven target belongs to whoever is driving it. A runner that cycled one
// anyway would fight a simulation run for control of the same target.
func TestADrivenTargetIsLeftAlone(t *testing.T) {
	runner, reg, _ := runnerHarness(t, platform.ModeDriven)

	runner.RunOnce(context.Background(), start)
	runner.Wait()

	snapshot, _ := reg.Get("storhall")
	if snapshot.LastDecision != nil {
		t.Error("LastDecision is set; the runner cycled a driven target")
	}
}

func TestATargetIsNotCycledFasterThanItsDecisionInterval(t *testing.T) {
	runner, reg, _ := runnerHarness(t, platform.ModeAutonomous)

	runner.RunOnce(context.Background(), start)
	runner.Wait()
	first, _ := reg.Get("storhall")

	runner.RunOnce(context.Background(), start.Add(10*time.Second))
	runner.Wait()
	second, _ := reg.Get("storhall")

	if !second.LastCycleAt.Equal(first.LastCycleAt) {
		t.Errorf("LastCycleAt moved to %v, want it held at %v — the interval is a minute",
			second.LastCycleAt, first.LastCycleAt)
	}
}

func TestATargetIsCycledAgainOnceItsIntervalHasPassed(t *testing.T) {
	runner, reg, _ := runnerHarness(t, platform.ModeAutonomous)

	runner.RunOnce(context.Background(), start)
	runner.Wait()
	runner.RunOnce(context.Background(), start.Add(2*time.Minute))
	runner.Wait()

	snapshot, _ := reg.Get("storhall")
	if !snapshot.LastCycleAt.Equal(start.Add(2 * time.Minute)) {
		t.Errorf("LastCycleAt = %v, want the second cycle at %v",
			snapshot.LastCycleAt, start.Add(2*time.Minute))
	}
}

// Changing the cadence is a settings change like any other, and must take
// effect without the service being restarted.
func TestShorteningTheDecisionIntervalTakesEffectImmediately(t *testing.T) {
	runner, reg, _ := runnerHarness(t, platform.ModeAutonomous)

	runner.RunOnce(context.Background(), start)
	runner.Wait()

	if _, err := reg.ApplySettings("storhall",
		[]byte(`{"decision_interval_seconds": 5}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	runner.RunOnce(context.Background(), start.Add(10*time.Second))
	runner.Wait()

	snapshot, _ := reg.Get("storhall")
	if !snapshot.LastCycleAt.Equal(start.Add(10 * time.Second)) {
		t.Errorf("LastCycleAt = %v, want a cycle at %v under the new five-second interval",
			snapshot.LastCycleAt, start.Add(10*time.Second))
	}
}

func TestATargetRegisteredLaterIsPickedUpWithoutARestart(t *testing.T) {
	runner, reg, _ := runnerHarness(t, platform.ModeAutonomous)

	s := settings()
	s.DecisionInterval = time.Minute
	later := platform.Target{
		ID: "kvarnberg", Kind: "seeing", Mode: platform.ModeAutonomous,
		Config: map[string]string{"local_coldstart_seconds": "0"},
	}
	if _, err := reg.Create(context.Background(), later, s); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	runner.RunOnce(context.Background(), start)
	runner.Wait()

	snapshot, _ := reg.Get("kvarnberg")
	if snapshot.LastDecision == nil {
		t.Error("the newly registered target was never cycled")
	}
}

func TestARemovedTargetStopsBeingCycled(t *testing.T) {
	runner, reg, simulator := runnerHarness(t, platform.ModeAutonomous)

	runner.RunOnce(context.Background(), start)
	runner.Wait()
	before := len(simulator.History("storhall"))

	if err := reg.Delete("storhall"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	runner.RunOnce(context.Background(), start.Add(2*time.Minute))
	runner.Wait()

	if got := len(simulator.History("storhall")); got != before {
		t.Errorf("the deleted target was cycled again: %d plans, want %d", got, before)
	}
}

func TestRunStopsWhenItsContextIsCancelled(t *testing.T) {
	runner, _, _ := runnerHarness(t, platform.ModeAutonomous)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		runner.Run(ctx, 10*time.Millisecond)
		close(stopped)
	}()

	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after its context was cancelled")
	}
}

// One target failing must not stop the others, or a single unreachable cluster
// takes the whole service down with it.
func TestOneFailingTargetDoesNotStopTheRest(t *testing.T) {
	simulator := simulation.New()
	platforms, err := platform.NewRegistry(seeing{simulator}, failing{})
	if err != nil {
		t.Fatalf("platform.NewRegistry() = %v", err)
	}
	reg, err := registry.New(platforms, registry.NewMemoryStore())
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	s := settings()
	s.DecisionInterval = time.Minute
	for _, target := range []platform.Target{
		{ID: "broken", Kind: "failing", Mode: platform.ModeAutonomous},
		{ID: "healthy", Kind: "seeing", Mode: platform.ModeAutonomous,
			Config: map[string]string{"local_coldstart_seconds": "0"}},
	} {
		if _, err := reg.Create(context.Background(), target, s); err != nil {
			t.Fatalf("Create(%s) = %v", target.ID, err)
		}
	}

	runner := controller.NewRunner(controller.New(platforms, reg), reg)
	runner.RunOnce(context.Background(), start)
	runner.Wait()

	broken, _ := reg.Get("broken")
	if broken.LastError == "" {
		t.Error("the failing target's error was not recorded")
	}
	healthy, _ := reg.Get("healthy")
	if healthy.LastDecision == nil {
		t.Error("the healthy target was not cycled")
	}
}

// failing is a platform that registers fine and then cannot be observed —
// exactly what an expired token or an unreachable API server looks like.
type failing struct{}

func (failing) Kind() platform.Kind { return "failing" }
func (failing) Schema() platform.Schema {
	return platform.Schema{Kind: "failing", Summary: "always fails to observe", SeesWorkload: true}
}
func (failing) Validate(context.Context, platform.Target) error { return nil }
func (failing) Observe(context.Context, platform.Target) (platform.Observation, error) {
	return platform.Observation{}, context.DeadlineExceeded
}
func (failing) Apply(context.Context, platform.Target, domain.Plan) (platform.ApplyResult, error) {
	return platform.ApplyResult{}, nil
}
