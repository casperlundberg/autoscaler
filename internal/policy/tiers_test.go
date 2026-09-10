package policy_test

import (
	"testing"

	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
	"github.com/casperlundberg/autoscaler/internal/policy"
)

func tierSettings(localCap, cloudCap, minLocal int) config.Settings {
	s := config.DefaultSettings()
	s.LocalExecutorCap = localCap
	s.CloudExecutorCap = cloudCap
	s.MinLocalExecutors = minLocal
	return s
}

func TestCapacityThatFitsOnPremiseNeverTouchesTheCloud(t *testing.T) {
	got := policy.SplitTiers(6, tierSettings(10, 100, 0))

	want := domain.Plan{LocalExecutors: 6, CloudExecutors: 0}
	if got != want {
		t.Errorf("SplitTiers(6) = %+v, want %+v", got, want)
	}
}

// Cloud is overflow, not a first resort: on-premise hardware is already paid
// for, so every executor that fits there is one the cloud is not billed for.
func TestOverflowAboveTheLocalCapGoesToTheCloud(t *testing.T) {
	got := policy.SplitTiers(14, tierSettings(10, 100, 0))

	want := domain.Plan{LocalExecutors: 10, CloudExecutors: 4}
	if got != want {
		t.Errorf("SplitTiers(14) = %+v, want %+v", got, want)
	}
}

func TestTheCloudCapIsAHardCeiling(t *testing.T) {
	got := policy.SplitTiers(100, tierSettings(10, 3, 0))

	want := domain.Plan{LocalExecutors: 10, CloudExecutors: 3}
	if got != want {
		t.Errorf("SplitTiers(100) = %+v, want %+v — the spend ceiling holds even "+
			"when the requirement is larger", got, want)
	}
}

func TestTheLocalFloorHoldsEvenWithNoWorkToDo(t *testing.T) {
	// A warm executor answers instantly; an empty pool pays a full coldstart
	// for the first job that arrives.
	got := policy.SplitTiers(0, tierSettings(10, 100, 2))

	want := domain.Plan{LocalExecutors: 2, CloudExecutors: 0}
	if got != want {
		t.Errorf("SplitTiers(0) = %+v, want %+v", got, want)
	}
}

func TestALocalCapOfZeroMakesTheTargetCloudOnly(t *testing.T) {
	got := policy.SplitTiers(7, tierSettings(0, 100, 0))

	want := domain.Plan{LocalExecutors: 0, CloudExecutors: 7}
	if got != want {
		t.Errorf("SplitTiers(7) = %+v, want %+v", got, want)
	}
}

func TestACloudCapOfZeroMakesTheTargetOnPremiseOnly(t *testing.T) {
	got := policy.SplitTiers(50, tierSettings(8, 0, 0))

	want := domain.Plan{LocalExecutors: 8, CloudExecutors: 0}
	if got != want {
		t.Errorf("SplitTiers(50) = %+v, want %+v", got, want)
	}
}

func TestANegativeRequirementIsTreatedAsNone(t *testing.T) {
	got := policy.SplitTiers(-5, tierSettings(10, 100, 1))

	want := domain.Plan{LocalExecutors: 1, CloudExecutors: 0}
	if got != want {
		t.Errorf("SplitTiers(-5) = %+v, want %+v", got, want)
	}
}
