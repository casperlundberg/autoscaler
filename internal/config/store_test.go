package config_test

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
)

func newStore(t *testing.T) *config.Store {
	t.Helper()
	store, err := config.NewStore(config.DefaultSettings())
	if err != nil {
		t.Fatalf("NewStore() = %v", err)
	}
	return store
}

func TestNewStoreRejectsSettingsItCouldNeverRunUnder(t *testing.T) {
	bad := config.DefaultSettings()
	bad.MaxScaleUpStep = 0

	if _, err := config.NewStore(bad); err == nil {
		t.Fatal("NewStore() = nil error for invalid settings, want a refusal")
	}
}

func TestAFreshStoreStartsAtVersionOne(t *testing.T) {
	got := newStore(t).Current()

	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero, want the moment the store was created")
	}
}

func TestApplyChangesOnlyTheFieldsMentioned(t *testing.T) {
	store := newStore(t)
	before := store.Current().Settings

	got, err := store.Apply([]byte(`{"local_executor_cap": 80}`), nil, "operator@example.org")
	if err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	if got.Version != 2 {
		t.Errorf("Version = %d, want 2", got.Version)
	}
	if got.Settings.LocalExecutorCap != 80 {
		t.Errorf("LocalExecutorCap = %d, want 80", got.Settings.LocalExecutorCap)
	}
	if got.Settings.MaxScaleUpStep != before.MaxScaleUpStep {
		t.Errorf("MaxScaleUpStep = %d, want it untouched at %d",
			got.Settings.MaxScaleUpStep, before.MaxScaleUpStep)
	}
	if got.UpdatedBy != "operator@example.org" {
		t.Errorf("UpdatedBy = %q, want the actor recorded", got.UpdatedBy)
	}
}

// A rejected write has to leave the running engine on the settings it already
// had. Half-applying a document would be worse than refusing it.
func TestARejectedPatchLeavesTheStoreExactlyAsItWas(t *testing.T) {
	store := newStore(t)
	before := store.Current()

	if _, err := store.Apply([]byte(`{"safety_factor": 0.2}`), nil, "operator"); err == nil {
		t.Fatal("Apply() = nil error for settings that fail validation")
	}

	after := store.Current()
	if after.Version != before.Version {
		t.Errorf("Version = %d, want it left at %d", after.Version, before.Version)
	}
	if after.Settings.SafetyFactor != before.Settings.SafetyFactor {
		t.Errorf("SafetyFactor = %v, want it left at %v",
			after.Settings.SafetyFactor, before.Settings.SafetyFactor)
	}
}

func TestAMisspelledFieldIsRefusedRatherThanIgnored(t *testing.T) {
	store := newStore(t)

	_, err := store.Apply([]byte(`{"local_executor_capp": 80}`), nil, "operator")
	if err == nil {
		t.Fatal("Apply() = nil error for an unknown field, want a refusal")
	}
	if store.Current().Version != 1 {
		t.Error("the store moved on despite refusing the write")
	}
}

func TestAStaleWriteIsRefusedSoTwoEditorsCannotOverwriteEachOther(t *testing.T) {
	store := newStore(t)

	// Two operators both read version 1. The first write wins.
	stale := int64(1)
	if _, err := store.Apply([]byte(`{"local_executor_cap": 80}`), &stale, "first"); err != nil {
		t.Fatalf("first Apply() = %v", err)
	}

	_, err := store.Apply([]byte(`{"local_executor_cap": 12}`), &stale, "second")
	if !errors.Is(err, config.ErrVersionConflict) {
		t.Fatalf("second Apply() = %v, want ErrVersionConflict", err)
	}
	if got := store.Current().Settings.LocalExecutorCap; got != 80 {
		t.Errorf("LocalExecutorCap = %d, want the first writer's 80 to have survived", got)
	}
}

func TestAWriteWithNoExpectedVersionIsUnconditional(t *testing.T) {
	store := newStore(t)
	if _, err := store.Apply([]byte(`{"local_executor_cap": 80}`), nil, "first"); err != nil {
		t.Fatalf("first Apply() = %v", err)
	}

	// No expected version: the caller is deliberately not doing
	// read-modify-write, which is what a scripted rollout wants.
	if _, err := store.Apply([]byte(`{"local_executor_cap": 12}`), nil, "second"); err != nil {
		t.Fatalf("second Apply() = %v", err)
	}
	if got := store.Current().Settings.LocalExecutorCap; got != 12 {
		t.Errorf("LocalExecutorCap = %d, want 12", got)
	}
}

func TestHistoryNamesWhatMovedAndWhatItMovedFrom(t *testing.T) {
	store := newStore(t)
	if _, err := store.Apply([]byte(`{"local_executor_cap": 80, "dry_run": true}`), nil, "operator"); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	history := store.History(10)
	if len(history) != 1 {
		t.Fatalf("History() returned %d entries, want 1", len(history))
	}
	entry := history[0]
	if entry.Version != 2 || entry.Actor != "operator" {
		t.Errorf("entry = %+v, want version 2 by operator", entry)
	}

	changed := map[string]config.FieldChange{}
	for _, f := range entry.Fields {
		changed[f.Field] = f
	}
	cap, ok := changed["local_executor_cap"]
	if !ok {
		t.Fatalf("local_executor_cap missing from %+v", entry.Fields)
	}
	// Without the old value an audit log cannot answer the only question it is
	// ever asked: what was this before someone changed it?
	if cap.From != "50" || cap.To != "80" {
		t.Errorf("local_executor_cap changed %q -> %q, want \"50\" -> \"80\"", cap.From, cap.To)
	}
	if _, ok := changed["dry_run"]; !ok {
		t.Errorf("dry_run missing from %+v", entry.Fields)
	}
	if _, ok := changed["max_scale_up_step"]; ok {
		t.Errorf("max_scale_up_step listed as changed when it was not: %+v", entry.Fields)
	}
}

func TestHistoryIsNewestFirstAndBounded(t *testing.T) {
	store := newStore(t)
	for i := 1; i <= 5; i++ {
		if _, err := store.Apply([]byte(`{"local_executor_cap": `+strconv.Itoa(i)+`}`), nil, "operator"); err != nil {
			t.Fatalf("Apply() = %v", err)
		}
	}

	history := store.History(2)
	if len(history) != 2 {
		t.Fatalf("History(2) returned %d entries, want 2", len(history))
	}
	if history[0].Version != 6 {
		t.Errorf("History(2)[0].Version = %d, want the newest, 6", history[0].Version)
	}
	if history[1].Version != 5 {
		t.Errorf("History(2)[1].Version = %d, want 5", history[1].Version)
	}
}

func TestCurrentHandsOutACopyNotTheLiveDocument(t *testing.T) {
	store := newStore(t)

	got := store.Current()
	got.Settings.Deadlines[100] = time.Nanosecond

	if store.Current().Settings.Deadline(100) == time.Nanosecond {
		t.Error("writing to the returned settings changed the document the engine runs under")
	}
}

func TestSubscribersSeeEveryAcceptedChange(t *testing.T) {
	store := newStore(t)
	updates, unsubscribe := store.Subscribe()
	defer unsubscribe()

	if _, err := store.Apply([]byte(`{"local_executor_cap": 80}`), nil, "operator"); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	select {
	case got := <-updates:
		if got.Settings.LocalExecutorCap != 80 {
			t.Errorf("subscriber saw cap %d, want 80", got.Settings.LocalExecutorCap)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber received nothing")
	}
}

func TestUnsubscribingStopsDelivery(t *testing.T) {
	store := newStore(t)
	updates, unsubscribe := store.Subscribe()
	unsubscribe()

	if _, err := store.Apply([]byte(`{"local_executor_cap": 80}`), nil, "operator"); err != nil {
		t.Fatalf("Apply() = %v", err)
	}

	// A closed subscription reads as closed, not as a value.
	select {
	case _, open := <-updates:
		if open {
			t.Error("received an update after unsubscribing")
		}
	case <-time.After(time.Second):
		t.Error("channel neither closed nor delivered")
	}
}

// A UI that stops reading its event stream must never be able to wedge the
// settings API for everybody else.
func TestASubscriberThatNeverReadsDoesNotBlockWrites(t *testing.T) {
	store := newStore(t)
	_, unsubscribe := store.Subscribe()
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 50; i++ {
			if _, err := store.Apply([]byte(`{"local_executor_cap": `+strconv.Itoa(i)+`}`), nil, "operator"); err != nil {
				t.Errorf("Apply() = %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writes blocked behind a subscriber that is not reading")
	}
}

func TestConcurrentWritersEachGetTheirOwnVersion(t *testing.T) {
	store := newStore(t)

	const writers = 32
	var wg sync.WaitGroup
	versions := make(chan int64, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshot, err := store.Apply([]byte(`{"dry_run": true}`), nil, "operator")
			if err != nil {
				t.Errorf("Apply() = %v", err)
				return
			}
			versions <- snapshot.Version
		}()
	}
	wg.Wait()
	close(versions)

	seen := map[int64]bool{}
	for v := range versions {
		if seen[v] {
			t.Errorf("version %d was handed to two writers", v)
		}
		seen[v] = true
	}
	if got := store.Current().Version; got != writers+1 {
		t.Errorf("final Version = %d, want %d", got, writers+1)
	}
}

func TestWarningsFlagAHorizonTooShortToSeeAColdstartComing(t *testing.T) {
	s := config.DefaultSettings()
	s.Horizon = time.Minute
	s.CloudColdstart = 5 * time.Minute

	warnings := s.Warnings()
	if len(warnings) == 0 {
		t.Fatal("Warnings() is empty, want the horizon/coldstart mismatch flagged")
	}
	joined := ""
	for _, w := range warnings {
		joined += w + "\n"
	}
	if !strings.Contains(joined, "horizon") || !strings.Contains(joined, "coldstart") {
		t.Errorf("Warnings() = %q, want it to explain the horizon/coldstart relationship", joined)
	}
}

func TestDefaultSettingsProduceNoWarnings(t *testing.T) {
	if got := config.DefaultSettings().Warnings(); len(got) != 0 {
		t.Errorf("Warnings() = %v, want none for the shipped defaults", got)
	}
}
