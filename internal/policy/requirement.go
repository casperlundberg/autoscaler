package policy

import (
	"math"
	"sort"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Requirement is how much capacity the workload needs, and how confident the
// engine is that the number is achievable.
type Requirement struct {
	// Minimum is the smallest executor count with no predicted breach.
	Minimum int

	// Executors is Minimum with the safety factor applied, clamped to the
	// combined tier ceilings. This is the number the plan is built from.
	Executors int

	// Feasible is false when no count within the ceilings avoids a breach —
	// an overload the operator's limits cannot absorb.
	Feasible bool

	// Projection is what is predicted under Executors, not under Minimum: the
	// operator is shown what the plan they are getting will actually do.
	Projection domain.Projection
}

// Required finds the smallest executor count that keeps every priority level
// inside its deadline, then adds the configured headroom.
//
// Smallest matters. Any count above the requirement also avoids a breach, so
// an engine that did not search for the minimum would happily hold far more
// capacity than the workload needs, every cycle, and the cloud tier is billed
// by the minute.
//
// The search is a binary search, which is sound because the model is monotone:
// adding an executor cannot reduce what any priority level is served, since
// each level takes what it needs and passes the remainder down. More capacity
// therefore never turns a satisfied level into a breaching one.
func Required(state domain.SystemState, settings config.Settings) Requirement {
	ceiling := settings.LocalExecutorCap + settings.CloudExecutorCap

	minimum, feasible := sort.Search(ceiling+1, func(n int) bool {
		return !Simulate(state, n, settings).BreachExpected
	}), true

	if minimum > ceiling {
		// Nothing within the operator's limits is enough. Run flat out rather
		// than giving up: a saturated system that is working is far better
		// than one that idles through an overload because no plan was perfect.
		minimum, feasible = ceiling, false
	}

	executors := minimum
	if feasible && settings.SafetyFactor > 1 {
		executors = int(math.Ceil(float64(minimum) * settings.SafetyFactor))
	}
	if executors > ceiling {
		executors = ceiling
	}

	return Requirement{
		Minimum:    minimum,
		Executors:  executors,
		Feasible:   feasible,
		Projection: Simulate(state, executors, settings),
	}
}
