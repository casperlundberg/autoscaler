package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/simulation"
	"github.com/casperlundberg/autoscaler/internal/policy"
	"github.com/casperlundberg/autoscaler/internal/registry"
	"github.com/casperlundberg/autoscaler/internal/secret"
)

// refusing is a platform whose Validate always fails, so registration-time
// checking can be tested without standing up a cluster.
type refusing struct{ platform.Provisioner }

func (refusing) Kind() platform.Kind { return "refusing" }
func (refusing) Schema() platform.Schema {
	return platform.Schema{Kind: "refusing", Summary: "always refuses"}
}
func (refusing) Validate(context.Context, platform.Target) error {
	return errors.New("these credentials do not work")
}

// watching is a platform that can see its own queue, and may therefore hold an
// autonomous target. simulation cannot, so without this nothing in this
// package could exercise autonomous mode at all.
type watching struct{ platform.Provisioner }

func (watching) Kind() platform.Kind { return "watching" }
func (watching) Schema() platform.Schema {
	return platform.Schema{Kind: "watching", Summary: "sees its own queue", SeesWorkload: true}
}
func (watching) Validate(context.Context, platform.Target) error { return nil }

func platforms(t *testing.T) *platform.Registry {
	t.Helper()
	reg, err := platform.NewRegistry(simulation.New(), refusing{}, watching{})
	if err != nil {
		t.Fatalf("NewRegistry() = %v", err)
	}
	return reg
}

func newRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.New(platforms(t), registry.NewMemoryStore())
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}
	return reg
}

func simTarget(id string) platform.Target {
	return platform.Target{
		ID: id, Name: "Simulated " + id, Kind: platform.KindSimulation,
		Config: map[string]string{"local_coldstart_seconds": "60"},
	}
}

func TestATargetCanBeRegisteredAndReadBack(t *testing.T) {
	reg := newRegistry(t)

	created, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings())
	if err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if created.Target.Name != "Simulated storhall" {
		t.Errorf("Name = %q", created.Target.Name)
	}

	got, err := reg.Get("storhall")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got.Target.Kind != platform.KindSimulation {
		t.Errorf("Kind = %q", got.Target.Kind)
	}
	if got.Settings.Version != 1 {
		t.Errorf("Settings.Version = %d, want 1", got.Settings.Version)
	}
}

func TestRegisteringAnUnknownPlatformIsRefused(t *testing.T) {
	reg := newRegistry(t)
	target := simTarget("storhall")
	target.Kind = "openstack"

	_, err := reg.Create(context.Background(), target, config.DefaultSettings())
	if err == nil || !strings.Contains(err.Error(), "openstack") {
		t.Errorf("Create() = %v, want a refusal naming the unknown platform", err)
	}
}

// A target whose credentials do not work is worth finding out about now, not
// during the first burst it is supposed to absorb.
func TestATargetThePlatformRejectsIsNotRegistered(t *testing.T) {
	reg := newRegistry(t)
	target := platform.Target{ID: "bad", Kind: "refusing"}

	_, err := reg.Create(context.Background(), target, config.DefaultSettings())
	if err == nil {
		t.Fatal("Create() = nil, want the platform's refusal")
	}
	if _, err := reg.Get("bad"); err == nil {
		t.Error("the rejected target was registered anyway")
	}
}

func TestSettingsThatCannotRunAreRefusedAtRegistration(t *testing.T) {
	reg := newRegistry(t)
	settings := config.DefaultSettings()
	settings.MaxScaleUpStep = 0

	if _, err := reg.Create(context.Background(), simTarget("storhall"), settings); err == nil {
		t.Error("Create() = nil for settings the engine could not run under")
	}
}

func TestTwoTargetsCannotShareAnID(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("first Create() = %v", err)
	}

	_, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings())
	if !errors.Is(err, registry.ErrAlreadyExists) {
		t.Errorf("second Create() = %v, want ErrAlreadyExists", err)
	}
}

func TestAnIDThatIsNotUsableAsAResourceNameIsRefused(t *testing.T) {
	reg := newRegistry(t)

	// The id becomes part of Deployment names, container names and Secret
	// names, so it has to be a legal one everywhere before it is accepted.
	for _, id := range []string{"", "Storhall", "stor hall", "stor_hall", "-storhall", strings.Repeat("s", 200)} {
		target := simTarget(id)
		if _, err := reg.Create(context.Background(), target, config.DefaultSettings()); err == nil {
			t.Errorf("Create() accepted the id %q", id)
		}
	}
}

func TestGettingAnUnknownTargetIsADistinguishableError(t *testing.T) {
	_, err := newRegistry(t).Get("nobody")

	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound", err)
	}
}

func TestListIsSortedSoTheUIDoesNotReshuffle(t *testing.T) {
	reg := newRegistry(t)
	for _, id := range []string{"zulu", "alpha", "mike"} {
		if _, err := reg.Create(context.Background(), simTarget(id), config.DefaultSettings()); err != nil {
			t.Fatalf("Create(%s) = %v", id, err)
		}
	}

	got := reg.List()
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("List() returned %d targets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Target.ID != want[i] {
			t.Fatalf("List() = %v, want %v", got, want)
		}
	}
}

func TestUpdatingATargetKeepsItsSettingsAndItsVersion(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if _, err := reg.ApplySettings("storhall", []byte(`{"local_executor_cap": 80}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	renamed := simTarget("storhall")
	renamed.Name = "Storhall North"
	if _, err := reg.Update(context.Background(), renamed); err != nil {
		t.Fatalf("Update() = %v", err)
	}

	got, err := reg.Get("storhall")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if got.Target.Name != "Storhall North" {
		t.Errorf("Name = %q, want the update applied", got.Target.Name)
	}
	// Editing a target's namespace must not silently reset the policy it runs
	// under, nor the version other editors are holding.
	if got.Settings.Settings.LocalExecutorCap != 80 {
		t.Errorf("LocalExecutorCap = %d, want the tuned 80 preserved",
			got.Settings.Settings.LocalExecutorCap)
	}
	if got.Settings.Version != 2 {
		t.Errorf("Settings.Version = %d, want 2", got.Settings.Version)
	}
}

// The obvious UI flow: read a target, change one field, send it back. The
// credentials it read were redacted, and sending them again must not wipe the
// real ones.
func TestUpdatingWithRedactedCredentialsKeepsTheStoredKeys(t *testing.T) {
	reg := newRegistry(t)
	original := simTarget("storhall")
	original.Credentials = secret.NewBundle(map[string]string{"bearer_token": "real-token"})
	if _, err := reg.Create(context.Background(), original, config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	// What the UI would send back: a bundle carrying only redaction markers.
	resubmitted := simTarget("storhall")
	resubmitted.Credentials = mustRoundTrip(t, original.Credentials)

	if _, err := reg.Update(context.Background(), resubmitted); err != nil {
		t.Fatalf("Update() = %v", err)
	}

	stored, err := reg.Credentials("storhall")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}
	if got, _ := stored.Get("bearer_token"); got != "real-token" {
		t.Errorf("bearer_token = %q, want the stored key preserved", got)
	}
}

func TestDeletingATargetRemovesIt(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	if err := reg.Delete("storhall"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if _, err := reg.Get("storhall"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Get() after Delete = %v, want ErrNotFound", err)
	}
}

func TestDeletingAnUnknownTargetSaysSo(t *testing.T) {
	if err := newRegistry(t).Delete("nobody"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Delete() = %v, want ErrNotFound", err)
	}
}

func TestLoopStateSurvivesBetweenCycles(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	when := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if err := reg.RecordCycle("storhall", registry.Cycle{
		Loop:     policyLoop(when),
		Decision: &domain.Decision{At: when, Reason: "scaled up"},
	}); err != nil {
		t.Fatalf("RecordCycle() = %v", err)
	}

	got, err := reg.Get("storhall")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if !got.Loop.LastScaleUp.Equal(when) {
		t.Errorf("Loop.LastScaleUp = %v, want %v", got.Loop.LastScaleUp, when)
	}
	if got.LastDecision == nil || got.LastDecision.Reason != "scaled up" {
		t.Errorf("LastDecision = %+v, want the recorded decision", got.LastDecision)
	}
}

// A target that keeps failing has to be visible without reading logs, or the
// first anyone knows is a queue that never drained.
func TestACycleErrorIsRememberedForTheStatusView(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	if err := reg.RecordCycle("storhall", registry.Cycle{Err: errors.New("api server unreachable")}); err != nil {
		t.Fatalf("RecordCycle() = %v", err)
	}

	got, _ := reg.Get("storhall")
	if !strings.Contains(got.LastError, "unreachable") {
		t.Errorf("LastError = %q, want the failure recorded", got.LastError)
	}

	if err := reg.RecordCycle("storhall", registry.Cycle{Decision: &domain.Decision{}}); err != nil {
		t.Fatalf("second RecordCycle() = %v", err)
	}
	got, _ = reg.Get("storhall")
	if got.LastError != "" {
		t.Errorf("LastError = %q, want it cleared by a cycle that worked", got.LastError)
	}
}

func TestConcurrentReadersAndWritersDoNotRace(t *testing.T) {
	reg := newRegistry(t)
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := reg.Get("storhall"); err != nil {
					t.Errorf("Get() = %v", err)
					return
				}
				reg.List()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := reg.RecordCycle("storhall", registry.Cycle{Decision: &domain.Decision{}}); err != nil {
					t.Errorf("RecordCycle() = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// Without persistence a restart loses every target and every access key, and
// the service comes back unable to scale anything.
func TestTargetsAndTheirKeysSurviveARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")

	store, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() = %v", err)
	}
	first, err := registry.New(platforms(t), store)
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	target := simTarget("storhall")
	target.Credentials = secret.NewBundle(map[string]string{"bearer_token": "real-token"})
	if _, err := first.Create(context.Background(), target, config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	if _, err := first.ApplySettings("storhall", []byte(`{"local_executor_cap": 80}`), nil, "operator"); err != nil {
		t.Fatalf("ApplySettings() = %v", err)
	}

	reopened, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopening NewFileStore() = %v", err)
	}
	second, err := registry.New(platforms(t), reopened)
	if err != nil {
		t.Fatalf("second registry.New() = %v", err)
	}

	got, err := second.Get("storhall")
	if err != nil {
		t.Fatalf("Get() after restart = %v", err)
	}
	if got.Settings.Settings.LocalExecutorCap != 80 {
		t.Errorf("LocalExecutorCap = %d, want the tuned 80 restored",
			got.Settings.Settings.LocalExecutorCap)
	}
	credentials, err := second.Credentials("storhall")
	if err != nil {
		t.Fatalf("Credentials() = %v", err)
	}
	if key, _ := credentials.Get("bearer_token"); key != "real-token" {
		t.Errorf("bearer_token = %q, want the stored key restored", key)
	}
}

// The persisted file holds access keys in the clear, so it must not be
// readable by anything else on the host.
func TestThePersistedFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	store, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() = %v", err)
	}
	reg, err := registry.New(platforms(t), store)
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}
	if _, err := reg.Create(context.Background(), simTarget("storhall"), config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("file mode is %v, want no access for group or other", mode)
	}
}

func policyLoop(when time.Time) policy.LoopState {
	return policy.LoopState{LastScaleUp: when}
}

// mustRoundTrip renders credentials the way the API does — redacted — and
// reads them back, which is exactly what a UI sends when it resubmits a target
// it has just read.
func mustRoundTrip(t *testing.T, bundle secret.Bundle) secret.Bundle {
	t.Helper()

	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	var back secret.Bundle
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}
	return back
}

// The runner cycles autonomous targets and skips driven ones, so a mode that is
// lost on restart is a target that silently stops being scaled: the service
// comes back healthy, the target is still listed with its settings and keys
// intact, and nothing ever cycles it again. Nothing else in the system reports
// this, which is what makes it worth a test of its own.
func TestAnAutonomousTargetIsStillAutonomousAfterARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")

	store, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() = %v", err)
	}
	first, err := registry.New(platforms(t), store)
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	target := platform.Target{
		ID: "storhall", Name: "Storhall", Kind: "watching",
		Mode: platform.ModeAutonomous,
	}
	if _, err := first.Create(context.Background(), target, config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}

	reopened, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopening NewFileStore() = %v", err)
	}
	second, err := registry.New(platforms(t), reopened)
	if err != nil {
		t.Fatalf("second registry.New() = %v", err)
	}

	got, err := second.Get("storhall")
	if err != nil {
		t.Fatalf("Get() after restart = %v", err)
	}
	if got.Target.Mode != platform.ModeAutonomous {
		t.Errorf("Mode = %q after a restart, want %q: the runner only cycles "+
			"autonomous targets, so this one has silently stopped being scaled",
			got.Target.Mode, platform.ModeAutonomous)
	}
}

// Mode was lost across restarts because the on-disk shape simply did not have
// the field, and every example test happened to use a target whose mode was
// already the default. So rather than naming fields one at a time, this
// compares the registry's whole view of a target before and after — and first
// insists the fixture leaves nothing at its zero value, because a field that
// is zero going in would survive being dropped.
func TestEveryFieldOfATargetSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")

	store, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() = %v", err)
	}
	first, err := registry.New(platforms(t), store)
	if err != nil {
		t.Fatalf("registry.New() = %v", err)
	}

	target := platform.Target{
		ID:          "storhall",
		Name:        "Storhall",
		Kind:        "watching",
		Mode:        platform.ModeAutonomous,
		Config:      map[string]string{"namespace": "mining", "deployment": "executors"},
		Credentials: secret.NewBundle(map[string]string{"bearer_token": "real-token"}),
	}

	fixture := reflect.ValueOf(target)
	for i := 0; i < fixture.NumField(); i++ {
		if fixture.Field(i).IsZero() {
			t.Fatalf("the fixture leaves Target.%s at its zero value, so this test "+
				"could not tell whether that field is persisted at all",
				fixture.Type().Field(i).Name)
		}
	}

	if _, err := first.Create(context.Background(), target, config.DefaultSettings()); err != nil {
		t.Fatalf("Create() = %v", err)
	}
	before, err := first.Get("storhall")
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}

	reopened, err := registry.NewFileStore(path)
	if err != nil {
		t.Fatalf("reopening NewFileStore() = %v", err)
	}
	second, err := registry.New(platforms(t), reopened)
	if err != nil {
		t.Fatalf("second registry.New() = %v", err)
	}
	after, err := second.Get("storhall")
	if err != nil {
		t.Fatalf("Get() after restart = %v", err)
	}

	if !reflect.DeepEqual(before.Target, after.Target) {
		t.Errorf("the target changed across a restart\n before: %+v\n  after: %+v",
			before.Target, after.Target)
	}
}
