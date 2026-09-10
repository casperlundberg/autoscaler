package app_test

import (
	"strings"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/app"
	"github.com/casperlundberg/autoscaler/internal/platform"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestTheDefaultsAreEnoughToStart(t *testing.T) {
	got, err := app.LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}

	if got.Address == "" {
		t.Error("Address is empty")
	}
	if got.Tick <= 0 {
		t.Errorf("Tick = %v, want a positive default", got.Tick)
	}
}

func TestEverySettingCanBeOverridden(t *testing.T) {
	got, err := app.LoadConfig(env(map[string]string{
		"AUTOSCALER_ADDRESS":    ":9000",
		"AUTOSCALER_API_TOKEN":  "s3cr3t",
		"AUTOSCALER_STATE_FILE": "/var/lib/autoscaler/targets.json",
		"AUTOSCALER_TICK":       "5s",
		"AUTOSCALER_LOG_LEVEL":  "debug",
	}))
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}

	if got.Address != ":9000" {
		t.Errorf("Address = %q", got.Address)
	}
	if got.Token != "s3cr3t" {
		t.Errorf("Token was not read")
	}
	if got.StateFile != "/var/lib/autoscaler/targets.json" {
		t.Errorf("StateFile = %q", got.StateFile)
	}
	if got.Tick != 5*time.Second {
		t.Errorf("Tick = %v, want 5s", got.Tick)
	}
}

func TestAnUnreadableDurationIsRefusedAtStartup(t *testing.T) {
	_, err := app.LoadConfig(env(map[string]string{"AUTOSCALER_TICK": "soon"}))

	if err == nil || !strings.Contains(err.Error(), "AUTOSCALER_TICK") {
		t.Errorf("LoadConfig() = %v, want the bad variable named", err)
	}
}

// Failing at startup is much better than starting and refusing every request:
// a container that will not start is visible, one that starts and 401s is not.
func TestWithNoStateFileTheServiceStillStarts(t *testing.T) {
	got, err := app.LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if got.StateFile != "" {
		t.Errorf("StateFile = %q, want empty so targets are kept in memory", got.StateFile)
	}
}

// Every platform the service supports has to be registered, or a target of
// that kind is rejected with "no adapter" at a point where nothing can be done
// about it.
func TestEveryPlatformIsRegistered(t *testing.T) {
	platforms, err := app.Platforms()
	if err != nil {
		t.Fatalf("Platforms() = %v", err)
	}

	for _, kind := range []platform.Kind{
		platform.KindSimulation,
		platform.KindKubernetes,
		platform.KindColonyOSPods,
		platform.KindColonyOSContainers,
	} {
		if _, err := platforms.Get(kind); err != nil {
			t.Errorf("Get(%q) = %v", kind, err)
		}
	}
}

func TestEveryRegisteredPlatformDescribesItself(t *testing.T) {
	platforms, err := app.Platforms()
	if err != nil {
		t.Fatalf("Platforms() = %v", err)
	}

	for _, schema := range platforms.Schemas() {
		if schema.Summary == "" {
			t.Errorf("platform %q has no summary; the UI renders it as the form's "+
				"explanation", schema.Kind)
		}
		for _, field := range append(append([]platform.Field{}, schema.Config...), schema.Credentials...) {
			if field.Label == "" {
				t.Errorf("platform %q field %q has no label", schema.Kind, field.Name)
			}
		}
	}
}
