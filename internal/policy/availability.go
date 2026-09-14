package policy

import (
	"sort"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Availability is how much capacity a plan actually provides across the
// horizon, as opposed to how much of it was asked for.
//
// The distinction is the whole reason coldstart is a setting. An executor that
// has been requested is not an executor that is working: it has to be
// scheduled, pulled, started and registered first, and on the cloud tier that
// is minutes. Simulating a candidate plan as though all of it served from the
// first step answers a question nobody asked — what count would have avoided
// this breach if capacity were instant — which is never the situation a
// scale-up decision is taken in.
type Availability struct {
	// Now is the executors that can take a job at elapsed zero.
	Now int

	// Later is capacity that joins during the horizon, ascending by when.
	Later []Arrival
}

// Arrival is capacity becoming able to work, After this long from the start of
// the horizon.
type Arrival struct {
	After     time.Duration
	Executors int
}

// Serving is how many executors can do work this far into the horizon.
func (a Availability) Serving(elapsed time.Duration) int {
	serving := a.Now
	for _, arrival := range a.Later {
		// Later is sorted, so the first arrival still in the future ends it.
		if arrival.After > elapsed {
			break
		}
		serving += arrival.Executors
	}
	return serving
}

// Total is every executor the plan holds, whether it can work yet or not.
func (a Availability) Total() int {
	total := a.Now
	for _, arrival := range a.Later {
		total += arrival.Executors
	}
	return total
}

// Starting is the capacity that has been asked for but cannot work yet.
func (a Availability) Starting() int { return a.Total() - a.Now }

// Instant is capacity that is all working from the first step.
//
// It is what a question about queue mechanics alone wants — a test of how the
// queue drains, where the ramp would only be noise. It is deliberately not
// what a decision uses, and Required reaches for it in exactly one place, for
// a reason stated there.
func Instant(executors int) Availability {
	if executors < 0 {
		executors = 0
	}
	return Availability{Now: executors}
}

// AvailabilityOf works out when a candidate plan's capacity can actually serve,
// given what the platform is running right now.
//
// An executor the plan keeps that is already ready serves immediately.
// Everything else is counted as arriving a full coldstart from now — both a
// brand-new request and an executor that was requested on an earlier cycle and
// is still starting.
//
// Charging an already-pending executor its whole coldstart a second time is
// deliberately pessimistic. An adapter reports how many executors are pending,
// not how long they have been pending, so the remaining wait is not knowable
// from an observation. The error is bounded by the coldstart, and it is in the
// safe direction: it can lead the engine to hold capacity it did not strictly
// need, never to miss a deadline it could have met. Reporting pending ages
// from the adapters would remove it, and is the honest way to — it would mean
// a new field on Capacity and real work in all four adapters.
func AvailabilityOf(plan domain.Plan, capacity domain.Capacity, settings config.Settings) Availability {
	local := max(plan.LocalExecutors, 0)
	cloud := max(plan.CloudExecutors, 0)

	// A plan below what is ready keeps only ready executors; the surplus is
	// what is being given back, and giving capacity back is instant.
	localNow := min(local, max(capacity.LocalReady, 0))
	cloudNow := min(cloud, max(capacity.CloudReady, 0))

	available := Availability{Now: localNow + cloudNow}

	for _, arrival := range []Arrival{
		{After: max(settings.LocalColdstart, 0), Executors: local - localNow},
		{After: max(settings.CloudColdstart, 0), Executors: cloud - cloudNow},
	} {
		if arrival.Executors > 0 {
			available.Later = append(available.Later, arrival)
		}
	}

	// Ascending, so Serving can stop at the first arrival past the point it is
	// asked about. Stable so that two tiers sharing a coldstart — which is
	// what a zero coldstart on both looks like — keep local before cloud.
	sort.SliceStable(available.Later, func(i, j int) bool {
		return available.Later[i].After < available.Later[j].After
	})

	return available
}
