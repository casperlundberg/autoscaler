package policy

import (
	"math"
	"sort"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Outcome says which of three situations a requirement came out of.
//
// They were one boolean until coldstart reached the decision engine, and that
// conflation stopped being harmless the moment it did. Once capacity is
// modelled as arriving late, "no executor count avoids this breach" becomes
// the ordinary state of a cold system facing a tight deadline — and the old
// response to it, run flat out at both caps, would spend the entire cloud
// budget every time a quiet pool woke up. The two cases have to be told apart
// because they call for opposite actions.
type Outcome int

const (
	// Achievable: some count within the caps avoids every predicted breach.
	Achievable Outcome = iota

	// Unavoidable: no count avoids the breach, because the capacity that would
	// have to serve it cannot start in time. More executors do not help — the
	// breach is already committed — so the requirement is what the queue needs
	// once capacity has arrived, and not the ceiling.
	Unavoidable

	// Overloaded: arrivals outrun even both caps combined serving from the
	// first instant. Coldstart is not the binding constraint; capacity is.
	// Run flat out.
	Overloaded
)

func (o Outcome) String() string {
	switch o {
	case Achievable:
		return "achievable"
	case Unavoidable:
		return "unavoidable"
	case Overloaded:
		return "overloaded"
	}
	return "unknown"
}

// Requirement is how much capacity the workload needs, and what kind of
// situation that number came out of.
type Requirement struct {
	// Minimum is the smallest executor count that solves the problem the
	// Outcome describes: no predicted breach when Achievable, and the
	// steady-state need once capacity has started when Unavoidable.
	Minimum int

	// Executors is Minimum with the safety factor applied, clamped to the
	// combined tier ceilings. This is the number the plan is built from.
	Executors int

	// Outcome distinguishes a requirement that avoids the breach from one that
	// cannot, and that from a genuine overload.
	Outcome Outcome

	// Projection is what is predicted under Executors, not under Minimum, and
	// under the ramp those executors actually arrive on: the operator is shown
	// what the plan they are getting will really do, including the breach it
	// cannot dodge.
	Projection domain.Projection

	// Withheld is how many more executors the work would have asked for had
	// none of it been exempt from cloud burst. When it is above zero, Minimum
	// and Outcome describe the counted work alone, and Executors is what that
	// needs or the local cap, whichever is larger.
	Withheld int
}

// Feasible reports whether the requirement avoids every predicted breach.
func (r Requirement) Feasible() bool { return r.Outcome == Achievable }

// Required finds the smallest executor count that keeps every priority level
// inside its deadline, then adds the configured headroom.
//
// Smallest matters. Any count above the requirement also avoids a breach, so
// an engine that did not search for the minimum would happily hold far more
// capacity than the workload needs, every cycle, and the cloud tier is billed
// by the minute.
//
// The search is a binary search, which is sound because the predicate is
// monotone in the executor count. Three steps carry that, and property tests
// hold each of them: SplitTiers is non-decreasing per tier in its argument;
// AvailabilityOf turns more plan into at-least-as-much capacity at every point
// in the horizon; and Simulate serves each level what it needs before passing
// the remainder down, so more capacity at every point can never turn a
// satisfied level into a breaching one.
//
// Work exempt from cloud burst is sized for twice. With every breach counted,
// the answer says how much local capacity the work can use — local capacity
// is already paid for, and exempt work may have all of it. With only counted
// breaches, the answer says how much capacity the work justifies buying. The
// requirement is the larger of the local share of the first and the whole of
// the second, so cloud is only ever overflow of counted work: exempt work
// still takes capacity from the counted work behind it, and that work's
// deadline stays a reason to burst.
func Required(state domain.SystemState, settings config.Settings) Requirement {
	everything := required(state, settings, func(p domain.Projection) bool { return p.BreachExpected })
	if !state.HasBurstExempt() {
		return everything
	}
	counted := required(state, settings, func(p domain.Projection) bool {
		return p.BreachExpected && !p.BreachesExemptOnly
	})

	local := min(everything.Executors, settings.LocalExecutorCap)
	executors := max(local, counted.Executors)
	switch {
	case executors == everything.Executors:
		return everything
	case executors > everything.Executors:
		// Possible because the two searches can end in different outcomes: a
		// breach of exempt work no count avoids asks only for the steady
		// state, while counted work beside it can justify more to stay on
		// time. The counted work's answer stands.
		return counted
	}

	return Requirement{
		Minimum:    counted.Minimum,
		Executors:  executors,
		Outcome:    counted.Outcome,
		Projection: Simulate(state, AvailabilityOf(SplitTiers(executors, settings), state.Capacity, settings), settings),
		Withheld:   everything.Executors - executors,
	}
}

// required is the search behind Required, for one definition of a breach
// worth provisioning against.
func required(state domain.SystemState, settings config.Settings,
	breach func(domain.Projection) bool) Requirement {
	ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

	// What a count of n actually provides, once the plan is split across tiers
	// and each tier's coldstart is charged against it.
	ramp := func(n int) Availability {
		return AvailabilityOf(SplitTiers(n, settings), state.Capacity, settings)
	}
	breaches := func(available Availability) bool {
		return breach(Simulate(state, available, settings))
	}

	minimum := sort.Search(ceiling+1, func(n int) bool { return !breaches(ramp(n)) })
	outcome := Achievable

	if minimum > ceiling {
		// Nothing within the caps avoids the breach. Before running flat out,
		// ask which constraint is actually binding — capacity, or the time
		// capacity takes to start. Instant is the one place in the engine that
		// deliberately ignores the ramp, and this is what it is for: it
		// separates "more executors would not help" from "no executors can
		// arrive in time".
		steady := sort.Search(ceiling+1, func(n int) bool {
			return !breaches(Instant(SplitTiers(n, settings).Total()))
		})
		if steady <= ceiling {
			// The queue is serviceable; only the coldstart is in the way. Ask
			// for what the work needs, not for everything available.
			minimum, outcome = steady, Unavoidable
		} else {
			minimum, outcome = ceiling, Overloaded
		}
	}

	executors := minimum
	// Headroom covers the error in the throughput estimate, so it is worth
	// having whenever the number it multiplies is a real requirement. At the
	// ceiling there is nothing left to add.
	if outcome != Overloaded && settings.SafetyFactor > 1 {
		executors = int(math.Ceil(float64(minimum) * settings.SafetyFactor))
	}
	if executors > ceiling {
		executors = ceiling
	}

	return Requirement{
		Minimum:    minimum,
		Executors:  executors,
		Outcome:    outcome,
		Projection: Simulate(state, ramp(executors), settings),
	}
}
