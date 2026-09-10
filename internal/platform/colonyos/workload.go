package colonyos

import (
	"fmt"
	"strconv"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
)

// WorkloadOptions controls how a colony's processes are turned into the
// aggregate the decision engine reads.
type WorkloadOptions struct {
	// Now is the instant the observation is taken at.
	Now time.Time

	// ArrivalWindow is how far back to look when measuring the incoming rate.
	// Too short and the rate is noise; too long and a burst is averaged away
	// before the engine can react to it.
	ArrivalWindow time.Duration

	// JobSecondsEnvKey is the process env entry declaring how long a job takes
	// to run. This is what converts a queue length into an executor count.
	JobSecondsEnvKey string

	// DefaultJobSeconds is used for work that does not declare its own.
	DefaultJobSeconds float64
}

// BuildWorkload aggregates a colony's processes into per-priority queues and a
// measured throughput.
//
// This is where a scheduler's individual jobs become the aggregate the engine
// works on. It is a pure function of its inputs so the interesting decisions —
// what counts as an arrival, where throughput comes from, what to do about
// clock skew — are testable without a ColonyOS server anywhere nearby.
func BuildWorkload(waiting, running []Process, opts WorkloadOptions) (platform.Workload, error) {
	if opts.ArrivalWindow <= 0 {
		return platform.Workload{}, fmt.Errorf("arrival window must be > 0, got %v", opts.ArrivalWindow)
	}
	if opts.DefaultJobSeconds <= 0 {
		return platform.Workload{}, fmt.Errorf("default job seconds must be > 0, got %v",
			opts.DefaultJobSeconds)
	}

	queues := map[domain.Priority]domain.QueueInfo{}
	oldestSubmission := map[domain.Priority]time.Time{}

	for _, p := range waiting {
		priority := domain.Priority(p.Priority)
		entry := queues[priority]
		entry.Depth++

		if existing, seen := oldestSubmission[priority]; !seen || p.SubmittedAt.Before(existing) {
			oldestSubmission[priority] = p.SubmittedAt
		}
		queues[priority] = entry
	}

	for priority, submittedAt := range oldestSubmission {
		age := opts.Now.Sub(submittedAt)
		if age < 0 {
			// Clocks between a scheduler and this service do drift. A negative
			// age would read as a job from the future and quietly suppress a
			// breach that is genuinely about to happen.
			age = 0
		}
		entry := queues[priority]
		entry.OldestJobAge = age
		queues[priority] = entry
	}

	// Work already picked up still arrived. Counting only what is still
	// waiting would read a busy, well-served system as having no incoming work
	// and scale it down in the middle of the flow.
	since := opts.Now.Add(-opts.ArrivalWindow)
	arrivals := map[domain.Priority]int{}
	for _, set := range [][]Process{waiting, running} {
		for _, p := range set {
			if p.SubmittedAt.After(since) {
				arrivals[domain.Priority(p.Priority)]++
			}
		}
	}
	for priority, count := range arrivals {
		entry := queues[priority]
		entry.ArrivalRate = float64(count) / opts.ArrivalWindow.Seconds()
		queues[priority] = entry
	}

	throughput, err := estimateThroughput(waiting, running, opts)
	if err != nil {
		return platform.Workload{}, err
	}

	return platform.Workload{Queues: queues, ExecutorThroughput: throughput}, nil
}

// estimateThroughput converts declared execution times into jobs per second
// per executor.
//
// Running work is preferred, because it is what executors are actually doing
// right now. Waiting work is the next best evidence, and the configured
// default is the floor — an idle colony is not a broken one, and returning
// zero throughput would have the engine reject the observation outright.
func estimateThroughput(waiting, running []Process, opts WorkloadOptions) (float64, error) {
	for _, set := range [][]Process{running, waiting} {
		mean, found, err := meanJobSeconds(set, opts)
		if err != nil {
			return 0, err
		}
		if found {
			return 1 / mean, nil
		}
	}
	return 1 / opts.DefaultJobSeconds, nil
}

func meanJobSeconds(processes []Process, opts WorkloadOptions) (mean float64, found bool, err error) {
	total, count := 0.0, 0

	for _, p := range processes {
		raw, ok := p.Env[opts.JobSecondsEnvKey]
		if !ok || raw == "" {
			continue
		}

		seconds, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil {
			// A declared but unreadable execution time is a broken submitter.
			// Guessing past it would put a wrong throughput into every
			// capacity calculation from here on, so it is named and refused.
			return 0, false, fmt.Errorf("process %q declares %s=%q, which is not a number: %w",
				p.ID, opts.JobSecondsEnvKey, raw, parseErr)
		}
		if seconds <= 0 {
			return 0, false, fmt.Errorf("process %q declares %s=%q; a job cannot take zero "+
				"time, and treating it as such makes throughput infinite",
				p.ID, opts.JobSecondsEnvKey, raw)
		}

		total += seconds
		count++
	}

	if count == 0 {
		return 0, false, nil
	}
	return total / float64(count), true, nil
}
