// Package config holds the settings the decision engine runs under, and the
// store that lets them be replaced while the service is running.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/casperlundberg/autoscaler/internal/domain"
)

// Settings is the complete set of numbers a decision depends on. Everything an
// operator can tune lives here and nowhere else, which is what makes "change
// this at runtime" a single, checkable operation rather than a hunt through
// environment variables.
type Settings struct {
	// DecisionInterval is how often the control loop decides.
	DecisionInterval time.Duration

	// Horizon is how far ahead the queue is simulated when deciding, and
	// SimulationStep is the resolution of that simulation.
	Horizon        time.Duration
	SimulationStep time.Duration

	// Deadlines is the SLA per priority level; DefaultDeadline covers a level
	// that has not been configured.
	Deadlines       map[domain.Priority]time.Duration
	DefaultDeadline time.Duration

	// LocalExecutorCap is the physical on-premise limit — real hardware that
	// cannot be exceeded. CloudExecutorCap is a spend ceiling rather than a
	// physical one, but is enforced identically.
	LocalExecutorCap  int
	CloudExecutorCap  int
	MinLocalExecutors int

	// Coldstart is how long a newly requested executor takes before it can
	// take work. It is what makes acting early rational.
	LocalColdstart time.Duration
	CloudColdstart time.Duration

	// Hysteresis. The asymmetry is intentional and is the single most
	// important guard against oscillation: go up fast, come down slowly.
	// A queue that is growing costs SLA breaches immediately; a queue that has
	// drained costs only idle executors, and tearing them down too eagerly
	// means paying the coldstart again minutes later.
	ScaleUpCooldown   time.Duration
	ScaleDownCooldown time.Duration
	MaxScaleUpStep    int
	MaxScaleDownStep  int

	// CloudMinLifetime keeps cloud capacity for at least this long once it is
	// paid for, so a brief lull cannot churn it.
	CloudMinLifetime time.Duration

	// SafetyFactor is headroom on required capacity, covering the error in the
	// throughput estimate the requirement is computed from. 1.0 means none.
	SafetyFactor float64

	// DryRun computes and records decisions without ever asking the platform
	// to change. It is how a real target is introduced to production safely.
	DryRun bool
}

// DefaultSettings is a coherent starting point, tuned for the seismic
// processing workload this service was built for: a 15s cadence, deadlines
// from 30 seconds at the top level to a day at the decay floor, and hysteresis
// that bursts hard and retreats slowly.
func DefaultSettings() Settings {
	return Settings{
		DecisionInterval: 15 * time.Second,
		Horizon:          15 * time.Minute,
		SimulationStep:   15 * time.Second,

		Deadlines: map[domain.Priority]time.Duration{
			400: 30 * time.Second,
			100: 60 * time.Second,
			50:  5 * time.Minute,
			25:  12 * time.Hour,
			0:   24 * time.Hour,
		},
		DefaultDeadline: 24 * time.Hour,

		LocalExecutorCap:  50,
		CloudExecutorCap:  200,
		MinLocalExecutors: 1,

		LocalColdstart: 2 * time.Minute,
		CloudColdstart: 5 * time.Minute,

		ScaleUpCooldown:   0,
		ScaleDownCooldown: 5 * time.Minute,
		MaxScaleUpStep:    50,
		MaxScaleDownStep:  5,

		CloudMinLifetime: 10 * time.Minute,

		SafetyFactor: 1.15,
		DryRun:       false,
	}
}

// Deadline is the SLA for a priority level.
//
// An unconfigured level falls back to DefaultDeadline rather than failing. The
// scheduler owns which levels exist, so a level appearing that the operator
// has not yet configured is a normal event, and stalling the control loop over
// it would be a far worse outcome than treating it as low-urgency work.
func (s Settings) Deadline(p domain.Priority) time.Duration {
	if d, ok := s.Deadlines[p]; ok {
		return d
	}
	return s.DefaultDeadline
}

// Validate rejects a settings document that cannot produce sensible decisions.
// It runs before any document is accepted, so the running engine is never
// handed one of these.
func (s Settings) Validate() error {
	if s.DecisionInterval <= 0 {
		return fmt.Errorf("decision_interval_seconds must be > 0, got %v", s.DecisionInterval)
	}
	if s.Horizon <= 0 {
		return fmt.Errorf("horizon_seconds must be > 0, got %v", s.Horizon)
	}
	if s.Horizon < s.DecisionInterval {
		return fmt.Errorf("horizon_seconds (%v) must be >= decision_interval_seconds (%v): "+
			"simulating less far ahead than the gap between decisions means a breach "+
			"can never be seen before it happens", s.Horizon, s.DecisionInterval)
	}
	if s.SimulationStep <= 0 {
		return fmt.Errorf("simulation_step_seconds must be > 0, got %v", s.SimulationStep)
	}
	if s.SimulationStep > s.Horizon {
		return fmt.Errorf("simulation_step_seconds (%v) must be <= horizon_seconds (%v)",
			s.SimulationStep, s.Horizon)
	}
	if s.DefaultDeadline <= 0 {
		return fmt.Errorf("default_deadline_seconds must be > 0, got %v", s.DefaultDeadline)
	}
	for _, p := range sortedPriorities(s.Deadlines) {
		if s.Deadlines[p] <= 0 {
			return fmt.Errorf("deadline for priority %d must be > 0, got %v", p, s.Deadlines[p])
		}
	}
	if s.LocalExecutorCap < 0 {
		return fmt.Errorf("local_executor_cap must be >= 0, got %d", s.LocalExecutorCap)
	}
	if s.CloudExecutorCap < 0 {
		return fmt.Errorf("cloud_executor_cap must be >= 0, got %d", s.CloudExecutorCap)
	}
	if s.MinLocalExecutors < 0 {
		return fmt.Errorf("min_local_executors must be >= 0, got %d", s.MinLocalExecutors)
	}
	if s.MinLocalExecutors > s.LocalExecutorCap {
		return fmt.Errorf("min_local_executors (%d) must be <= local_executor_cap (%d): "+
			"a floor above the ceiling leaves no plan that satisfies both",
			s.MinLocalExecutors, s.LocalExecutorCap)
	}
	if s.LocalColdstart < 0 {
		return fmt.Errorf("local_coldstart_seconds must be >= 0, got %v", s.LocalColdstart)
	}
	if s.CloudColdstart < 0 {
		return fmt.Errorf("cloud_coldstart_seconds must be >= 0, got %v", s.CloudColdstart)
	}
	if s.ScaleUpCooldown < 0 {
		return fmt.Errorf("scale_up_cooldown_seconds must be >= 0, got %v", s.ScaleUpCooldown)
	}
	if s.ScaleDownCooldown < 0 {
		return fmt.Errorf("scale_down_cooldown_seconds must be >= 0, got %v", s.ScaleDownCooldown)
	}
	if s.MaxScaleUpStep <= 0 {
		return fmt.Errorf("max_scale_up_step must be > 0, got %d: a step of zero freezes "+
			"capacity permanently", s.MaxScaleUpStep)
	}
	if s.MaxScaleDownStep <= 0 {
		return fmt.Errorf("max_scale_down_step must be > 0, got %d: a step of zero freezes "+
			"capacity permanently", s.MaxScaleDownStep)
	}
	if s.CloudMinLifetime < 0 {
		return fmt.Errorf("cloud_min_lifetime_seconds must be >= 0, got %v", s.CloudMinLifetime)
	}
	if s.SafetyFactor < 1 {
		return fmt.Errorf("safety_factor must be >= 1.0, got %v: a factor below one "+
			"deliberately provisions less than the requirement", s.SafetyFactor)
	}
	return nil
}

func sortedPriorities(m map[domain.Priority]time.Duration) []domain.Priority {
	out := make([]domain.Priority, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// settingsWire is the JSON shape. Durations cross the wire as seconds because
// these values are typed in by hand in the settings editor and read back in an
// audit log; Go's native nanosecond integers are neither writable nor readable
// by a person.
type settingsWire struct {
	DecisionInterval float64 `json:"decision_interval_seconds"`
	Horizon          float64 `json:"horizon_seconds"`
	SimulationStep   float64 `json:"simulation_step_seconds"`

	Deadlines       map[string]float64 `json:"deadline_seconds_by_priority"`
	DefaultDeadline float64            `json:"default_deadline_seconds"`

	LocalExecutorCap  int `json:"local_executor_cap"`
	CloudExecutorCap  int `json:"cloud_executor_cap"`
	MinLocalExecutors int `json:"min_local_executors"`

	LocalColdstart float64 `json:"local_coldstart_seconds"`
	CloudColdstart float64 `json:"cloud_coldstart_seconds"`

	ScaleUpCooldown   float64 `json:"scale_up_cooldown_seconds"`
	ScaleDownCooldown float64 `json:"scale_down_cooldown_seconds"`
	MaxScaleUpStep    int     `json:"max_scale_up_step"`
	MaxScaleDownStep  int     `json:"max_scale_down_step"`

	CloudMinLifetime float64 `json:"cloud_min_lifetime_seconds"`

	SafetyFactor float64 `json:"safety_factor"`
	DryRun       bool    `json:"dry_run"`
}

func secs(d time.Duration) float64 { return d.Seconds() }

func dur(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func (s Settings) toWire() settingsWire {
	deadlines := make(map[string]float64, len(s.Deadlines))
	for p, d := range s.Deadlines {
		deadlines[strconv.Itoa(int(p))] = secs(d)
	}
	return settingsWire{
		DecisionInterval:  secs(s.DecisionInterval),
		Horizon:           secs(s.Horizon),
		SimulationStep:    secs(s.SimulationStep),
		Deadlines:         deadlines,
		DefaultDeadline:   secs(s.DefaultDeadline),
		LocalExecutorCap:  s.LocalExecutorCap,
		CloudExecutorCap:  s.CloudExecutorCap,
		MinLocalExecutors: s.MinLocalExecutors,
		LocalColdstart:    secs(s.LocalColdstart),
		CloudColdstart:    secs(s.CloudColdstart),
		ScaleUpCooldown:   secs(s.ScaleUpCooldown),
		ScaleDownCooldown: secs(s.ScaleDownCooldown),
		MaxScaleUpStep:    s.MaxScaleUpStep,
		MaxScaleDownStep:  s.MaxScaleDownStep,
		CloudMinLifetime:  secs(s.CloudMinLifetime),
		SafetyFactor:      s.SafetyFactor,
		DryRun:            s.DryRun,
	}
}

// MarshalJSON renders settings in the wire shape.
func (s Settings) MarshalJSON() ([]byte, error) { return json.Marshal(s.toWire()) }

// UnmarshalJSON applies a document onto the receiver's current value, so a
// partial document is a patch: fields the caller did not mention keep the
// value they already had. That is what lets the settings editor send one
// changed field without first reconstructing the whole document, and what lets
// a full document be sent as an ordinary replace.
//
// Unknown fields are rejected. A misspelled key that is silently dropped is
// the worst outcome a settings API can produce, because the operator is told
// the change succeeded and it did not.
func (s *Settings) UnmarshalJSON(data []byte) error {
	wire := s.toWire()

	// Absent means "leave the deadlines alone"; present means "these are now
	// the deadlines". Decoding onto the existing map would instead merge, and
	// a level could never be removed.
	wire.Deadlines = nil

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return err
	}

	out := Settings{
		DecisionInterval:  dur(wire.DecisionInterval),
		Horizon:           dur(wire.Horizon),
		SimulationStep:    dur(wire.SimulationStep),
		Deadlines:         s.Deadlines,
		DefaultDeadline:   dur(wire.DefaultDeadline),
		LocalExecutorCap:  wire.LocalExecutorCap,
		CloudExecutorCap:  wire.CloudExecutorCap,
		MinLocalExecutors: wire.MinLocalExecutors,
		LocalColdstart:    dur(wire.LocalColdstart),
		CloudColdstart:    dur(wire.CloudColdstart),
		ScaleUpCooldown:   dur(wire.ScaleUpCooldown),
		ScaleDownCooldown: dur(wire.ScaleDownCooldown),
		MaxScaleUpStep:    wire.MaxScaleUpStep,
		MaxScaleDownStep:  wire.MaxScaleDownStep,
		CloudMinLifetime:  dur(wire.CloudMinLifetime),
		SafetyFactor:      wire.SafetyFactor,
		DryRun:            wire.DryRun,
	}

	if wire.Deadlines != nil {
		deadlines := make(map[domain.Priority]time.Duration, len(wire.Deadlines))
		for key, seconds := range wire.Deadlines {
			p, err := strconv.Atoi(key)
			if err != nil {
				return fmt.Errorf("deadline_seconds_by_priority: key %q is not a priority level: %w", key, err)
			}
			deadlines[domain.Priority(p)] = dur(seconds)
		}
		out.Deadlines = deadlines
	}

	*s = out
	return nil
}

// Clone returns a copy that shares no mutable state with the original, so a
// caller cannot reach through a handed-out settings value and mutate the
// document the engine is running under.
func (s Settings) Clone() Settings {
	out := s
	out.Deadlines = make(map[domain.Priority]time.Duration, len(s.Deadlines))
	for p, d := range s.Deadlines {
		out.Deadlines[p] = d
	}
	return out
}
