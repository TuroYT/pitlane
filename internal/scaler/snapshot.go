package scaler

import (
	"sort"

	"github.com/TuroYT/pitlane/internal/store"
)

// RunnerState is shown in the UI.
type RunnerState struct {
	Name      string `json:"name"`
	Slot      int    `json:"slot"`
	Busy      bool   `json:"busy"`
	JobName   string `json:"jobName"`
	Repo      string `json:"repo"`
	StartedAt int64  `json:"startedAt"`
	// Predicted is the memory reserved for its job (bytes).
	Predicted int64 `json:"predicted"`
}

// TargetState is shown in the UI.
type TargetState struct {
	store.Target
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Desired int    `json:"desired"`
	Waiting int    `json:"waiting"`
	// WaitReason says why jobs wait (pool.WaitSlot, WaitMemory,
	// WaitStrictSlot), "" when none does.
	WaitReason string        `json:"waitReason,omitempty"`
	Runners    []RunnerState `json:"runners"`
}

// Snapshot returns the live state of every target.
func (m *Manager) Snapshot() []TargetState {
	targets, err := m.store.Targets()
	if err != nil {
		m.log.Error("load targets", "error", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TargetState, 0, len(targets))
	for _, st := range targets {
		ts := TargetState{Target: st, Status: "disabled", Runners: []RunnerState{}}
		if t, ok := m.targets[st.ID]; ok {
			t.mu.Lock()
			ts.Status, ts.Error, ts.Desired, ts.Waiting, ts.WaitReason = t.status, t.err, t.desired, t.waiting, t.waitReason
			ts.ScaleSetID = t.scaleSetID
			for _, r := range t.runners {
				ts.Runners = append(ts.Runners, RunnerState{Name: r.name, Slot: r.lease.Slot, Busy: r.busy, JobName: r.jobName, Repo: r.repo, StartedAt: r.startedAt.Unix(), Predicted: r.predicted})
			}
			t.mu.Unlock()
			sort.Slice(ts.Runners, func(i, j int) bool { return ts.Runners[i].Slot < ts.Runners[j].Slot })
		} else if st.Enabled && !m.creds().Valid() {
			ts.Status = "no-credentials"
		}
		out = append(out, ts)
	}
	return out
}
