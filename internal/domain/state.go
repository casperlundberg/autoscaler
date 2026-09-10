// Package domain holds the vocabulary of the autoscaling problem: what the
// system looks like right now, and what a decision about it is. Nothing here
// knows about HTTP, Kubernetes, ColonyOS, or how a decision is reached — those
// live in packages that import this one, never the other way round.
package domain

import (
	"fmt"
	"sort"
	"time"
)

// Priority is a workload priority level. Higher values are more urgent, and
// each level carries its own SLA deadline. The numbers themselves are the
// scheduler's, not ours: this service never invents a level, it only ever
// reports on the ones it observes.
type Priority int

// QueueInfo is the aggregate state of a single priority level.
//
// Aggregate, deliberately: the autoscaler never sees individual jobs. SLA
// deadlines are defined per priority level, so per-level counts and ages are
// everything a capacity decision needs, and staying at that altitude keeps the
// service decoupled from any particular scheduler's job schema.
type QueueInfo struct {
	// Priority is filled in by SortedQueues from the map key; it is not part
	// of the wire representation, where the key already carries it.
	Priority Priority `json:"-"`

	// Depth is the number of jobs waiting at this level.
	Depth int `json:"depth"`

	// OldestJobAge is how long the oldest waiting job has been waiting. This,
	// against the level's deadline, is what "how much time is left" means.
	OldestJobAge time.Duration `json:"oldest_job_age"`

	// ArrivalRate is incoming jobs per second at this level.
	ArrivalRate float64 `json:"arrival_rate_per_second"`
}

// Capacity is what the platform is currently running, split by tier and by
// whether it can actually do work yet.
type Capacity struct {
	// LocalReady and CloudReady are executors that can pick up a job now.
	LocalReady int `json:"local_ready"`
	CloudReady int `json:"cloud_ready"`

	// LocalPending and CloudPending are executors that have been asked for
	// but are still starting. They are real commitments — an already-issued
	// scale-up — but they are not throughput yet.
	LocalPending int `json:"local_pending"`
	CloudPending int `json:"cloud_pending"`
}

// TotalReady is the executors that can do work right now.
func (c Capacity) TotalReady() int { return c.LocalReady + c.CloudReady }

// TotalPending is the executors that are still starting.
func (c Capacity) TotalPending() int { return c.LocalPending + c.CloudPending }

// Total is every executor the platform is holding, ready or not.
func (c Capacity) Total() int { return c.TotalReady() + c.TotalPending() }

// AsPlan is what the platform has already been asked for, which is what a
// freshly computed plan must be compared against. Comparing against ready
// executors alone would re-issue the same scale-up on every cycle of a
// coldstart, and end up with several times the intended capacity.
func (c Capacity) AsPlan() Plan {
	return Plan{
		LocalExecutors: c.LocalReady + c.LocalPending,
		CloudExecutors: c.CloudReady + c.CloudPending,
	}
}

// SystemState is one observation of the workload and the capacity serving it.
// It is produced by a platform adapter and is the only input the decision
// engine gets.
type SystemState struct {
	Timestamp time.Time `json:"timestamp"`

	// Queues is the waiting work, keyed by priority level.
	Queues map[Priority]QueueInfo `json:"queues"`

	Capacity Capacity `json:"capacity"`

	// ExecutorThroughput is jobs completed per second by one executor. It is
	// measured, not assumed: it is the term that converts "this much queue"
	// into "this many executors".
	ExecutorThroughput float64 `json:"executor_throughput_per_second"`
}

// TotalDepth is the number of jobs waiting across every priority level.
func (s SystemState) TotalDepth() int {
	total := 0
	for _, q := range s.Queues {
		total += q.Depth
	}
	return total
}

// TotalArrivalRate is incoming jobs per second across every priority level.
func (s SystemState) TotalArrivalRate() float64 {
	total := 0.0
	for _, q := range s.Queues {
		total += q.ArrivalRate
	}
	return total
}

// SortedQueues is the queues in descending priority order, each entry carrying
// its own level.
//
// This is the only ordered view of the queues, and every part of the decision
// path uses it. Go randomises map iteration on purpose; a decision that walked
// the map directly would not be reproducible from the same observation, which
// would make a replayed run disagree with the live one it is supposed to
// explain.
func (s SystemState) SortedQueues() []QueueInfo {
	out := make([]QueueInfo, 0, len(s.Queues))
	for priority, q := range s.Queues {
		q.Priority = priority
		out = append(out, q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// Validate rejects an observation that cannot be true, so a bad adapter fails
// where the bug is rather than several layers downstream inside a capacity
// calculation.
func (s SystemState) Validate() error {
	if s.Timestamp.IsZero() {
		return fmt.Errorf("timestamp is required on an observation")
	}
	if s.ExecutorThroughput <= 0 {
		return fmt.Errorf("executor_throughput_per_second must be > 0, got %v: "+
			"a zero rate means an executor completes nothing, which no capacity "+
			"calculation can act on", s.ExecutorThroughput)
	}

	for _, q := range s.SortedQueues() {
		if q.Depth < 0 {
			return fmt.Errorf("priority %d: depth must be >= 0, got %d", q.Priority, q.Depth)
		}
		if q.ArrivalRate < 0 {
			return fmt.Errorf("priority %d: arrival_rate_per_second must be >= 0, got %v",
				q.Priority, q.ArrivalRate)
		}
		if q.OldestJobAge < 0 {
			return fmt.Errorf("priority %d: oldest_job_age must be >= 0, got %v",
				q.Priority, q.OldestJobAge)
		}
	}

	for _, f := range []struct {
		name  string
		value int
	}{
		{"local_ready", s.Capacity.LocalReady},
		{"cloud_ready", s.Capacity.CloudReady},
		{"local_pending", s.Capacity.LocalPending},
		{"cloud_pending", s.Capacity.CloudPending},
	} {
		if f.value < 0 {
			return fmt.Errorf("capacity.%s must be >= 0, got %d", f.name, f.value)
		}
	}
	return nil
}
