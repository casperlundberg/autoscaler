package policy_test

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

// Work exempt from cloud burst is work an operator has decided may miss its
// deadline rather than be paid for. It still has to be served — by whatever
// capacity exists, in priority order — so it still takes that capacity from
// everything behind it. What it may not do is be the reason cloud capacity is
// bought.

// exemptSettings is a small local tier in front of a large cloud one, so
// whether the cloud was bought is never ambiguous.
func exemptSettings() config.Settings {
	s := noSafety(noColdstart(baseSettings()))
	s.Deadlines = map[domain.Priority]time.Duration{
		400: 30 * time.Second,
		100: 60 * time.Second,
		50:  5 * time.Minute,
	}
	s.LocalExecutorCap = 5
	s.CloudExecutorCap = 100
	s.MinLocalExecutors = 0
	return s
}

func withExempt(state domain.SystemState, exempt map[domain.Priority]domain.QueueInfo) domain.SystemState {
	state.BurstExempt = exempt
	return state
}

func TestExemptWorkThatWouldBreachDoesNotBuyCloud(t *testing.T) {
	settings := exemptSettings()
	heavy := map[domain.Priority]domain.QueueInfo{
		400: {Depth: 300, OldestJobAge: 10 * time.Second, ArrivalRate: 2},
	}

	counted := policy.Required(stateWith(1, heavy), settings)
	if counted.Executors <= settings.LocalExecutorCap {
		t.Fatalf("the same work, counted, needs only %d executors; the test needs it to need cloud",
			counted.Executors)
	}

	got := policy.Required(withExempt(stateWith(1, nil), heavy), settings)

	if got.Executors != settings.LocalExecutorCap {
		t.Errorf("Executors = %d, want the local cap of %d: local capacity is already paid for, "+
			"and exempt work may use all of it, but no more", got.Executors, settings.LocalExecutorCap)
	}
	if got.Withheld != counted.Executors-settings.LocalExecutorCap {
		t.Errorf("Withheld = %d, want the %d executors the work would have bought had it counted",
			got.Withheld, counted.Executors-settings.LocalExecutorCap)
	}
	if !got.Projection.BreachExpected || !got.Projection.BreachesExemptOnly {
		t.Errorf("Projection = %+v, want the breach predicted and marked as exempt work's alone",
			got.Projection)
	}
}

func TestExemptWorkThatFitsLocallyIsProvisionedForLikeAnyOther(t *testing.T) {
	settings := exemptSettings()
	settings.LocalExecutorCap = 50
	light := map[domain.Priority]domain.QueueInfo{
		100: {Depth: 120, OldestJobAge: 20 * time.Second, ArrivalRate: 1},
	}

	counted := policy.Required(stateWith(1, light), settings)
	got := policy.Required(withExempt(stateWith(1, nil), light), settings)

	if got.Executors != counted.Executors {
		t.Errorf("Executors = %d exempt against %d counted: exemption is about the cloud, and "+
			"work that fits on local capacity should get it either way", got.Executors, counted.Executors)
	}
	if got.Withheld != 0 {
		t.Errorf("Withheld = %d, want 0 when nothing was held back", got.Withheld)
	}
}

// Exempt work is served in priority order like everything else, so a flood of
// it at a high level starves counted work beneath it. The counted work's
// deadline is still a reason to buy capacity.
func TestCountedWorkStarvedByExemptWorkStillBuysCloud(t *testing.T) {
	settings := exemptSettings()
	state := withExempt(stateWith(1, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 40, OldestJobAge: 20 * time.Second, ArrivalRate: 1},
	}), map[domain.Priority]domain.QueueInfo{
		400: {Depth: 200, OldestJobAge: 5 * time.Second, ArrivalRate: 3},
	})

	got := policy.Required(state, settings)

	if got.Executors <= settings.LocalExecutorCap {
		t.Errorf("Executors = %d: the counted P100 work behind the exempt flood will breach on "+
			"local capacity alone, and that is a reason to burst", got.Executors)
	}
	onlyCounted := policy.Required(stateWith(1, state.Queues), settings)
	if got.Executors <= onlyCounted.Executors {
		t.Errorf("Executors = %d, no more than the %d the counted work needs on its own: the "+
			"exempt work ahead of it was treated as if it took no capacity",
			got.Executors, onlyCounted.Executors)
	}
}

// Without exemption, one job already past its deadline is a breach no count
// can avoid, and the engine runs flat out. That is right for work that must
// not be late and ruinous for work an operator has chosen to let be late.
func TestAnExemptJobAlreadyLateDoesNotSendEverythingToTheCeiling(t *testing.T) {
	settings := exemptSettings()
	late := map[domain.Priority]domain.QueueInfo{
		400: {Depth: 3, OldestJobAge: 5 * time.Minute},
	}

	counted := policy.Required(stateWith(1, late), settings)
	if counted.Outcome != policy.Overloaded {
		t.Fatalf("counted, a late job gives %v; the test assumes it is an overload", counted.Outcome)
	}

	got := policy.Required(withExempt(stateWith(1, nil), late), settings)

	if got.Executors > settings.LocalExecutorCap {
		t.Errorf("Executors = %d, want nothing past local for a late job that is exempt", got.Executors)
	}
	if got.Outcome == policy.Overloaded {
		t.Error("Outcome = overloaded: the only breach is exempt work's, and exempt work cannot overload the cloud")
	}
}

func TestAnEmptyExemptionDecidesExactlyAsNone(t *testing.T) {
	settings := exemptSettings()
	queues := map[domain.Priority]domain.QueueInfo{
		400: {Depth: 30, OldestJobAge: 12 * time.Second, ArrivalRate: 0.7},
		100: {Depth: 90, OldestJobAge: 45 * time.Second, ArrivalRate: 1.3},
	}
	plain := stateWith(1, queues)
	empty := withExempt(stateWith(1, queues), map[domain.Priority]domain.QueueInfo{
		400: {}, 100: {}, 25: {},
	})

	if a, b := policy.Required(plain, settings), policy.Required(empty, settings); a != b {
		t.Errorf("Required = %+v with no exemption and %+v with an empty one", a, b)
	}
	for n := 0; n <= 20; n++ {
		a := policy.Simulate(plain, policy.Instant(n), settings)
		b := policy.Simulate(empty, policy.Instant(n), settings)
		if a != b {
			t.Fatalf("at %d executors Simulate = %+v with no exemption and %+v with an empty one", n, a, b)
		}
	}
}

// Within a level the queue is first in, first out whichever kind a job is. So
// counted work that has waited longer than the exempt work beside it is served
// first, and does not breach for being in the same level as a backlog.
func TestWithinALevelTheOldestWorkIsServedFirstWhateverItsKind(t *testing.T) {
	settings := exemptSettings()
	state := withExempt(stateWith(0.5, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 5, OldestJobAge: 55 * time.Second},
	}), map[domain.Priority]domain.QueueInfo{
		100: {Depth: 100, OldestJobAge: 5 * time.Second},
	})

	got := policy.Simulate(state, policy.Instant(1), settings)

	if !got.BreachExpected {
		t.Fatalf("Projection = %+v: a hundred exempt jobs at one job every two seconds must breach", got)
	}
	if !got.BreachesExemptOnly {
		t.Errorf("Projection = %+v: the five counted jobs were older than every exempt one and "+
			"one executor clears them inside their deadline", got)
	}
}

// And the other way round: counted work that arrived after an exempt backlog
// waits behind it, and breaches with it.
func TestCountedWorkQueuedBehindAnExemptBacklogBreachesWithIt(t *testing.T) {
	settings := exemptSettings()
	state := withExempt(stateWith(0.5, map[domain.Priority]domain.QueueInfo{
		100: {Depth: 5, OldestJobAge: 5 * time.Second},
	}), map[domain.Priority]domain.QueueInfo{
		100: {Depth: 100, OldestJobAge: 55 * time.Second},
	})

	got := policy.Simulate(state, policy.Instant(1), settings)

	if !got.BreachExpected || got.BreachesExemptOnly {
		t.Errorf("Projection = %+v, want the counted work behind the backlog to breach too", got)
	}
}

func TestTheReasonSaysWhatExemptionHeldBack(t *testing.T) {
	settings := exemptSettings()
	state := withExempt(stateWith(1, nil), map[domain.Priority]domain.QueueInfo{
		400: {Depth: 300, OldestJobAge: 10 * time.Second, ArrivalRate: 2},
	})
	state.Capacity = domain.Capacity{LocalReady: 5}

	decision, _ := policy.Decide(state, domain.LoopState{}, settings)

	if decision.Plan.CloudExecutors != 0 {
		t.Errorf("Plan = %+v, want no cloud for exempt work", decision.Plan)
	}
	for _, want := range []string{"exempt from cloud burst", "300 of 300 waiting"} {
		if !strings.Contains(decision.Reason, want) {
			t.Errorf("Reason = %q, want it to mention %q", decision.Reason, want)
		}
	}
}

// The binary search in Required is sound only if each predicate it searches is
// monotone in the executor count, exempt work included.
func TestAvoidingACountedBreachIsMonotoneInTheExecutorCount(t *testing.T) {
	r := rand.New(rand.NewPCG(37, 53))

	for trial := 0; trial < 600; trial++ {
		settings := randomSettings(r)
		state := randomExemptState(r)
		if err := state.Validate(); err != nil {
			t.Fatalf("trial %d: generated an observation the engine refuses: %v", trial, err)
		}
		ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

		for _, predicate := range []struct {
			name     string
			breaches func(domain.Projection) bool
		}{
			{"any breach", func(p domain.Projection) bool { return p.BreachExpected }},
			{"counted breach", func(p domain.Projection) bool { return p.BreachExpected && !p.BreachesExemptOnly }},
		} {
			solved := -1
			for n := 0; n <= ceiling; n++ {
				available := policy.AvailabilityOf(policy.SplitTiers(n, settings), state.Capacity, settings)
				breaches := predicate.breaches(policy.Simulate(state, available, settings))
				switch {
				case !breaches && solved < 0:
					solved = n
				case breaches && solved >= 0:
					t.Fatalf("trial %d: %s: %d executors avoid it but %d do not\nsettings: %+v\nstate: %+v",
						trial, predicate.name, solved, n, settings, state)
				}
			}
		}
	}
}

// The promise, stated as a property: whatever exempt work is waiting, cloud is
// bought only when counted work would breach on local capacity alone.
func TestCloudIsBoughtOnlyWhenCountedWorkBreachesWithoutIt(t *testing.T) {
	r := rand.New(rand.NewPCG(41, 59))

	for trial := 0; trial < 600; trial++ {
		settings := randomSettings(r)
		// Headroom would put cloud on top of a local-sized requirement, which
		// is a separate and older decision than the one under test.
		settings.SafetyFactor = 1
		state := randomExemptState(r)

		got := policy.Required(state, settings)
		if got.Executors > settings.LocalExecutorCap+settings.CloudExecutorCap {
			t.Fatalf("trial %d: Executors %d over the ceiling", trial, got.Executors)
		}
		if policy.SplitTiers(got.Executors, settings).CloudExecutors == 0 {
			continue
		}
		local := policy.AvailabilityOf(policy.SplitTiers(settings.LocalExecutorCap, settings), state.Capacity, settings)
		if p := policy.Simulate(state, local, settings); !p.BreachExpected || p.BreachesExemptOnly {
			t.Fatalf("trial %d: bought cloud (%d executors) though counted work does not breach "+
				"on the local cap alone: %+v\nsettings: %+v\nstate: %+v",
				trial, got.Executors, p, settings, state)
		}
	}
}

func randomExemptState(r *rand.Rand) domain.SystemState {
	state := randomState(r)
	state.BurstExempt = map[domain.Priority]domain.QueueInfo{}
	for _, p := range []domain.Priority{400, 100, 50, 25, 0} {
		if r.IntN(3) == 0 {
			continue
		}
		state.BurstExempt[p] = domain.QueueInfo{
			Depth:        r.IntN(300),
			OldestJobAge: time.Duration(r.IntN(1800)) * time.Second,
			ArrivalRate:  r.Float64() * 3,
		}
	}
	return state
}
