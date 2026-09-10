package colonyos_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos"
	"github.com/casperlundberg/autoscaler/internal/platform/colonyos/colonytest"
)

var now = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

func options() colonyos.WorkloadOptions {
	return colonyos.WorkloadOptions{
		Now:               now,
		ArrivalWindow:     10 * time.Minute,
		JobSecondsEnvKey:  "exec_seconds",
		DefaultJobSeconds: 20,
	}
}

func ago(d time.Duration) time.Time { return now.Add(-d) }

func TestQueuesAreGroupedByPriority(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("a", 100, ago(time.Minute), "bemis", nil),
		colonytest.Job("b", 100, ago(2*time.Minute), "bemis", nil),
		colonytest.Job("c", 25, ago(time.Minute), "bemis", nil),
	}

	got, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if got.Queues[100].Depth != 2 {
		t.Errorf("P100 depth = %d, want 2", got.Queues[100].Depth)
	}
	if got.Queues[25].Depth != 1 {
		t.Errorf("P25 depth = %d, want 1", got.Queues[25].Depth)
	}
}

func TestTheOldestWaitingJobSetsTheLevelsAge(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("young", 100, ago(30*time.Second), "bemis", nil),
		colonytest.Job("old", 100, ago(9*time.Minute), "bemis", nil),
	}

	got, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if want := 9 * time.Minute; got.Queues[100].OldestJobAge != want {
		t.Errorf("OldestJobAge = %v, want %v", got.Queues[100].OldestJobAge, want)
	}
}

// Clocks between a scheduler and this service do drift. A negative age would
// read as a job from the future and quietly suppress a real breach.
func TestAJobSubmittedInTheFutureAgesToZeroNotNegative(t *testing.T) {
	waiting := []colonyos.Process{colonytest.Job("skewed", 100, now.Add(time.Minute), "bemis", nil)}

	got, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if got.Queues[100].OldestJobAge != 0 {
		t.Errorf("OldestJobAge = %v, want 0", got.Queues[100].OldestJobAge)
	}
}

func TestArrivalRateCountsOnlyWhatArrivedInsideTheWindow(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("recent-1", 100, ago(time.Minute), "bemis", nil),
		colonytest.Job("recent-2", 100, ago(5*time.Minute), "bemis", nil),
		colonytest.Job("ancient", 100, ago(2*time.Hour), "bemis", nil),
	}

	got, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	// Two arrivals across a ten-minute window.
	want := 2.0 / (10 * 60)
	if math.Abs(got.Queues[100].ArrivalRate-want) > 1e-9 {
		t.Errorf("ArrivalRate = %v, want %v", got.Queues[100].ArrivalRate, want)
	}
}

// Work that has already been picked up still arrived. Counting only what is
// still waiting would read a busy, well-served system as having no incoming
// work at all, and scale it down mid-flow.
func TestRunningWorkCountsTowardsTheArrivalRate(t *testing.T) {
	running := []colonyos.Process{
		colonytest.Job("started", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "20"}),
	}

	got, err := colonyos.BuildWorkload(nil, running, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if got.Queues[100].ArrivalRate == 0 {
		t.Error("ArrivalRate = 0, want the running process counted as an arrival")
	}
	if got.Queues[100].Depth != 0 {
		t.Errorf("Depth = %d, want 0: a running job is not waiting", got.Queues[100].Depth)
	}
}

func TestThroughputComesFromTheExecutionTimeOfRunningWork(t *testing.T) {
	running := []colonyos.Process{
		colonytest.Job("a", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "10"}),
		colonytest.Job("b", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "30"}),
	}

	got, err := colonyos.BuildWorkload(nil, running, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	// Mean of 10s and 30s is 20s, so one executor completes 1/20 jobs a second.
	if want := 1.0 / 20.0; math.Abs(got.ExecutorThroughput-want) > 1e-9 {
		t.Errorf("ExecutorThroughput = %v, want %v", got.ExecutorThroughput, want)
	}
}

func TestWithNothingRunningTheWaitingWorkEstimatesThroughput(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("a", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "40"}),
	}

	got, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if want := 1.0 / 40.0; math.Abs(got.ExecutorThroughput-want) > 1e-9 {
		t.Errorf("ExecutorThroughput = %v, want %v", got.ExecutorThroughput, want)
	}
}

func TestWithNoDeclaredExecutionTimesTheConfiguredDefaultIsUsed(t *testing.T) {
	waiting := []colonyos.Process{colonytest.Job("a", 100, ago(time.Minute), "bemis", nil)}

	got, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if want := 1.0 / 20.0; math.Abs(got.ExecutorThroughput-want) > 1e-9 {
		t.Errorf("ExecutorThroughput = %v, want the default %v", got.ExecutorThroughput, want)
	}
}

// A declared but unreadable execution time is a broken submitter, and guessing
// past it would put a wrong throughput into every capacity calculation. The
// process is named so the submitter can be found.
func TestAnUnreadableExecutionTimeIsAnErrorNamingTheProcess(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("bad-one", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "quickly"}),
	}

	_, err := colonyos.BuildWorkload(waiting, nil, options())
	if err == nil {
		t.Fatal("BuildWorkload() = nil error for an unreadable exec_seconds")
	}
	if !strings.Contains(err.Error(), "bad-one") {
		t.Errorf("BuildWorkload() = %q, want it to name the process", err)
	}
}

func TestAnExecutionTimeOfZeroIsRefused(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("instant", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "0"}),
	}

	if _, err := colonyos.BuildWorkload(waiting, nil, options()); err == nil {
		t.Error("BuildWorkload() = nil error for a zero execution time, which would " +
			"make throughput infinite")
	}
}

func TestAnEmptyColonyProducesAnEmptyQueueAndAUsableThroughput(t *testing.T) {
	got, err := colonyos.BuildWorkload(nil, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	if len(got.Queues) != 0 {
		t.Errorf("Queues = %v, want empty", got.Queues)
	}
	// Still positive: the engine rejects an observation with zero throughput,
	// and an idle colony is not a broken one.
	if got.ExecutorThroughput <= 0 {
		t.Errorf("ExecutorThroughput = %v, want a positive fallback", got.ExecutorThroughput)
	}
}

func TestTheResultIsAValidObservationForTheEngine(t *testing.T) {
	waiting := []colonyos.Process{
		colonytest.Job("a", 100, ago(time.Minute), "bemis", map[string]string{"exec_seconds": "15"}),
		colonytest.Job("b", 25, ago(time.Hour), "bemis", nil),
	}

	workload, err := colonyos.BuildWorkload(waiting, nil, options())
	if err != nil {
		t.Fatalf("BuildWorkload() = %v", err)
	}

	state := domain.SystemState{
		Timestamp:          now,
		Queues:             workload.Queues,
		ExecutorThroughput: workload.ExecutorThroughput,
	}
	if err := state.Validate(); err != nil {
		t.Errorf("the observation built here is rejected by the engine: %v", err)
	}
}

func TestAnArrivalWindowOfZeroIsRefused(t *testing.T) {
	opts := options()
	opts.ArrivalWindow = 0

	if _, err := colonyos.BuildWorkload(nil, nil, opts); err == nil {
		t.Error("BuildWorkload() = nil error for a zero arrival window")
	}
}
