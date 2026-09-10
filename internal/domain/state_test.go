package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
)

func sampleState() domain.SystemState {
	return domain.SystemState{
		Timestamp: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Queues: map[domain.Priority]domain.QueueInfo{
			25:  {Depth: 40, OldestJobAge: 90 * time.Second, ArrivalRate: 0.5},
			100: {Depth: 10, OldestJobAge: 20 * time.Second, ArrivalRate: 0.2},
			50:  {Depth: 5, OldestJobAge: 5 * time.Second, ArrivalRate: 0.1},
		},
		Capacity:           domain.Capacity{LocalReady: 4, CloudReady: 2, LocalPending: 1},
		ExecutorThroughput: 0.25,
	}
}

func TestTotalDepthSumsEveryPriority(t *testing.T) {
	if got, want := sampleState().TotalDepth(), 55; got != want {
		t.Errorf("TotalDepth() = %d, want %d", got, want)
	}
}

func TestTotalArrivalRateSumsEveryPriority(t *testing.T) {
	got := sampleState().TotalArrivalRate()
	if want := 0.8; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("TotalArrivalRate() = %v, want %v", got, want)
	}
}

// Map iteration in Go is deliberately randomised. Every decision this service
// makes has to be reproducible from the same observation, so the only ordered
// view of the queues is this one, and it must be ordered by urgency.
func TestSortedQueuesIsDescendingByPriority(t *testing.T) {
	queues := sampleState().SortedQueues()

	var got []domain.Priority
	for _, q := range queues {
		got = append(got, q.Priority)
	}
	want := []domain.Priority{100, 50, 25}
	if len(got) != len(want) {
		t.Fatalf("SortedQueues() returned %d levels, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SortedQueues() order = %v, want %v", got, want)
		}
	}
}

func TestSortedQueuesCarriesThePriorityOntoEachEntry(t *testing.T) {
	for _, q := range sampleState().SortedQueues() {
		if q.Priority == 0 {
			t.Errorf("SortedQueues() entry has zero Priority: %+v", q)
		}
	}
}

func TestSortedQueuesOnEmptyStateIsEmptyNotNilPanic(t *testing.T) {
	if got := (domain.SystemState{}).SortedQueues(); len(got) != 0 {
		t.Errorf("SortedQueues() on empty state = %v, want empty", got)
	}
}

func TestCapacityTotals(t *testing.T) {
	c := domain.Capacity{LocalReady: 4, CloudReady: 2, LocalPending: 1, CloudPending: 3}

	if got, want := c.TotalReady(), 6; got != want {
		t.Errorf("TotalReady() = %d, want %d", got, want)
	}
	if got, want := c.TotalPending(), 4; got != want {
		t.Errorf("TotalPending() = %d, want %d", got, want)
	}
	// Pending executors are capacity that is coming but cannot run a job yet.
	// Counting them as usable is what causes a scale-up to be issued twice.
	if got, want := c.Total(), 10; got != want {
		t.Errorf("Total() = %d, want %d", got, want)
	}
}

func TestCapacityAsPlanTakesReadyAndPendingTogether(t *testing.T) {
	c := domain.Capacity{LocalReady: 4, CloudReady: 2, LocalPending: 1, CloudPending: 3}

	// What the platform has been *asked* for is ready+pending, and that is
	// what a new plan is compared against — otherwise every cycle during a
	// coldstart would re-issue the same scale-up.
	want := domain.Plan{LocalExecutors: 5, CloudExecutors: 5}
	if got := c.AsPlan(); got != want {
		t.Errorf("AsPlan() = %+v, want %+v", got, want)
	}
}

func TestValidateAcceptsAWellFormedState(t *testing.T) {
	if err := sampleState().Validate(); err != nil {
		t.Errorf("Validate() on a well-formed state returned %v", err)
	}
}

func TestValidateRejectsMalformedStates(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.SystemState)
		wantErr string
	}{
		{
			name:    "zero timestamp",
			mutate:  func(s *domain.SystemState) { s.Timestamp = time.Time{} },
			wantErr: "timestamp",
		},
		{
			name:    "negative queue depth",
			mutate:  func(s *domain.SystemState) { s.Queues[25] = domain.QueueInfo{Depth: -1} },
			wantErr: "depth",
		},
		{
			name:    "negative arrival rate",
			mutate:  func(s *domain.SystemState) { s.Queues[25] = domain.QueueInfo{ArrivalRate: -0.1} },
			wantErr: "arrival_rate",
		},
		{
			name:    "negative oldest job age",
			mutate:  func(s *domain.SystemState) { s.Queues[25] = domain.QueueInfo{OldestJobAge: -time.Second} },
			wantErr: "oldest_job_age",
		},
		{
			name:    "negative executor count",
			mutate:  func(s *domain.SystemState) { s.Capacity.LocalReady = -1 },
			wantErr: "local_ready",
		},
		{
			// Throughput of zero means "an executor completes nothing", which
			// makes every capacity calculation divide by zero. It is always an
			// observation bug, never a real reading.
			name:    "non-positive executor throughput",
			mutate:  func(s *domain.SystemState) { s.ExecutorThroughput = 0 },
			wantErr: "executor_throughput",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := sampleState()
			tt.mutate(&state)

			err := state.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
