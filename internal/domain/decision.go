package domain

import (
	"encoding/json"
	"time"
)

// Plan is the capacity the decision engine wants to exist: an absolute count
// per tier, not a delta.
//
// Absolute on purpose. A delta ("add three") is only meaningful relative to a
// reading that may already be stale by the time it is applied, and two deltas
// racing produce a count nobody asked for. An absolute plan is idempotent:
// applying it twice is the same as applying it once, which is exactly the
// property a control loop that retries needs.
type Plan struct {
	LocalExecutors int `json:"local_executors"`
	CloudExecutors int `json:"cloud_executors"`
}

// Total is the executor count across both tiers.
func (p Plan) Total() int { return p.LocalExecutors + p.CloudExecutors }

// Action names what a plan change did, for logs and for the UI. It is derived
// from the two plans, never set by hand, so it cannot drift from what actually
// happened.
type Action string

const (
	// ActionMaintain means the plan did not move.
	ActionMaintain Action = "maintain"
	// ActionScaleUp means local grew with cloud untouched.
	ActionScaleUp Action = "scale_up"
	// ActionScaleDown means local shrank with cloud untouched.
	ActionScaleDown Action = "scale_down"
	// ActionCloudBurst means cloud capacity was added.
	ActionCloudBurst Action = "cloud_burst"
	// ActionCloudRelease means cloud capacity was given back.
	ActionCloudRelease Action = "cloud_release"
	// ActionRebalance means the tiers moved in opposite directions — work
	// shifting between on-prem and cloud rather than total capacity changing.
	ActionRebalance Action = "rebalance"
)

// ClassifyAction names the move from prev to next.
//
// Precedence is deliberate. Opposite-direction moves are a rebalance and get
// their own name, because calling them a scale-up or a teardown would make the
// decision log misdescribe the most interesting cycles of a run. Otherwise a
// cloud change wins over a local one: cloud is the tier that costs money and
// takes minutes to arrive, so it is the event worth naming.
func ClassifyAction(prev, next Plan) Action {
	localDelta := next.LocalExecutors - prev.LocalExecutors
	cloudDelta := next.CloudExecutors - prev.CloudExecutors

	switch {
	case localDelta == 0 && cloudDelta == 0:
		return ActionMaintain
	case localDelta > 0 && cloudDelta < 0, localDelta < 0 && cloudDelta > 0:
		return ActionRebalance
	case cloudDelta > 0:
		return ActionCloudBurst
	case cloudDelta < 0:
		return ActionCloudRelease
	case localDelta > 0:
		return ActionScaleUp
	default:
		return ActionScaleDown
	}
}

// Projection is what the forward simulation predicted for the plan that was
// chosen. It is carried on the decision so a run can be read back and the
// reasoning checked, rather than only the outcome.
type Projection struct {
	// BreachExpected is whether any priority level was predicted to pass its
	// deadline within the horizon under the chosen plan.
	BreachExpected bool `json:"breach_expected"`

	// FirstBreachPriority and FirstBreachIn describe the earliest predicted
	// breach. Meaningful only when BreachExpected is true.
	FirstBreachPriority Priority      `json:"first_breach_priority,omitempty"`
	FirstBreachIn       time.Duration `json:"first_breach_in,omitempty"`

	// PeakQueueDepth is the deepest the total queue got during the horizon.
	PeakQueueDepth int `json:"peak_queue_depth"`

	// DrainedAt is how long the queue took to clear, or 0 if it did not clear
	// within the horizon.
	DrainedAt time.Duration `json:"drained_at,omitempty"`
}

// Decision is one cycle's output: the plan, what it changed, and why.
type Decision struct {
	At time.Time `json:"at"`

	// Previous is the capacity the platform had already been asked for;
	// Plan is what it should be asked for now.
	Previous Plan `json:"previous"`
	Plan     Plan `json:"plan"`

	Action Action `json:"action"`

	// Reason is a human-readable explanation. It is shown verbatim in the UI
	// and in the run log, so it names the rule that fired and the numbers that
	// made it fire.
	Reason string `json:"reason"`

	// Projection is the forward simulation behind this plan.
	Projection Projection `json:"projection"`

	// SettingsVersion is the settings document this decision was made under.
	// Settings change while the service runs, so without this a decision
	// cannot be explained after the fact.
	SettingsVersion int64 `json:"settings_version"`

	// Constrained records that a limit — the local cap, the cloud ceiling, a
	// per-cycle step limit, cooldown — held the plan back from what the
	// projection actually asked for.
	Constrained bool   `json:"constrained"`
	Constraint  string `json:"constraint,omitempty"`
}

// Delta is the per-tier change this decision represents.
func (d Decision) Delta() (local, cloud int) {
	return d.Plan.LocalExecutors - d.Previous.LocalExecutors,
		d.Plan.CloudExecutors - d.Previous.CloudExecutors
}

// IsNoop reports whether the decision leaves capacity exactly as it was, and
// therefore needs nothing applied to the platform.
func (d Decision) IsNoop() bool { return d.Plan == d.Previous }

// projectionWire is Projection's JSON shape, in seconds for the same reason
// the rest of this contract is.
type projectionWire struct {
	BreachExpected      bool     `json:"breach_expected"`
	FirstBreachPriority Priority `json:"first_breach_priority,omitempty"`
	FirstBreachInSecs   float64  `json:"first_breach_in_seconds,omitempty"`
	PeakQueueDepth      int      `json:"peak_queue_depth"`
	DrainedAtSecs       float64  `json:"drained_at_seconds,omitempty"`
}

// MarshalJSON renders a projection with its durations in seconds.
func (p Projection) MarshalJSON() ([]byte, error) {
	return json.Marshal(projectionWire{
		BreachExpected:      p.BreachExpected,
		FirstBreachPriority: p.FirstBreachPriority,
		FirstBreachInSecs:   p.FirstBreachIn.Seconds(),
		PeakQueueDepth:      p.PeakQueueDepth,
		DrainedAtSecs:       p.DrainedAt.Seconds(),
	})
}

// UnmarshalJSON reads a projection whose durations are in seconds.
func (p *Projection) UnmarshalJSON(data []byte) error {
	var wire projectionWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*p = Projection{
		BreachExpected:      wire.BreachExpected,
		FirstBreachPriority: wire.FirstBreachPriority,
		FirstBreachIn:       time.Duration(wire.FirstBreachInSecs * float64(time.Second)),
		PeakQueueDepth:      wire.PeakQueueDepth,
		DrainedAt:           time.Duration(wire.DrainedAtSecs * float64(time.Second)),
	}
	return nil
}
