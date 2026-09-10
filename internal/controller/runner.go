package controller

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

// Runner cycles autonomous targets on their own schedule.
//
// It re-reads the registry on every tick rather than holding a goroutine per
// target. Targets are registered and removed while the service runs, and their
// decision intervals change while it runs, so a runner built around long-lived
// per-target goroutines would need lifecycle plumbing to stay correct — and
// would still be wrong for the interval, which is a settings value and can
// change between any two cycles.
type Runner struct {
	controller *Controller
	targets    *registry.Registry
	log        *slog.Logger

	mu       sync.Mutex
	lastRun  map[string]time.Time
	inflight map[string]bool

	wg sync.WaitGroup
}

// NewRunner builds a runner over a controller and its registry.
func NewRunner(controller *Controller, targets *registry.Registry) *Runner {
	return &Runner{
		controller: controller,
		targets:    targets,
		log:        slog.Default(),
		lastRun:    map[string]time.Time{},
		inflight:   map[string]bool{},
	}
}

// WithLogger returns the runner using a given logger.
func (r *Runner) WithLogger(log *slog.Logger) *Runner {
	r.log = log
	return r
}

// Run ticks until its context is cancelled.
//
// The tick is the scheduler's granularity, not a target's cadence: each target
// is cycled on its own decision interval, and the tick only has to be fine
// enough not to blur them.
func (r *Runner) Run(ctx context.Context, tick time.Duration) {
	if tick <= 0 {
		tick = time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Let whatever is mid-flight finish rather than abandoning a
			// half-applied plan.
			r.Wait()
			return
		case <-ticker.C:
			r.RunOnce(ctx, time.Now().UTC())
		}
	}
}

// RunOnce starts a cycle for every autonomous target that is due, and returns
// without waiting for them.
//
// Not waiting is deliberate: one target on an unreachable cluster will sit
// there until its request times out, and every other target has to keep being
// scaled while it does.
func (r *Runner) RunOnce(ctx context.Context, now time.Time) {
	for _, snapshot := range r.targets.List() {
		if snapshot.Target.Mode != platform.ModeAutonomous {
			// A driven target belongs to whoever is driving it. Cycling one
			// here would fight a simulation run for control of it.
			continue
		}
		if !r.due(snapshot, now) {
			continue
		}
		r.start(ctx, snapshot.Target.ID, now)
	}
}

// Wait blocks until every in-flight cycle has finished.
func (r *Runner) Wait() { r.wg.Wait() }

// due decides whether a target's interval has elapsed. The interval is read
// from the target's live settings on every tick, so shortening it takes effect
// at once rather than after the next restart.
func (r *Runner) due(snapshot registry.Snapshot, now time.Time) bool {
	interval := snapshot.Settings.Settings.DecisionInterval
	if interval <= 0 {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.inflight[snapshot.Target.ID] {
		// The previous cycle is still running. Starting another would have two
		// cycles deciding from the same stale capacity and both acting on it.
		return false
	}
	last, seen := r.lastRun[snapshot.Target.ID]
	if seen && now.Sub(last) < interval {
		return false
	}
	return true
}

func (r *Runner) start(ctx context.Context, targetID string, now time.Time) {
	r.mu.Lock()
	r.inflight[targetID] = true
	r.lastRun[targetID] = now
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.inflight, targetID)
			r.mu.Unlock()
		}()

		result, err := r.controller.Cycle(ctx, targetID, Input{At: now})
		if err != nil {
			// Logged and dropped. The failure is already recorded against the
			// target for the status view, and one unreachable cluster must not
			// stop every other target from being scaled.
			r.log.Warn("cycle failed", "target", targetID, "error", err)
			return
		}
		if result.IsNoop() {
			return
		}
		r.log.Info("scaled",
			"target", targetID,
			"action", result.Decision.Action,
			"local", result.Decision.Plan.LocalExecutors,
			"cloud", result.Decision.Plan.CloudExecutors,
			"reason", result.Decision.Reason)
	}()
}
