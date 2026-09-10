package platform

import (
	"context"
	"time"
)

type cycleTimeKey struct{}

// WithCycleTime marks a context with the instant the control cycle is running
// at.
//
// This exists for one reason: a replayed run moves through compressed time,
// and an adapter that models anything time-dependent — the simulation
// adapter's coldstart, above all — has to age its state on the run's clock,
// not the wall clock. Two simulated hours can pass in a second of real time,
// and a coldstart measured against time.Now() would be over before it started.
//
// It is carried on the context rather than added to Observe and Apply because
// no adapter that talks to a real platform has any use for it: Kubernetes and
// Docker have their own clocks, and threading a parameter through every
// adapter for one adapter's benefit would be the wrong trade.
func WithCycleTime(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, cycleTimeKey{}, at)
}

// CycleTime is the instant the current cycle is running at, defaulting to now.
// A live target never sets it and gets real time, which is correct for it.
func CycleTime(ctx context.Context) time.Time {
	if at, ok := ctx.Value(cycleTimeKey{}).(time.Time); ok && !at.IsZero() {
		return at
	}
	return time.Now().UTC()
}

// Resettable is implemented by adapters holding per-target state that can be
// discarded. In practice that means the simulation adapter: each run starts
// from an empty fleet, or the previous run's executors would be counted as
// capacity the new one never provisioned.
type Resettable interface {
	Reset(targetID string)
}
