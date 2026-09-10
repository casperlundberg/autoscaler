// Package policy turns an observation into a plan. It is the only place in
// the service that decides anything, and it does so without knowing what a
// pod, a container or a ColonyOS executor is.
package policy

import (
	"math"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Simulate plays the queue forward under a fixed executor count and reports
// what would happen.
//
// This is the core of the service. A threshold rule ("scale up above N jobs")
// cannot answer the question an SLA actually asks, which is *will anything
// miss its deadline*, because the answer depends on arrival rate, on how the
// work is spread across priority levels, and on how much of it is already old.
// Simulating answers it directly.
//
// The model, stated plainly so its limits are visible:
//
//   - Capacity is served strict-priority. The highest level takes what it
//     needs, the next takes what is left, and so on down. This is what makes
//     low-priority starvation a predictable outcome rather than a surprise.
//   - Within a level the queue is FIFO and ages are assumed spread evenly from
//     the oldest job down to the newest. So serving half a level's backlog
//     halves the age of its oldest waiting job. That is exactly right when
//     arrivals were steady, and approximate otherwise.
//   - A level is in breach when it still has work waiting and its oldest job
//     has waited longer than the level's deadline. That single test catches
//     both failure modes: starvation, where a level never receives capacity,
//     and saturation, where it receives capacity but arrivals outrun it.
//
// The observation is not modified.
func Simulate(state domain.SystemState, executors int, settings config.Settings) domain.Projection {
	step := settings.SimulationStep
	if step <= 0 || settings.Horizon <= 0 {
		return domain.Projection{}
	}
	if executors < 0 {
		executors = 0
	}

	levels := state.SortedQueues()

	// Working copies. Depths are floats because a step serves a fractional
	// number of jobs; rounding each step would accumulate a large error over a
	// long horizon.
	depth := make([]float64, len(levels))
	headAge := make([]time.Duration, len(levels))
	for i, q := range levels {
		depth[i] = float64(q.Depth)
		headAge[i] = q.OldestJobAge
	}

	throughput := state.ExecutorThroughput
	if throughput < 0 {
		throughput = 0
	}
	servedPerStep := float64(executors) * throughput * step.Seconds()

	projection := domain.Projection{PeakQueueDepth: totalDepth(depth)}

	// A job that is already late is a breach now, not a predicted one. Saying
	// so at elapsed zero is more useful than waiting a step to notice.
	if i, ok := firstBreach(levels, depth, headAge, settings); ok {
		projection.BreachExpected = true
		projection.FirstBreachPriority = levels[i].Priority
		projection.FirstBreachIn = 0
	}

	steps := int(math.Ceil(float64(settings.Horizon) / float64(step)))
	for n := 1; n <= steps; n++ {
		elapsed := time.Duration(n) * step
		if elapsed > settings.Horizon {
			elapsed = settings.Horizon
		}

		remaining := servedPerStep
		for i := range levels {
			before := depth[i]

			served := math.Min(before, remaining)
			remaining -= served
			depth[i] = before - served

			// Ages spread evenly across the backlog: serving a fraction of the
			// level moves the head forward by that same fraction of its age.
			// Everything still waiting then ages by one step.
			ratio := 0.0
			if before > 0 {
				ratio = depth[i] / before
			}
			headAge[i] = time.Duration(float64(headAge[i])*ratio) + step

			depth[i] += levels[i].ArrivalRate * step.Seconds()
		}

		if total := totalDepth(depth); total > projection.PeakQueueDepth {
			projection.PeakQueueDepth = total
		}
		if projection.DrainedAt == 0 && totalDepth(depth) == 0 {
			projection.DrainedAt = elapsed
		}

		if !projection.BreachExpected {
			if i, ok := firstBreach(levels, depth, headAge, settings); ok {
				projection.BreachExpected = true
				projection.FirstBreachPriority = levels[i].Priority
				projection.FirstBreachIn = elapsed
			}
		}
	}

	return projection
}

// firstBreach returns the index of the most urgent level currently in breach.
// Levels are walked in descending priority, so when several breach on the same
// step the most urgent one is the one named.
func firstBreach(levels []domain.QueueInfo, depth []float64, headAge []time.Duration,
	settings config.Settings) (int, bool) {
	for i := range levels {
		// No waiting job means nothing can be late, whatever the age reading
		// says. A stale age on an empty level is a normal artefact of how
		// schedulers report.
		if depth[i] <= 0 {
			continue
		}
		if headAge[i] > settings.Deadline(levels[i].Priority) {
			return i, true
		}
	}
	return 0, false
}

func totalDepth(depth []float64) int {
	total := 0.0
	for _, d := range depth {
		total += d
	}
	return int(math.Ceil(total))
}
