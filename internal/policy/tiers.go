package policy

import (
	"github.com/casperlundberg/autoscaler/internal/config"
	"github.com/casperlundberg/autoscaler/internal/domain"
)

// SplitTiers turns a total executor requirement into a per-tier plan.
//
// Local first, always. On-premise hardware is already bought and idle capacity
// there costs nothing, so every executor that fits locally is one the cloud is
// not billed for. Cloud is overflow — proven overflow, since the requirement
// it is covering came out of a simulation that showed the local tier alone
// would breach.
//
// Both caps are hard. The local one is physical: asking for more executors
// than there are machines produces pods that will never schedule. The cloud
// one is a spend ceiling, and an autoscaler that could exceed it on its own is
// not one an operator can safely leave running.
func SplitTiers(required int, settings config.Settings) domain.Plan {
	if required < 0 {
		required = 0
	}

	local := required
	if local > settings.LocalExecutorCap {
		local = settings.LocalExecutorCap
	}

	// The floor holds even with nothing to do. A warm executor answers the
	// next job immediately; an empty pool makes the first job after a quiet
	// spell pay a full coldstart, which is exactly when latency is most
	// visible.
	if local < settings.MinLocalExecutors {
		local = settings.MinLocalExecutors
	}

	cloud := required - local
	if cloud < 0 {
		cloud = 0
	}
	if cloud > settings.CloudExecutorCap {
		cloud = settings.CloudExecutorCap
	}

	return domain.Plan{LocalExecutors: local, CloudExecutors: cloud}
}
