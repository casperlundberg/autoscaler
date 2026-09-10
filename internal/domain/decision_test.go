package domain_test

import (
	"testing"

	"github.com/casperlundberg/autoscaler/internal/domain"
)

func TestPlanTotal(t *testing.T) {
	p := domain.Plan{LocalExecutors: 6, CloudExecutors: 4}
	if got, want := p.Total(), 10; got != want {
		t.Errorf("Total() = %d, want %d", got, want)
	}
}

func TestClassifyAction(t *testing.T) {
	tests := []struct {
		name string
		prev domain.Plan
		next domain.Plan
		want domain.Action
	}{
		{
			name: "nothing moves",
			prev: domain.Plan{LocalExecutors: 4, CloudExecutors: 2},
			next: domain.Plan{LocalExecutors: 4, CloudExecutors: 2},
			want: domain.ActionMaintain,
		},
		{
			name: "local grows, cloud untouched",
			prev: domain.Plan{LocalExecutors: 4},
			next: domain.Plan{LocalExecutors: 7},
			want: domain.ActionScaleUp,
		},
		{
			name: "local shrinks, cloud untouched",
			prev: domain.Plan{LocalExecutors: 7},
			next: domain.Plan{LocalExecutors: 4},
			want: domain.ActionScaleDown,
		},
		{
			// Cloud coming online is the expensive, externally visible event.
			// It stays legible in the log even when local moved on the same
			// cycle, which it usually did.
			name: "cloud grows",
			prev: domain.Plan{LocalExecutors: 8, CloudExecutors: 0},
			next: domain.Plan{LocalExecutors: 8, CloudExecutors: 5},
			want: domain.ActionCloudBurst,
		},
		{
			name: "cloud grows alongside local",
			prev: domain.Plan{LocalExecutors: 4, CloudExecutors: 1},
			next: domain.Plan{LocalExecutors: 8, CloudExecutors: 5},
			want: domain.ActionCloudBurst,
		},
		{
			name: "cloud shrinks",
			prev: domain.Plan{LocalExecutors: 8, CloudExecutors: 5},
			next: domain.Plan{LocalExecutors: 8, CloudExecutors: 2},
			want: domain.ActionCloudRelease,
		},
		{
			name: "cloud shrinks alongside local",
			prev: domain.Plan{LocalExecutors: 8, CloudExecutors: 5},
			next: domain.Plan{LocalExecutors: 6, CloudExecutors: 2},
			want: domain.ActionCloudRelease,
		},
		{
			// Work moving back on-prem as the burst subsides is neither a
			// scale-up nor a teardown, and calling it either one makes the
			// decision log lie about what happened.
			name: "local grows as cloud is given back",
			prev: domain.Plan{LocalExecutors: 4, CloudExecutors: 6},
			next: domain.Plan{LocalExecutors: 8, CloudExecutors: 2},
			want: domain.ActionRebalance,
		},
		{
			name: "local shrinks as cloud takes over",
			prev: domain.Plan{LocalExecutors: 8, CloudExecutors: 0},
			next: domain.Plan{LocalExecutors: 6, CloudExecutors: 4},
			want: domain.ActionRebalance,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain.ClassifyAction(tt.prev, tt.next); got != tt.want {
				t.Errorf("ClassifyAction(%+v, %+v) = %q, want %q", tt.prev, tt.next, got, tt.want)
			}
		})
	}
}

func TestDecisionDelta(t *testing.T) {
	d := domain.Decision{
		Previous: domain.Plan{LocalExecutors: 4, CloudExecutors: 6},
		Plan:     domain.Plan{LocalExecutors: 8, CloudExecutors: 2},
	}

	local, cloud := d.Delta()
	if local != 4 || cloud != -4 {
		t.Errorf("Delta() = (%d, %d), want (4, -4)", local, cloud)
	}
}

func TestDecisionIsNoopOnlyWhenNothingChanges(t *testing.T) {
	same := domain.Plan{LocalExecutors: 4, CloudExecutors: 2}
	if !(domain.Decision{Previous: same, Plan: same}).IsNoop() {
		t.Error("IsNoop() = false for an unchanged plan, want true")
	}

	moved := domain.Decision{Previous: same, Plan: domain.Plan{LocalExecutors: 5, CloudExecutors: 2}}
	if moved.IsNoop() {
		t.Error("IsNoop() = true for a changed plan, want false")
	}
}
