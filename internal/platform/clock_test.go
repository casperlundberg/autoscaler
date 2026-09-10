package platform_test

import (
	"context"
	"testing"
	"time"

	"github.com/casperlundberg/autoscaler/internal/platform"
)

func TestCycleTimeDefaultsToTheWallClock(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)

	got := platform.CycleTime(context.Background())

	if got.Before(before) {
		t.Errorf("CycleTime() = %v, want roughly now", got)
	}
}

func TestCycleTimeReturnsTheRunsOwnClockWhenSet(t *testing.T) {
	simulated := time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC)

	got := platform.CycleTime(platform.WithCycleTime(context.Background(), simulated))

	if !got.Equal(simulated) {
		t.Errorf("CycleTime() = %v, want the run's clock %v", got, simulated)
	}
}

func TestAZeroCycleTimeIsIgnoredRatherThanObeyed(t *testing.T) {
	// A caller that forgot to set a timestamp must not put every adapter at
	// the zero year, where nothing has ever finished starting.
	got := platform.CycleTime(platform.WithCycleTime(context.Background(), time.Time{}))

	if got.IsZero() {
		t.Error("CycleTime() = zero time, want a fallback to the wall clock")
	}
}
