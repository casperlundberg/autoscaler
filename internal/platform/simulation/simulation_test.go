package simulation_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/simulation"
)

var start = time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)

func target(config map[string]string) platform.Target {
	return platform.Target{ID: "run-1", Name: "simulated mine", Kind: platform.KindSimulation, Config: config}
}

func at(t time.Time) context.Context {
	return platform.WithCycleTime(context.Background(), t)
}

func observe(t *testing.T, a *simulation.Adapter, tgt platform.Target, when time.Time) domain.Capacity {
	t.Helper()
	got, err := a.Observe(at(when), tgt)
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}
	return got.Capacity
}

func apply(t *testing.T, a *simulation.Adapter, tgt platform.Target, plan domain.Plan, when time.Time) platform.ApplyResult {
	t.Helper()
	got, err := a.Apply(at(when), tgt, plan)
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}
	return got
}

func TestTheAdapterAnnouncesItselfCorrectly(t *testing.T) {
	a := simulation.New()

	if a.Kind() != platform.KindSimulation {
		t.Errorf("Kind() = %q, want %q", a.Kind(), platform.KindSimulation)
	}
	// A simulation has no queue of its own: the workload is whatever log the
	// run is replaying, and it arrives with the request.
	if a.Schema().SeesWorkload {
		t.Error("Schema().SeesWorkload = true, want false")
	}
}

func TestAFreshTargetHoldsNothing(t *testing.T) {
	got := observe(t, simulation.New(), target(nil), start)

	if got != (domain.Capacity{}) {
		t.Errorf("Capacity = %+v, want everything zero", got)
	}
}

func TestNewExecutorsAreNotUsableUntilTheirColdstartElapses(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{"local_coldstart_seconds": "120"})

	apply(t, a, tgt, domain.Plan{LocalExecutors: 4}, start)

	// The whole point of modelling coldstart: capacity requested is not
	// capacity available, and a controller that conflates the two orders the
	// same scale-up over and over.
	during := observe(t, a, tgt, start.Add(60*time.Second))
	if during.LocalReady != 0 || during.LocalPending != 4 {
		t.Errorf("at 60s: %+v, want 0 ready and 4 pending", during)
	}

	after := observe(t, a, tgt, start.Add(121*time.Second))
	if after.LocalReady != 4 || after.LocalPending != 0 {
		t.Errorf("at 121s: %+v, want 4 ready and 0 pending", after)
	}
}

func TestCloudExecutorsUseTheirOwnColdstart(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{
		"local_coldstart_seconds": "60",
		"cloud_coldstart_seconds": "300",
	})

	apply(t, a, tgt, domain.Plan{LocalExecutors: 2, CloudExecutors: 3}, start)

	got := observe(t, a, tgt, start.Add(90*time.Second))
	if got.LocalReady != 2 {
		t.Errorf("LocalReady = %d, want 2 — the local tier is warm by 90s", got.LocalReady)
	}
	if got.CloudPending != 3 {
		t.Errorf("CloudPending = %d, want 3 — the cloud tier is not", got.CloudPending)
	}
}

func TestAZeroColdstartMakesCapacityImmediate(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{"local_coldstart_seconds": "0"})

	apply(t, a, tgt, domain.Plan{LocalExecutors: 5}, start)

	if got := observe(t, a, tgt, start); got.LocalReady != 5 {
		t.Errorf("LocalReady = %d, want 5 immediately", got.LocalReady)
	}
}

// Scaling down should give back the executors that cost the least to lose.
// A pending one has not started working yet; a warm one has already paid for
// its coldstart and would have to pay again if it were rebuilt.
func TestScalingDownGivesBackTheNewestExecutorsFirst(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{"local_coldstart_seconds": "100"})

	apply(t, a, tgt, domain.Plan{LocalExecutors: 3}, start)
	warm := start.Add(200 * time.Second)
	apply(t, a, tgt, domain.Plan{LocalExecutors: 6}, warm) // 3 warm, 3 starting
	apply(t, a, tgt, domain.Plan{LocalExecutors: 3}, warm.Add(10*time.Second))

	got := observe(t, a, tgt, warm.Add(10*time.Second))
	if got.LocalReady != 3 || got.LocalPending != 0 {
		t.Errorf("Capacity = %+v, want the 3 warm executors kept and the 3 "+
			"still starting released", got)
	}
}

func TestScalingBelowTheWarmCountReleasesWarmExecutorsToo(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{"local_coldstart_seconds": "10"})

	apply(t, a, tgt, domain.Plan{LocalExecutors: 6}, start)
	later := start.Add(time.Minute)
	apply(t, a, tgt, domain.Plan{LocalExecutors: 2}, later)

	if got := observe(t, a, tgt, later); got.LocalReady != 2 {
		t.Errorf("LocalReady = %d, want 2", got.LocalReady)
	}
}

func TestApplyingTheSamePlanTwiceChangesNothing(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{"local_coldstart_seconds": "100"})
	plan := domain.Plan{LocalExecutors: 4}

	apply(t, a, tgt, plan, start)
	second := apply(t, a, tgt, plan, start.Add(30*time.Second))

	if second.Changed {
		t.Error("Changed = true for a plan that was already in force")
	}
	// Re-applying must not restart anyone's coldstart, or a controller that
	// re-sends its plan each cycle would keep its executors permanently
	// pending.
	if got := observe(t, a, tgt, start.Add(101*time.Second)); got.LocalReady != 4 {
		t.Errorf("LocalReady = %d at 101s, want 4: re-applying restarted the coldstart", got.LocalReady)
	}
}

func TestApplyReportsTheCapacityNowHeld(t *testing.T) {
	a := simulation.New()
	tgt := target(nil)
	plan := domain.Plan{LocalExecutors: 7, CloudExecutors: 2}

	got := apply(t, a, tgt, plan, start)

	if got.Applied != plan {
		t.Errorf("Applied = %+v, want %+v — a simulation always grants what is asked",
			got.Applied, plan)
	}
	if !got.Changed {
		t.Error("Changed = false for a plan that added executors")
	}
}

func TestObserveReportsNoWorkload(t *testing.T) {
	a := simulation.New()

	got, err := a.Observe(at(start), target(nil))
	if err != nil {
		t.Fatalf("Observe() = %v", err)
	}
	if got.Workload != nil {
		t.Errorf("Workload = %+v, want nil: the queue belongs to the log being "+
			"replayed, not to this adapter", got.Workload)
	}
}

func TestTargetsDoNotSeeEachOthersExecutors(t *testing.T) {
	a := simulation.New()
	one := platform.Target{ID: "run-1", Kind: platform.KindSimulation}
	two := platform.Target{ID: "run-2", Kind: platform.KindSimulation}

	apply(t, a, one, domain.Plan{LocalExecutors: 5}, start)

	if got := observe(t, a, two, start); got.Total() != 0 {
		t.Errorf("second target sees %+v, want nothing", got)
	}
}

func TestResetClearsATargetsFleet(t *testing.T) {
	a := simulation.New()
	tgt := target(map[string]string{"local_coldstart_seconds": "0"})
	apply(t, a, tgt, domain.Plan{LocalExecutors: 5}, start)

	a.Reset(tgt.ID)

	// Without this, a second run would begin holding the first run's
	// executors and count them as capacity it never provisioned.
	if got := observe(t, a, tgt, start); got.Total() != 0 {
		t.Errorf("Capacity after Reset = %+v, want nothing", got)
	}
}

func TestHistoryRecordsWhatWasAskedForAndWhen(t *testing.T) {
	a := simulation.New()
	tgt := target(nil)

	apply(t, a, tgt, domain.Plan{LocalExecutors: 2}, start)
	apply(t, a, tgt, domain.Plan{LocalExecutors: 5}, start.Add(time.Minute))

	history := a.History(tgt.ID)
	if len(history) != 2 {
		t.Fatalf("History() returned %d entries, want 2", len(history))
	}
	if history[0].Plan.LocalExecutors != 2 || !history[0].At.Equal(start) {
		t.Errorf("history[0] = %+v, want the first plan at %v", history[0], start)
	}
	if history[1].Plan.LocalExecutors != 5 {
		t.Errorf("history[1] = %+v, want the second plan", history[1])
	}
}

func TestValidateAcceptsATargetWithNoCredentials(t *testing.T) {
	if err := simulation.New().Validate(context.Background(), target(nil)); err != nil {
		t.Errorf("Validate() = %v, want nil: a simulation needs no access to anything", err)
	}
}

func TestValidateRejectsAColdstartThatIsNotANumber(t *testing.T) {
	err := simulation.New().Validate(context.Background(),
		target(map[string]string{"local_coldstart_seconds": "two minutes"}))

	if err == nil || !strings.Contains(err.Error(), "local_coldstart_seconds") {
		t.Errorf("Validate() = %v, want an error naming the bad field", err)
	}
}

func TestConcurrentRunsDoNotCorruptEachOther(t *testing.T) {
	a := simulation.New()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tgt := platform.Target{ID: string(rune('a' + i)), Kind: platform.KindSimulation}
			for cycle := 0; cycle < 20; cycle++ {
				when := start.Add(time.Duration(cycle) * time.Minute)
				if _, err := a.Apply(at(when), tgt, domain.Plan{LocalExecutors: cycle}); err != nil {
					t.Errorf("Apply() = %v", err)
					return
				}
				if _, err := a.Observe(at(when), tgt); err != nil {
					t.Errorf("Observe() = %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
