package config_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

func TestDefaultSettingsAreValid(t *testing.T) {
	if err := config.DefaultSettings().Validate(); err != nil {
		t.Fatalf("DefaultSettings() failed its own Validate(): %v", err)
	}
}

func TestDeadlineFallsBackToTheDefaultForAnUnknownPriority(t *testing.T) {
	s := config.DefaultSettings()
	s.Deadlines = map[domain.Priority]time.Duration{100: 60 * time.Second}
	s.DefaultDeadline = 12 * time.Hour

	if got, want := s.Deadline(100), 60*time.Second; got != want {
		t.Errorf("Deadline(100) = %v, want %v", got, want)
	}
	// A level the operator never configured is not an error: the scheduler
	// owns the level numbers, and a new one appearing must not stall the
	// control loop. It gets the most forgiving deadline instead.
	if got, want := s.Deadline(7), 12*time.Hour; got != want {
		t.Errorf("Deadline(7) on an unconfigured level = %v, want the default %v", got, want)
	}
}

func TestValidateRejectsIncoherentSettings(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Settings)
		wantErr string
	}{
		{"zero decision interval", func(s *config.Settings) { s.DecisionInterval = 0 }, "decision_interval"},
		{"zero horizon", func(s *config.Settings) { s.Horizon = 0 }, "horizon"},
		{
			// Simulating less far ahead than the gap between decisions means
			// the engine can never see a breach coming before it arrives.
			name:    "horizon shorter than the decision interval",
			mutate:  func(s *config.Settings) { s.Horizon = 5 * time.Second; s.DecisionInterval = 15 * time.Second },
			wantErr: "horizon",
		},
		{"zero simulation step", func(s *config.Settings) { s.SimulationStep = 0 }, "simulation_step"},
		{
			name:    "simulation step longer than the horizon",
			mutate:  func(s *config.Settings) { s.SimulationStep = 2 * time.Hour },
			wantErr: "simulation_step",
		},
		{"zero default deadline", func(s *config.Settings) { s.DefaultDeadline = 0 }, "default_deadline"},
		{
			name:    "a configured deadline of zero",
			mutate:  func(s *config.Settings) { s.Deadlines = map[domain.Priority]time.Duration{100: 0} },
			wantErr: "deadline",
		},
		{"negative local cap", func(s *config.Settings) { s.LocalExecutorCap = -1 }, "local_executor_cap"},
		{"negative cloud cap", func(s *config.Settings) { s.CloudExecutorCap = -1 }, "cloud_executor_cap"},
		{
			// A floor above the ceiling has no satisfying plan, so every cycle
			// would be a violation of one limit or the other.
			name:    "minimum local above the local cap",
			mutate:  func(s *config.Settings) { s.MinLocalExecutors = 99; s.LocalExecutorCap = 8 },
			wantErr: "min_local_executors",
		},
		{"negative local coldstart", func(s *config.Settings) { s.LocalColdstart = -time.Second }, "local_coldstart"},
		{"negative cloud coldstart", func(s *config.Settings) { s.CloudColdstart = -time.Second }, "cloud_coldstart"},
		{"negative scale-up cooldown", func(s *config.Settings) { s.ScaleUpCooldown = -time.Second }, "scale_up_cooldown"},
		{"negative scale-down cooldown", func(s *config.Settings) { s.ScaleDownCooldown = -time.Second }, "scale_down_cooldown"},
		{
			// A step limit of zero freezes capacity permanently — the most
			// dangerous possible typo, so it is rejected rather than obeyed.
			name:    "zero scale-up step",
			mutate:  func(s *config.Settings) { s.MaxScaleUpStep = 0 },
			wantErr: "max_scale_up_step",
		},
		{"zero scale-down step", func(s *config.Settings) { s.MaxScaleDownStep = 0 }, "max_scale_down_step"},
		{
			name:    "safety factor below one",
			mutate:  func(s *config.Settings) { s.SafetyFactor = 0.9 },
			wantErr: "safety_factor",
		},
		{"negative cloud minimum lifetime", func(s *config.Settings) { s.CloudMinLifetime = -time.Second }, "cloud_min_lifetime"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := config.DefaultSettings()
			tt.mutate(&s)

			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Durations cross the wire as seconds, not as Go's nanosecond integers: these
// numbers are edited by hand in the UI and read by a human in an audit log.
func TestSettingsRoundTripThroughJSONAsSeconds(t *testing.T) {
	s := config.DefaultSettings()
	s.DecisionInterval = 15 * time.Second
	s.Deadlines = map[domain.Priority]time.Duration{400: 30 * time.Second, 25: 12 * time.Hour}

	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("Unmarshal to map = %v", err)
	}
	if got, want := generic["decision_interval_seconds"], 15.0; got != want {
		t.Errorf("decision_interval_seconds = %v, want %v", got, want)
	}
	deadlines, ok := generic["deadline_seconds_by_priority"].(map[string]any)
	if !ok {
		t.Fatalf("deadline_seconds_by_priority missing or wrong shape: %#v", generic["deadline_seconds_by_priority"])
	}
	if got, want := deadlines["400"], 30.0; got != want {
		t.Errorf("deadline for P400 = %v, want %v", got, want)
	}

	var back config.Settings
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal back = %v", err)
	}
	if back.DecisionInterval != s.DecisionInterval {
		t.Errorf("DecisionInterval round-trip = %v, want %v", back.DecisionInterval, s.DecisionInterval)
	}
	if back.Deadline(25) != 12*time.Hour {
		t.Errorf("Deadline(25) round-trip = %v, want 12h", back.Deadline(25))
	}
	if back.SafetyFactor != s.SafetyFactor {
		t.Errorf("SafetyFactor round-trip = %v, want %v", back.SafetyFactor, s.SafetyFactor)
	}
}

// This is what makes PATCH work: unmarshalling a partial document onto a copy
// of the current settings has to leave every unmentioned field alone.
func TestUnmarshallingAPartialDocumentLeavesOtherFieldsIntact(t *testing.T) {
	s := config.DefaultSettings()
	s.LocalExecutorCap = 40

	if err := json.Unmarshal([]byte(`{"max_scale_up_step": 12}`), &s); err != nil {
		t.Fatalf("Unmarshal partial = %v", err)
	}

	if got, want := s.MaxScaleUpStep, 12; got != want {
		t.Errorf("MaxScaleUpStep = %d, want %d", got, want)
	}
	if got, want := s.LocalExecutorCap, 40; got != want {
		t.Errorf("LocalExecutorCap = %d, want it left at %d", got, want)
	}
	if got, want := s.DecisionInterval, config.DefaultSettings().DecisionInterval; got != want {
		t.Errorf("DecisionInterval = %v, want it left at %v", got, want)
	}
}

func TestUnmarshallingRejectsUnknownFields(t *testing.T) {
	s := config.DefaultSettings()

	// A misspelled key that is silently ignored is the worst failure mode for
	// a settings API: the operator sees 200 OK and believes the change landed.
	err := json.Unmarshal([]byte(`{"max_scale_up_stepp": 12}`), &s)
	if err == nil {
		t.Fatal("Unmarshal with an unknown field = nil, want an error")
	}
	if !strings.Contains(err.Error(), "max_scale_up_stepp") {
		t.Errorf("error = %q, want it to name the unknown field", err)
	}
}

// Settings are handed out to callers while the engine keeps running under
// them. Sharing the deadline map would let a caller change the live policy by
// writing to a value it was only ever given to read.
func TestCloneSharesNoMutableStateWithTheOriginal(t *testing.T) {
	original := config.DefaultSettings()
	clone := original.Clone()

	clone.Deadlines[100] = time.Nanosecond
	clone.LocalExecutorCap = 999

	if original.Deadline(100) == time.Nanosecond {
		t.Error("writing to the clone's deadlines changed the original")
	}
	if original.LocalExecutorCap == 999 {
		t.Error("writing to the clone's cap changed the original")
	}
}
