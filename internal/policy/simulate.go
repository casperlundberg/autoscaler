// Package policy turns an observation into a plan. It is the only place in
// the service that decides anything, and it does so without knowing what a
// pod, a container or a ColonyOS executor is.
package policy

import (
	"math"
	"sort"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Simulate plays the queue forward under a given capacity ramp and reports
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
//   - Capacity is whatever Availability says can serve at that point in the
//     horizon, not a flat count. Executors that are still starting contribute
//     nothing until they have started, which is what makes a scale-up during
//     a coldstart predict the breach it is actually going to get.
//   - Work exempt from cloud burst is served exactly like the rest: in
//     priority order, and first in, first out beside the counted work at its
//     level. Its breaches are predicted like any other, and flagged, so that
//     Required can decline to buy cloud for them.
//
// The observation is not modified.
func Simulate(state domain.SystemState, available Availability, settings config.Settings) domain.Projection {
	step := settings.SimulationStep
	if step <= 0 || settings.Horizon <= 0 {
		return domain.Projection{}
	}
	levels := levelsOf(state)
	exempt := state.HasBurstExempt()

	throughput := state.ExecutorThroughput
	if throughput < 0 {
		throughput = 0
	}
	projection := domain.Projection{PeakQueueDepth: totalDepth(levels)}

	// A counted breach anywhere in the horizon is what makes a breach one the
	// plan failed to prevent rather than one it accepted. Without exempt work
	// every breach is counted, and the first one found settles it.
	countedBreach := false
	check := func(elapsed time.Duration) {
		if !projection.BreachExpected {
			if i, ok := firstBreach(levels, settings, false); ok {
				projection.BreachExpected = true
				projection.FirstBreachPriority = levels[i].priority
				projection.FirstBreachIn = elapsed
				countedBreach = !exempt
			}
		}
		if exempt && !countedBreach {
			_, countedBreach = firstBreach(levels, settings, true)
		}
	}

	// A job that is already late is a breach now, not a predicted one. Saying
	// so at elapsed zero is more useful than waiting a step to notice.
	check(0)

	steps := int(math.Ceil(float64(settings.Horizon) / float64(step)))
	for n := 1; n <= steps; n++ {
		elapsed := time.Duration(n) * step
		if elapsed > settings.Horizon {
			elapsed = settings.Horizon
		}

		// Capacity as it stands at the start of this step. An executor that
		// becomes ready part-way through did not serve the work that was
		// waiting at the beginning of it, and crediting it would let the
		// projection borrow throughput from the future.
		remaining := float64(available.Serving(elapsed-step)) * throughput * step.Seconds()

		for i := range levels {
			remaining = levels[i].serve(remaining, step)
		}

		if total := totalDepth(levels); total > projection.PeakQueueDepth {
			projection.PeakQueueDepth = total
		}
		if projection.DrainedAt == 0 && totalDepth(levels) == 0 {
			projection.DrainedAt = elapsed
		}

		check(elapsed)
	}

	projection.BreachesExemptOnly = projection.BreachExpected && !countedBreach
	return projection
}

// work is one kind of work at one level during a simulation: its backlog, as a
// float because a step serves a fractional number of jobs and rounding each
// step would accumulate a large error over a long horizon; the age of its
// oldest job; and its arrival rate.
type work struct {
	depth   float64
	headAge time.Duration
	rate    float64
}

// level is one priority level: counted work, and work exempt from cloud
// burst.
type level struct {
	priority domain.Priority
	counted  work
	exempt   work

	// mixed is whether both kinds share the level. A level with only one kind
	// is simulated exactly as levels were before exemption existed, so an
	// observation without exempt work gets the decision it always got.
	mixed bool
}

// levelsOf is the observation's levels, most urgent first.
func levelsOf(state domain.SystemState) []level {
	byPriority := map[domain.Priority]*level{}
	var out []*level
	add := func(q domain.QueueInfo, exempt bool) {
		l := byPriority[q.Priority]
		if l == nil {
			l = &level{priority: q.Priority}
			byPriority[q.Priority] = l
			out = append(out, l)
		}
		w := work{depth: float64(q.Depth), headAge: q.OldestJobAge, rate: q.ArrivalRate}
		if exempt {
			l.exempt = w
		} else {
			l.counted = w
		}
	}
	for _, q := range state.SortedQueues() {
		add(q, false)
	}
	for _, q := range state.SortedBurstExempt() {
		// Exempt levels with nothing waiting and nothing arriving stay empty
		// for the whole horizon, and are left out rather than turning a level
		// into a mixed one.
		if q.Depth > 0 || q.ArrivalRate > 0 {
			add(q, true)
		}
	}

	levels := make([]level, len(out))
	for i, l := range out {
		l.mixed = occupied(l.counted) && occupied(l.exempt)
		levels[i] = *l
	}
	sort.SliceStable(levels, func(i, j int) bool { return levels[i].priority > levels[j].priority })
	return levels
}

func occupied(w work) bool { return w.depth > 0 || w.rate > 0 }

// serve spends up to remaining jobs of capacity on the level, ages what is
// left by one step, admits the step's arrivals, and returns the capacity left
// for the levels below.
func (l *level) serve(remaining float64, step time.Duration) float64 {
	before := l.counted.depth + l.exempt.depth
	if !l.mixed {
		// Summing in a zero would not change the float, but only one kind is
		// served here, and reading its depth directly keeps that obvious.
		before = l.only().depth
	}
	served := math.Min(before, remaining)
	remaining -= served

	if l.mixed {
		serveOldestFirst(&l.counted, &l.exempt, served)
	} else {
		serveEvenly(l.only(), served)
	}
	for _, w := range []*work{&l.counted, &l.exempt} {
		w.headAge += step
		w.depth += w.rate * step.Seconds()
	}
	return remaining
}

// only is the level's single kind of work when it is not mixed.
func (l *level) only() *work {
	if occupied(l.exempt) {
		return &l.exempt
	}
	return &l.counted
}

// serveEvenly serves one kind of work on its own. Ages are spread evenly
// across the backlog, so serving a fraction of it moves the head forward by
// that same fraction of its age.
func serveEvenly(w *work, served float64) {
	before := w.depth
	w.depth = before - served
	ratio := 0.0
	if before > 0 {
		ratio = w.depth / before
	}
	w.headAge = time.Duration(float64(w.headAge) * ratio)
}

// serveOldestFirst serves two kinds of work sharing a level, first in, first
// out across both.
//
// Each kind's ages are spread evenly from its head down to zero, as for a
// level on its own. Serving oldest first then removes every job older than
// some cutoff age, whichever kind it is, and the cutoff is the one that
// removes exactly the jobs served: the older kind alone down to the younger
// kind's head, and both together below that. Each kind's new head is the
// cutoff. With one kind empty this is serveEvenly, as it should be.
func serveOldestFirst(a, b *work, served float64) {
	older, newer := a, b
	if b.headAge > a.headAge {
		older, newer = b, a
	}
	switch {
	case newer.depth <= 0:
		serveEvenly(older, served)
		newer.headAge = 0
		return
	case older.depth <= 0:
		serveEvenly(newer, served)
		older.headAge = 0
		return
	}

	oldAge, newAge := float64(older.headAge), float64(newer.headAge)
	if oldAge <= 0 {
		// Everything in both arrived this instant: no order to follow, so
		// each kind gives up the same share.
		share := served / (older.depth + newer.depth)
		older.depth -= older.depth * share
		newer.depth -= newer.depth * share
		return
	}

	olderAlone := older.depth * (oldAge - newAge) / oldAge
	switch {
	case served <= olderAlone:
		serveEvenly(older, served)
	case newAge <= 0:
		rest := served - older.depth
		older.depth, older.headAge = 0, 0
		newer.depth = math.Max(0, newer.depth-rest)
	default:
		cutoff := (older.depth + newer.depth - served) / (older.depth/oldAge + newer.depth/newAge)
		cutoff = math.Max(0, math.Min(cutoff, newAge))
		older.depth *= cutoff / oldAge
		newer.depth *= cutoff / newAge
		older.headAge = time.Duration(cutoff)
		newer.headAge = time.Duration(cutoff)
	}
}

// mixedLevelThreshold is how much of one kind of work a mixed level has to
// hold before that kind counts as waiting.
//
// On its own a level drains to exactly zero once capacity covers it. Beside
// another kind it never does: the even spread of ages leaves a sliver of each
// kind at every age until the whole level is clear, and that sliver's head is
// the level's head — so a fifth of a job of counted work sitting in an exempt
// backlog would breach with the backlog and buy cloud for work that is not
// there. Half a job is the point where rounding says a job is waiting.
const mixedLevelThreshold = 0.5

// firstBreach returns the index of the most urgent level currently in breach.
// Levels are walked in descending priority, so when several breach on the same
// step the most urgent one is the one named. countedOnly ignores work exempt
// from cloud burst.
func firstBreach(levels []level, settings config.Settings, countedOnly bool) (int, bool) {
	for i, l := range levels {
		deadline := settings.Deadline(l.priority)
		if late(l.counted, deadline, l.mixed) || (!countedOnly && late(l.exempt, deadline, l.mixed)) {
			return i, true
		}
	}
	return 0, false
}

// late is whether a kind of work has a job waiting past its deadline.
func late(w work, deadline time.Duration, mixed bool) bool {
	// No waiting job means nothing can be late, whatever the age reading
	// says. A stale age on an empty level is a normal artefact of how
	// schedulers report.
	if w.depth <= 0 || (mixed && w.depth < mixedLevelThreshold) {
		return false
	}
	return w.headAge > deadline
}

func totalDepth(levels []level) int {
	total := 0.0
	for _, l := range levels {
		if l.mixed {
			total += l.counted.depth + l.exempt.depth
		} else {
			total += l.only().depth
		}
	}
	return int(math.Ceil(total))
}
