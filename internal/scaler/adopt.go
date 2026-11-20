package scaler

import (
	"context"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
)

// untrustedLease turns the lease of a runner whose target is unknown into a
// strict one: its slot is rebuilt and handed to nobody before that.
func untrustedLease(l pool.Lease) pool.Lease {
	l.Strict, l.Owner, l.TargetID = true, pool.StrictOwner, 0
	return l
}

// stopUntracked stops a runner container left by a previous run that no
// target will adopt (target label missing or invalid, target deleted or
// disabled), the same way as a job over its time limit, then finishes it:
// logs archived, container removed (forcibly if the graceful stop failed),
// slot released as strict.
func (m *Manager) stopUntracked(r *runner, running bool) {
	if running {
		ctx, cancel := context.WithTimeout(context.Background(), timeoutStopGrace+time.Minute)
		if err := m.pool.StopRunner(ctx, r.containerID, timeoutStopGrace); err != nil {
			m.log.Error("stop untracked runner, removing it forcibly", "container", r.name, "id", r.containerID, "error", err)
		}
		cancel()
	}
	m.finishRunner(nil, r)
}
