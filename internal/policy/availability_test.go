package policy_test

import (
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

// coldSettings gives the two tiers distinguishable coldstarts, so a test can
// tell which one a piece of capacity was charged.
func coldSettings() config.Settings {
	s := baseSettings()
	s.LocalColdstart = 2 * time.Minute
	s.CloudColdstart = 5 * time.Minute
	s.LocalExecutorCap = 10
	s.CloudExecutorCap = 10
	s.MinLocalExecutors = 0
	return s
}

func TestCapacityThatIsAlreadyReadyServesFromTheFirstStep(t *testing.T) {
	capacity := domain.Capacity{LocalReady: 4}

	got := policy.AvailabilityOf(domain.Plan{LocalExecutors: 4}, capacity, coldSettings())

	if got.Now != 4 {
		t.Errorf("Now = %d, want 4: these executors are already working", got.Now)
	}
	if got.Starting() != 0 {
		t.Errorf("Starting() = %d, want 0", got.Starting())
	}
}

func TestANewLocalExecutorCannotWorkUntilItsColdstartHasElapsed(t *testing.T) {
	settings := coldSettings()

	got := policy.AvailabilityOf(domain.Plan{LocalExecutors: 3}, domain.Capacity{}, settings)

	if got.Serving(0) != 0 {
		t.Errorf("Serving(0) = %d, want 0: nothing has started yet", got.Serving(0))
	}
	if got.Serving(settings.LocalColdstart-time.Second) != 0 {
		t.Errorf("Serving(just under the coldstart) = %d, want 0",
			got.Serving(settings.LocalColdstart-time.Second))
	}
	if got.Serving(settings.LocalColdstart) != 3 {
		t.Errorf("Serving(the coldstart) = %d, want 3", got.Serving(settings.LocalColdstart))
	}
}

// The two tiers do not start at the same speed, and the difference is the
// reason the cloud tier is overflow rather than a first resort.
func TestEachTierIsChargedItsOwnColdstart(t *testing.T) {
	settings := coldSettings()
	plan := domain.Plan{LocalExecutors: 2, CloudExecutors: 3}

	got := policy.AvailabilityOf(plan, domain.Capacity{}, settings)

	if n := got.Serving(settings.LocalColdstart); n != 2 {
		t.Errorf("Serving(local coldstart) = %d, want only the 2 local executors", n)
	}
	if n := got.Serving(settings.CloudColdstart); n != 5 {
		t.Errorf("Serving(cloud coldstart) = %d, want all 5", n)
	}
	if got.Total() != 5 {
		t.Errorf("Total() = %d, want 5 regardless of when they arrive", got.Total())
	}
}

// Giving capacity back does not take a coldstart, so a plan below what is
// ready is fully available at once — and must not be modelled as if the
// executors it keeps had to start again.
func TestScalingDownIsImmediate(t *testing.T) {
	capacity := domain.Capacity{LocalReady: 8, CloudReady: 4}

	got := policy.AvailabilityOf(domain.Plan{LocalExecutors: 3, CloudExecutors: 1},
		capacity, coldSettings())

	if got.Now != 4 {
		t.Errorf("Now = %d, want 4: the retained executors were already running", got.Now)
	}
	if got.Starting() != 0 {
		t.Errorf("Starting() = %d, want 0", got.Starting())
	}
}

// An observation reports how many executors are pending, never how long they
// have been. So a pending executor is charged its whole coldstart again. That
// is pessimistic by up to one coldstart, and pessimistic is the side to be
// wrong on: it can only hold capacity that was not strictly needed, never miss
// a deadline that could have been met.
func TestAnExecutorAlreadyStartingIsNotCountedAsThroughputYet(t *testing.T) {
	settings := coldSettings()
	capacity := domain.Capacity{LocalReady: 1, LocalPending: 6}

	got := policy.AvailabilityOf(domain.Plan{LocalExecutors: 7}, capacity, settings)

	if got.Now != 1 {
		t.Errorf("Now = %d, want 1: six of the seven cannot take a job", got.Now)
	}
	if got.Serving(settings.LocalColdstart) != 7 {
		t.Errorf("Serving(coldstart) = %d, want all 7 by then",
			got.Serving(settings.LocalColdstart))
	}
}

func TestInstantCapacityServesThroughout(t *testing.T) {
	got := policy.Instant(6)

	if got.Serving(0) != 6 || got.Serving(time.Hour) != 6 {
		t.Errorf("Serving = %d then %d, want 6 throughout", got.Serving(0), got.Serving(time.Hour))
	}
	if got.Starting() != 0 {
		t.Errorf("Starting() = %d, want 0", got.Starting())
	}
}

func TestANegativeExecutorCountIsTreatedAsNone(t *testing.T) {
	if got := policy.Instant(-5); got.Total() != 0 {
		t.Errorf("Instant(-5).Total() = %d, want 0", got.Total())
	}
	got := policy.AvailabilityOf(domain.Plan{LocalExecutors: -3}, domain.Capacity{}, coldSettings())
	if got.Total() != 0 {
		t.Errorf("AvailabilityOf a negative plan totals %d, want 0", got.Total())
	}
}
