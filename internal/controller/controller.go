// Package controller runs the loop: observe, decide, enact.
//
// It is the only place where the decision engine and a platform adapter meet,
// and it is deliberately thin. Everything hard lives elsewhere — the policy in
// policy, the protocols in the adapters, the tunables in config — so what is
// left here is the sequencing, plus the handful of rules that only make sense
// once you can see a whole cycle at once.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/policy"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

// Input is what a caller supplies for one cycle.
type Input struct {
	// At is the instant this cycle runs at. A simulation run passes its own
	// compressed clock; a live target leaves it unset and gets real time.
	At time.Time

	// Workload is the queue to decide against. It is required for a target on
	// a platform that cannot see its own queue, and it is honoured only for a
	// target in driven mode — an autonomous target always decides from what is
	// actually there, whatever a caller claims.
	Workload *platform.Workload
}

// Result is what one cycle produced.
type Result struct {
	Decision    domain.Decision       `json:"decision"`
	Observation platform.Observation  `json:"observation"`
	Applied     *platform.ApplyResult `json:"applied,omitempty"`
}

// IsNoop reports whether the cycle left capacity exactly as it was.
func (r Result) IsNoop() bool { return r.Decision.IsNoop() }

// Controller runs cycles against registered targets.
type Controller struct {
	platforms *platform.Registry
	targets   *registry.Registry
}

// New wires a controller to the platforms it can drive and the targets it
// knows about.
func New(platforms *platform.Registry, targets *registry.Registry) *Controller {
	return &Controller{platforms: platforms, targets: targets}
}

// Cycle runs one observe-decide-enact for a target.
//
// The whole cycle runs under one settings version, read once at the top.
// Reading settings twice would let a change land mid-cycle and produce a
// decision taken half under the old policy and half under the new — rare,
// untraceable, and impossible to reproduce from a bug report.
func (c *Controller) Cycle(ctx context.Context, targetID string, input Input) (Result, error) {
	now := input.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// Everything below runs on this cycle's clock, including whatever
	// coldstart an adapter models: a replayed run moves through compressed
	// time, and a coldstart aged against the wall clock would be over before
	// it started.
	ctx = platform.WithCycleTime(ctx, now)

	snapshot, err := c.targets.Get(targetID)
	if err != nil {
		return Result{}, err
	}
	settingsStore, err := c.targets.Settings(targetID)
	if err != nil {
		return Result{}, err
	}
	settings := settingsStore.Current()

	credentials, err := c.targets.Credentials(targetID)
	if err != nil {
		return Result{}, err
	}
	target := snapshot.Target
	target.Credentials = credentials

	provisioner, err := c.platforms.Get(target.Kind)
	if err != nil {
		return Result{}, err
	}

	result, loop, cycleErr := c.execute(ctx, provisioner, target, snapshot.Loop, settings, input, now)

	// Recorded either way. A target that has been failing for an hour must be
	// visible on its own status, not only to whoever happened to call it.
	record := registry.Cycle{Loop: loop, Err: cycleErr, At: now}
	if cycleErr == nil {
		decision := result.Decision
		record.Decision = &decision
	}
	_ = c.targets.RecordCycle(targetID, record)

	if cycleErr != nil {
		return Result{}, cycleErr
	}
	return result, nil
}

func (c *Controller) execute(ctx context.Context, provisioner platform.Provisioner,
	target platform.Target, loop policy.LoopState, settings config.Snapshot,
	input Input, now time.Time) (Result, policy.LoopState, error) {
	observation, err := provisioner.Observe(ctx, target)
	if err != nil {
		return Result{}, loop, fmt.Errorf("observing target %q: %w", target.ID, err)
	}

	workload, err := resolveWorkload(target, observation, input)
	if err != nil {
		return Result{}, loop, err
	}

	// Capacity always comes from the platform, never from the caller. A
	// controller that took someone's word for what is already running would
	// re-issue the same scale-up on every cycle and end up far past what it
	// intended.
	state := domain.SystemState{
		Timestamp:          now,
		Queues:             workload.Queues,
		Capacity:           observation.Capacity,
		ExecutorThroughput: workload.ExecutorThroughput,
	}

	decision, nextLoop := policy.Decide(state, loop, settings.Settings)
	// Stamped here rather than inside the engine: the engine is a pure
	// function of the settings value, and has no idea which version it is.
	// Without this a decision read back later cannot be explained, because the
	// settings it was taken under have moved on.
	decision.SettingsVersion = settings.Version

	result := Result{Decision: decision, Observation: observation}

	// Nothing to do. Re-sending an unchanged plan every fifteen seconds is a
	// write per target per cycle that carries no information.
	if decision.IsNoop() {
		return result, nextLoop, nil
	}
	// Dry run is how a real target is introduced to production: the decision
	// is computed and recorded so it can be read, and nothing is created.
	if settings.Settings.DryRun {
		return result, nextLoop, nil
	}

	applied, err := provisioner.Apply(ctx, target, decision.Plan)
	if err != nil {
		return result, nextLoop, fmt.Errorf("applying %+v to target %q: %w",
			decision.Plan, target.ID, err)
	}
	result.Applied = &applied
	return result, nextLoop, nil
}

// resolveWorkload decides whose view of the queue this cycle uses.
//
// An autonomous target always uses the platform's own, whatever a caller
// supplies: that is what autonomous means, and honouring a supplied workload
// there would let a mistaken caller scale live infrastructure against a queue
// that does not exist.
func resolveWorkload(target platform.Target, observation platform.Observation,
	input Input) (*platform.Workload, error) {
	if target.Mode == platform.ModeDriven && input.Workload != nil {
		return input.Workload, nil
	}
	if observation.Workload != nil {
		return observation.Workload, nil
	}
	if input.Workload != nil {
		return input.Workload, nil
	}
	return nil, fmt.Errorf(
		"target %q is on the %s platform, which cannot see its own queue, and no "+
			"workload was supplied with the request", target.ID, target.Kind)
}
