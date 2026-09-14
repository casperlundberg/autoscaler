package policy_test

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

// Property tests for the decision engine.
//
// These assert what must hold for every observation rather than for one worked
// example. They are here because the engine's most load-bearing claim was, for
// a long time, only argued in a comment: Required finds its answer with a
// binary search, which is sound only if the predicate it searches is monotone
// in the executor count. The argument was right, but nothing would have said
// so if a later change had broken it — sort.Search on a non-monotone predicate
// does not fail, it quietly returns a count that is not minimal, or not a
// solution at all.
//
// The generators stay inside what Validate accepts, because an observation the
// engine would refuse tells us nothing about the engine.

// randomSettings produces a coherent settings document with the knobs that
// affect the search varying, including both coldstarts.
func randomSettings(r *rand.Rand) config.Settings {
	s := config.DefaultSettings()
	s.Horizon = time.Duration(2+r.IntN(20)) * time.Minute
	s.SimulationStep = time.Duration(5+r.IntN(25)) * time.Second
	s.Deadlines = map[domain.Priority]time.Duration{
		400: time.Duration(10+r.IntN(60)) * time.Second,
		100: time.Duration(30+r.IntN(300)) * time.Second,
		50:  time.Duration(1+r.IntN(20)) * time.Minute,
	}
	s.DefaultDeadline = time.Duration(1+r.IntN(24)) * time.Hour
	s.LocalExecutorCap = r.IntN(30)
	s.CloudExecutorCap = r.IntN(30)
	s.MinLocalExecutors = r.IntN(1 + s.LocalExecutorCap)
	s.LocalColdstart = time.Duration(r.IntN(300)) * time.Second
	s.CloudColdstart = time.Duration(r.IntN(600)) * time.Second
	s.SafetyFactor = 1 + r.Float64()
	return s
}

// randomState produces an observation the engine will accept, with capacity
// spread across ready and pending so the ramp is exercised.
func randomState(r *rand.Rand) domain.SystemState {
	queues := map[domain.Priority]domain.QueueInfo{}
	for _, p := range []domain.Priority{400, 100, 50, 25} {
		if r.IntN(4) == 0 {
			continue
		}
		queues[p] = domain.QueueInfo{
			Depth:        r.IntN(500),
			OldestJobAge: time.Duration(r.IntN(1800)) * time.Second,
			ArrivalRate:  r.Float64() * 5,
		}
	}
	return domain.SystemState{
		Timestamp: noon,
		Queues:    queues,
		Capacity: domain.Capacity{
			LocalReady:   r.IntN(12),
			CloudReady:   r.IntN(12),
			LocalPending: r.IntN(12),
			CloudPending: r.IntN(12),
		},
		ExecutorThroughput: 0.05 + r.Float64()*3,
	}
}

// The precondition for the binary search in Required, stated directly: once
// some executor count avoids the breach, every larger count must too. A single
// counterexample makes sort.Search's answer meaningless.
func TestAvoidingABreachIsMonotoneInTheExecutorCount(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 29))

	for trial := 0; trial < 600; trial++ {
		settings := randomSettings(r)
		state := randomState(r)
		if err := state.Validate(); err != nil {
			t.Fatalf("trial %d: generated an observation the engine refuses: %v", trial, err)
		}
		ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

		solved := -1
		for n := 0; n <= ceiling; n++ {
			available := policy.AvailabilityOf(policy.SplitTiers(n, settings), state.Capacity, settings)
			breaches := policy.Simulate(state, available, settings).BreachExpected

			switch {
			case !breaches && solved < 0:
				solved = n
			case breaches && solved >= 0:
				t.Fatalf("trial %d: %d executors avoid the breach but %d do not — "+
					"the predicate is not monotone, so the binary search in Required "+
					"is unsound\nsettings: %+v\nstate: %+v", trial, solved, n, settings, state)
			}
		}
	}
}

// The middle step of that argument on its own, so a break is localised: more
// plan is at-least-as-much capacity, at every point in the horizon.
func TestMorePlanIsNeverLessCapacityAtAnyPointInTheHorizon(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 31))

	for trial := 0; trial < 600; trial++ {
		settings := randomSettings(r)
		capacity := randomState(r).Capacity
		ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

		for n := 1; n <= ceiling; n++ {
			lower := policy.AvailabilityOf(policy.SplitTiers(n-1, settings), capacity, settings)
			higher := policy.AvailabilityOf(policy.SplitTiers(n, settings), capacity, settings)

			for elapsed := time.Duration(0); elapsed <= settings.Horizon; elapsed += settings.SimulationStep {
				if higher.Serving(elapsed) < lower.Serving(elapsed) {
					t.Fatalf("trial %d: at %s, %d executors serve %d but %d serve %d",
						trial, elapsed, n, higher.Serving(elapsed), n-1, lower.Serving(elapsed))
				}
			}
		}
	}
}

// And the first step: the tier split never moves a tier backwards as the total
// grows. Local filling before cloud is what makes the cloud tier overflow
// rather than a first resort, and it is also what keeps the ramp monotone,
// since the two tiers do not start at the same speed.
func TestTheTierSplitNeverMovesATierBackwards(t *testing.T) {
	r := rand.New(rand.NewPCG(17, 37))

	for trial := 0; trial < 400; trial++ {
		settings := randomSettings(r)
		ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

		for n := 1; n <= ceiling; n++ {
			lower := policy.SplitTiers(n-1, settings)
			higher := policy.SplitTiers(n, settings)

			if higher.LocalExecutors < lower.LocalExecutors {
				t.Fatalf("trial %d: local went %d -> %d as the total rose %d -> %d",
					trial, lower.LocalExecutors, higher.LocalExecutors, n-1, n)
			}
			if higher.CloudExecutors < lower.CloudExecutors {
				t.Fatalf("trial %d: cloud went %d -> %d as the total rose %d -> %d",
					trial, lower.CloudExecutors, higher.CloudExecutors, n-1, n)
			}
			if higher.LocalExecutors > settings.LocalExecutorCap {
				t.Fatalf("trial %d: local %d over its cap of %d",
					trial, higher.LocalExecutors, settings.LocalExecutorCap)
			}
			if higher.CloudExecutors > settings.CloudExecutorCap {
				t.Fatalf("trial %d: cloud %d over its cap of %d",
					trial, higher.CloudExecutors, settings.CloudExecutorCap)
			}
		}
	}
}

// Modelling the ramp can only ever make the engine more careful. If capacity
// serving from the first instant would still breach, capacity that arrives
// late certainly does — so the old instant model was optimistic everywhere,
// never pessimistic anywhere, which is exactly the wrong direction for an SLA.
func TestARampIsNeverMoreOptimisticThanInstantCapacity(t *testing.T) {
	r := rand.New(rand.NewPCG(19, 41))

	for trial := 0; trial < 600; trial++ {
		settings := randomSettings(r)
		state := randomState(r)
		ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap
		if ceiling == 0 {
			continue
		}
		n := r.IntN(ceiling + 1)

		plan := policy.SplitTiers(n, settings)
		ramped := policy.Simulate(state, policy.AvailabilityOf(plan, state.Capacity, settings), settings)
		instant := policy.Simulate(state, policy.Instant(plan.Total()), settings)

		if instant.BreachExpected && !ramped.BreachExpected {
			t.Fatalf("trial %d: %d executors breach when serving from now but not "+
				"when they have to start first\nsettings: %+v\nstate: %+v",
				trial, plan.Total(), settings, state)
		}
		if ramped.BreachExpected && instant.BreachExpected &&
			ramped.FirstBreachIn > instant.FirstBreachIn {
			t.Fatalf("trial %d: the ramp puts the first breach at %s, later than "+
				"instant capacity's %s", trial, ramped.FirstBreachIn, instant.FirstBreachIn)
		}
	}
}

// What Required promises, checked against what it returns, for every
// observation rather than the one in the example test.
func TestTheRequirementIsAlwaysASolutionAndAlwaysMinimal(t *testing.T) {
	r := rand.New(rand.NewPCG(23, 43))

	for trial := 0; trial < 600; trial++ {
		settings := randomSettings(r)
		state := randomState(r)
		ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

		got := policy.Required(state, settings)

		if got.Executors > ceiling || got.Minimum > ceiling {
			t.Fatalf("trial %d: Minimum %d / Executors %d over the ceiling of %d",
				trial, got.Minimum, got.Executors, ceiling)
		}
		if got.Executors < got.Minimum {
			t.Fatalf("trial %d: Executors %d below Minimum %d — headroom cannot be negative",
				trial, got.Executors, got.Minimum)
		}

		breaches := func(n int) bool {
			available := policy.AvailabilityOf(policy.SplitTiers(n, settings), state.Capacity, settings)
			return policy.Simulate(state, available, settings).BreachExpected
		}

		switch got.Outcome {
		case policy.Achievable:
			if breaches(got.Minimum) {
				t.Fatalf("trial %d: Achievable, but Minimum = %d still breaches", trial, got.Minimum)
			}
			if got.Minimum > 0 && !breaches(got.Minimum-1) {
				t.Fatalf("trial %d: Minimum = %d, but %d also avoids the breach",
					trial, got.Minimum, got.Minimum-1)
			}
			if got.Projection.BreachExpected {
				t.Fatalf("trial %d: Achievable, but the projection under the chosen "+
					"%d executors breaches", trial, got.Executors)
			}

		case policy.Unavoidable:
			// No count avoids it — that is what unavoidable means, and it is
			// the claim most worth checking, because getting it wrong sends
			// the engine to the ceiling for nothing.
			for n := 0; n <= ceiling; n++ {
				if !breaches(n) {
					t.Fatalf("trial %d: Unavoidable, but %d executors avoid the breach", trial, n)
				}
			}
			// It must still be serviceable once capacity has arrived,
			// otherwise it is an overload and belongs in the other branch.
			if policy.Simulate(state, policy.Instant(policy.SplitTiers(got.Minimum, settings).Total()),
				settings).BreachExpected {
				t.Fatalf("trial %d: Unavoidable with Minimum = %d, which does not hold "+
					"the queue even serving from now — that is an overload", trial, got.Minimum)
			}

		case policy.Overloaded:
			if got.Minimum != ceiling || got.Executors != ceiling {
				t.Fatalf("trial %d: Overloaded should run flat out, got Minimum %d / "+
					"Executors %d against a ceiling of %d", trial, got.Minimum, got.Executors, ceiling)
			}
		}
	}
}

// Decide is a pure function of its inputs. A replayed run and the live run it
// explains have to reach the same decision, or a run proves nothing about the
// controller it was meant to be evidence about.
func TestDecideIsAPureFunctionOfItsInputs(t *testing.T) {
	r := rand.New(rand.NewPCG(29, 47))

	for trial := 0; trial < 400; trial++ {
		settings := randomSettings(r)
		state := randomState(r)
		loop := domain.LoopState{
			LastScaleUp:   noon.Add(-time.Duration(r.IntN(600)) * time.Second),
			LastScaleDown: noon.Add(-time.Duration(r.IntN(600)) * time.Second),
			CloudSince:    noon.Add(-time.Duration(r.IntN(600)) * time.Second),
		}

		first, firstLoop := policy.Decide(state, loop, settings)
		for again := 0; again < 3; again++ {
			got, gotLoop := policy.Decide(state, loop, settings)
			if got != first || gotLoop != firstLoop {
				t.Fatalf("trial %d: Decide returned %+v then %+v for one input", trial, first, got)
			}
		}

		// And whatever it decided has to be inside the operator's limits.
		if first.Plan.LocalExecutors > settings.LocalExecutorCap {
			t.Fatalf("trial %d: planned %d local against a cap of %d",
				trial, first.Plan.LocalExecutors, settings.LocalExecutorCap)
		}
		if first.Plan.CloudExecutors > settings.CloudExecutorCap {
			t.Fatalf("trial %d: planned %d cloud against a cap of %d",
				trial, first.Plan.CloudExecutors, settings.CloudExecutorCap)
		}
		if first.Reason == "" {
			t.Fatalf("trial %d: a decision with no reasoning", trial)
		}
	}
}
