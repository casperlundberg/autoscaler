package policy

import (
	"fmt"
	"strings"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Decide produces the plan for one cycle.
//
// The observation's own timestamp is the clock. Not time.Now(): a replayed run
// moves through compressed time, and cooldowns measured against wall-clock
// time would evaporate — a five-minute cooldown would expire within one
// simulated hour that took two real seconds to play.
func Decide(state domain.SystemState, loop domain.LoopState, settings config.Settings) (domain.Decision, domain.LoopState) {
	now := state.Timestamp
	current := state.Capacity.AsPlan()

	if err := state.Validate(); err != nil {
		// Hold, do not guess. An observation this broken means the adapter is
		// wrong, and acting on it would turn a reporting bug into a capacity
		// incident.
		return domain.Decision{
			At:          now,
			Previous:    current,
			Plan:        current,
			Action:      domain.ActionMaintain,
			Reason:      "observation rejected, holding current capacity: " + err.Error(),
			Constrained: true,
			Constraint:  "invalid observation",
		}, loop
	}

	requirement := Required(state, settings)
	desired := SplitTiers(requirement.Executors, settings)

	plan, constraint := applyHysteresis(now, current, desired, loop, settings)
	plan = clampToLimits(plan, settings)

	return domain.Decision{
		At:          now,
		Previous:    current,
		Plan:        plan,
		Action:      domain.ClassifyAction(current, plan),
		Reason:      explain(state, current, requirement, settings),
		Projection:  requirement.Projection,
		Constrained: constraint != "",
		Constraint:  constraint,
	}, advance(now, current, plan, loop)
}

// applyHysteresis is what stops the engine from thrashing. Left alone, a
// controller that recomputes from scratch every fifteen seconds will chase
// noise in the arrival rate, and each round trip costs a coldstart.
func applyHysteresis(now time.Time, current, desired domain.Plan, loop domain.LoopState,
	settings config.Settings) (domain.Plan, string) {
	var constraints []string
	plan := desired

	// Cloud capacity is billed from the moment it is requested. Handing it
	// back during a lull and buying it again minutes later is the most
	// expensive possible way to ride out a burst.
	if plan.CloudExecutors < current.CloudExecutors && !loop.CloudSince.IsZero() &&
		now.Sub(loop.CloudSince) < settings.CloudMinLifetime {
		plan.CloudExecutors = current.CloudExecutors
		constraints = append(constraints, "cloud minimum lifetime")
	}

	// Cooldowns are directional: having just scaled up does not stop a
	// scale-down, and the reverse. A zero timestamp means it has never
	// happened, which must not read as "it happened at the epoch".
	switch {
	case plan.Total() > current.Total():
		if !loop.LastScaleUp.IsZero() && now.Sub(loop.LastScaleUp) < settings.ScaleUpCooldown {
			return current, "scale-up cooldown"
		}
	case plan.Total() < current.Total():
		if !loop.LastScaleDown.IsZero() && now.Sub(loop.LastScaleDown) < settings.ScaleDownCooldown {
			return current, "scale-down cooldown"
		}
	}

	local, localLimited := limitStep(current.LocalExecutors, plan.LocalExecutors, settings)
	cloud, cloudLimited := limitStep(current.CloudExecutors, plan.CloudExecutors, settings)
	plan = domain.Plan{LocalExecutors: local, CloudExecutors: cloud}
	if localLimited || cloudLimited {
		constraints = append(constraints, "per-cycle step limit")
	}

	return plan, strings.Join(constraints, "; ")
}

// limitStep caps how far one tier may move in a single cycle, and reports
// whether it had to.
func limitStep(current, target int, settings config.Settings) (int, bool) {
	switch {
	case target > current+settings.MaxScaleUpStep:
		return current + settings.MaxScaleUpStep, true
	case target < current-settings.MaxScaleDownStep:
		return current - settings.MaxScaleDownStep, true
	default:
		return target, false
	}
}

// clampToLimits is the last word before a plan leaves the engine. Step limits
// and cooldowns work from the current capacity, which may itself be outside
// the configured bounds — an operator lowering the local cap while forty
// executors are running, for instance — so the bounds are re-applied here
// rather than assumed to hold.
func clampToLimits(plan domain.Plan, settings config.Settings) domain.Plan {
	if plan.LocalExecutors > settings.LocalExecutorCap {
		plan.LocalExecutors = settings.LocalExecutorCap
	}
	if plan.LocalExecutors < settings.MinLocalExecutors {
		plan.LocalExecutors = settings.MinLocalExecutors
	}
	if plan.LocalExecutors < 0 {
		plan.LocalExecutors = 0
	}
	if plan.CloudExecutors > settings.CloudExecutorCap {
		plan.CloudExecutors = settings.CloudExecutorCap
	}
	if plan.CloudExecutors < 0 {
		plan.CloudExecutors = 0
	}
	return plan
}

// advance rolls the loop memory forward.
//
// A cycle that changed nothing must not restart the cooldown clocks: if it
// did, a system sitting at its correct capacity would keep pushing its own
// cooldown forward and the next real scale-down could never fire.
func advance(now time.Time, current, plan domain.Plan, loop domain.LoopState) domain.LoopState {
	next := loop

	if plan != current {
		switch {
		case plan.Total() > current.Total():
			next.LastScaleUp = now
		case plan.Total() < current.Total():
			next.LastScaleDown = now
		}
		if plan.CloudExecutors > current.CloudExecutors {
			next.CloudSince = now
		}
	}

	// No cloud capacity means no lifetime to protect. Clearing this
	// unconditionally also sweeps up a stale timestamp left behind when the
	// tier was emptied by something other than a decision.
	if plan.CloudExecutors == 0 {
		next.CloudSince = time.Time{}
	}
	return next
}

// explain says why the decision came out as it did, in terms an operator
// reading a run log can check against the numbers they can see.
func explain(state domain.SystemState, current domain.Plan, requirement Requirement,
	settings config.Settings) string {
	waiting := state.TotalDepth()
	arriving := state.TotalArrivalRate()

	switch requirement.Outcome {
	case Overloaded:
		return fmt.Sprintf(
			"overload: %d jobs waiting, %.2f/s arriving, and even %d executors "+
				"(both caps combined) serving from now leave P%d breaching in %s; "+
				"running at the ceiling",
			waiting, arriving, requirement.Executors,
			requirement.Projection.FirstBreachPriority,
			requirement.Projection.FirstBreachIn.Round(time.Second))

	case Unavoidable:
		// Worth saying explicitly, because the operator can act on it and on
		// nothing else here: this breach is not a provisioning mistake, and no
		// number of executors would have dodged it. What shortens it next time
		// is a warmer floor or a faster image, not a bigger cap.
		return fmt.Sprintf(
			"P%d breaches in %s and no executor count avoids it — nothing can "+
				"start inside the deadline (local %s, cloud %s coldstart); asking "+
				"for the %d the queue needs once capacity arrives (minimum %d plus "+
				"%.2fx safety) for %d jobs waiting, %.2f/s arriving",
			requirement.Projection.FirstBreachPriority,
			requirement.Projection.FirstBreachIn.Round(time.Second),
			settings.LocalColdstart, settings.CloudColdstart,
			requirement.Executors, requirement.Minimum,
			settings.SafetyFactor, waiting, arriving)
	}

	atCurrent := Simulate(state, AvailabilityOf(current, state.Capacity, settings), settings)
	if atCurrent.BreachExpected {
		return fmt.Sprintf(
			"P%d breaches in %s at the current %d executors%s; %d needed "+
				"(minimum %d plus %.2fx safety) for %d jobs waiting, %.2f/s arriving",
			atCurrent.FirstBreachPriority, atCurrent.FirstBreachIn.Round(time.Second),
			current.Total(), starting(state.Capacity), requirement.Executors,
			requirement.Minimum, settings.SafetyFactor, waiting, arriving)
	}
	if requirement.Executors < current.Total() {
		return fmt.Sprintf(
			"no breach predicted; %d executors%s is above the requirement of %d "+
				"for %d jobs waiting, %.2f/s arriving",
			current.Total(), starting(state.Capacity), requirement.Executors, waiting, arriving)
	}
	return fmt.Sprintf(
		"no breach predicted at %d executors%s; requirement is %d for %d jobs waiting, %.2f/s arriving",
		current.Total(), starting(state.Capacity), requirement.Executors, waiting, arriving)
}

// starting annotates an executor count with how much of it cannot work yet.
//
// Without this, a reasoning string reads the same whether all ten executors
// are serving or seven are still pulling an image — and those are different
// claims about the same number. The projection already accounts for the
// difference; the sentence explaining it should say so too.
func starting(capacity domain.Capacity) string {
	if pending := capacity.TotalPending(); pending > 0 {
		return fmt.Sprintf(" (%d still starting)", pending)
	}
	return ""
}
