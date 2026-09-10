// Package simulation is the platform adapter that provisions nothing.
//
// It is a first-class adapter, not a test double. A simulation run computes
// its decisions with the real engine, under real settings, through the real
// control loop — the only substitution is here, at the very edge, where a plan
// would otherwise become pods. That is what makes a replayed run evidence
// about the live system rather than a model of it.
package simulation

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
)

const (
	fieldLocalColdstart = "local_coldstart_seconds"
	fieldCloudColdstart = "cloud_coldstart_seconds"
)

// Record is one plan this adapter was asked to enact.
type Record struct {
	At   time.Time   `json:"at"`
	Plan domain.Plan `json:"plan"`
}

// Adapter holds a simulated fleet per target.
type Adapter struct {
	mu     sync.Mutex
	fleets map[string]*fleet
}

// fleet is the executors a target currently holds. Each entry is the instant
// that executor becomes able to take work, so ready and pending fall out of
// comparing against the cycle's clock rather than being tracked separately and
// kept in step.
type fleet struct {
	local   []time.Time
	cloud   []time.Time
	history []Record
}

// New returns an adapter with no fleets.
func New() *Adapter {
	return &Adapter{fleets: map[string]*fleet{}}
}

// Kind identifies this adapter.
func (a *Adapter) Kind() platform.Kind { return platform.KindSimulation }

// Schema declares the two knobs a simulated platform has. Both are coldstarts,
// because coldstart is the only property of a real platform that changes what
// a decision should be: everything else about provisioning either succeeds or
// fails, but coldstart is why acting early is rational.
func (a *Adapter) Schema() platform.Schema {
	return platform.Schema{
		Kind: platform.KindSimulation,
		Summary: "Computes decisions with the real engine and reports what they " +
			"would have provisioned. Nothing is created.",
		SeesWorkload: false,
		Config: []platform.Field{
			{
				Name:        fieldLocalColdstart,
				Label:       "Local coldstart (seconds)",
				Description: "How long a newly requested on-premise executor takes before it can take work.",
				Default:     "120",
				Example:     "120",
			},
			{
				Name:        fieldCloudColdstart,
				Label:       "Cloud coldstart (seconds)",
				Description: "The same for the cloud tier, which is normally slower to start.",
				Default:     "300",
				Example:     "300",
			},
		},
	}
}

// Validate checks the coldstarts parse. There is nothing to reach, so there is
// nothing else that can be wrong.
func (a *Adapter) Validate(_ context.Context, target platform.Target) error {
	if _, err := a.coldstart(target, fieldLocalColdstart); err != nil {
		return err
	}
	_, err := a.coldstart(target, fieldCloudColdstart)
	return err
}

// Observe counts the fleet against the cycle's clock.
func (a *Adapter) Observe(ctx context.Context, target platform.Target) (platform.Observation, error) {
	now := platform.CycleTime(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()

	f := a.fleets[target.ID]
	if f == nil {
		return platform.Observation{}, nil
	}

	localReady, localPending := split(f.local, now)
	cloudReady, cloudPending := split(f.cloud, now)

	// No Workload: the queue in a simulation belongs to the log being
	// replayed, and arrives with the request. Inventing one here would mean a
	// run's results depended on this adapter's idea of a workload rather than
	// on the data under test.
	return platform.Observation{
		Capacity: domain.Capacity{
			LocalReady: localReady, LocalPending: localPending,
			CloudReady: cloudReady, CloudPending: cloudPending,
		},
	}, nil
}

// Apply makes the fleet match the plan.
func (a *Adapter) Apply(ctx context.Context, target platform.Target,
	plan domain.Plan) (platform.ApplyResult, error) {
	now := platform.CycleTime(ctx)

	localColdstart, err := a.coldstart(target, fieldLocalColdstart)
	if err != nil {
		return platform.ApplyResult{}, err
	}
	cloudColdstart, err := a.coldstart(target, fieldCloudColdstart)
	if err != nil {
		return platform.ApplyResult{}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	f := a.fleets[target.ID]
	if f == nil {
		f = &fleet{}
		a.fleets[target.ID] = f
	}

	before := domain.Plan{LocalExecutors: len(f.local), CloudExecutors: len(f.cloud)}
	f.local = resize(f.local, plan.LocalExecutors, now, localColdstart)
	f.cloud = resize(f.cloud, plan.CloudExecutors, now, cloudColdstart)
	f.history = append(f.history, Record{At: now, Plan: plan})

	applied := domain.Plan{LocalExecutors: len(f.local), CloudExecutors: len(f.cloud)}
	return platform.ApplyResult{
		Applied: applied,
		Changed: applied != before,
		Detail: fmt.Sprintf("simulated fleet %d/%d local, %d/%d cloud",
			before.LocalExecutors, applied.LocalExecutors,
			before.CloudExecutors, applied.CloudExecutors),
	}, nil
}

// Reset discards a target's fleet, so a new run starts from nothing rather
// than inheriting executors it never provisioned.
func (a *Adapter) Reset(targetID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fleets, targetID)
}

// History is every plan a target was asked to enact, oldest first.
func (a *Adapter) History(targetID string) []Record {
	a.mu.Lock()
	defer a.mu.Unlock()

	f := a.fleets[targetID]
	if f == nil {
		return nil
	}
	return append([]Record(nil), f.history...)
}

// resize grows or shrinks a tier.
//
// Growing appends executors that become ready one coldstart from now. Existing
// entries are left exactly as they are, which is what makes re-applying an
// unchanged plan a no-op — a control loop re-sends its plan every cycle, and
// restarting everyone's coldstart each time would leave a fleet permanently
// pending and never able to serve anything.
//
// Shrinking removes from the end, which is newest-first. That is the cheap
// end: an executor still starting has not done any work, while a warm one has
// already paid for its coldstart and would have to pay again if rebuilt.
func resize(tier []time.Time, want int, now time.Time, coldstart time.Duration) []time.Time {
	if want < 0 {
		want = 0
	}
	switch {
	case want > len(tier):
		readyAt := now.Add(coldstart)
		for i := len(tier); i < want; i++ {
			tier = append(tier, readyAt)
		}
	case want < len(tier):
		tier = tier[:want]
	}
	// Keep warmest first so the newest-first removal above stays true even if
	// coldstarts are reconfigured mid-run.
	sort.Slice(tier, func(i, j int) bool { return tier[i].Before(tier[j]) })
	return tier
}

func split(tier []time.Time, now time.Time) (ready, pending int) {
	for _, readyAt := range tier {
		if !readyAt.After(now) {
			ready++
		} else {
			pending++
		}
	}
	return ready, pending
}

func (a *Adapter) coldstart(target platform.Target, field string) (time.Duration, error) {
	raw := a.Schema().ConfigValue(target, field)
	if raw == "" {
		return 0, nil
	}
	seconds, err := a.Schema().ConfigInt(target, field)
	if err != nil {
		return 0, err
	}
	if seconds < 0 {
		return 0, fmt.Errorf("config.%s must be >= 0, got %d", field, seconds)
	}
	return time.Duration(seconds) * time.Second, nil
}
