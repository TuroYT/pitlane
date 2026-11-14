package scaler

import (
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestTimedOut(t *testing.T) {
	now := time.Now()
	limit := 360 * time.Minute
	old := now.Add(-7 * time.Hour)
	for _, tc := range []struct {
		name  string
		r     runner
		limit time.Duration
		want  bool
	}{
		{"busy too long", runner{busy: true, busySince: old}, limit, true},
		{"busy within limit", runner{busy: true, busySince: now.Add(-time.Hour)}, limit, false},
		{"unlimited", runner{busy: true, busySince: old}, 0, false},
		{"idle warm runner", runner{busy: false, startedAt: old}, limit, false},
		{"already exited", runner{busy: true, busySince: old, exited: true}, limit, false},
		{"already killed", runner{busy: true, busySince: old, killed: true}, limit, false},
		{"start unknown", runner{busy: true}, limit, false},
	} {
		if got := timedOut(&tc.r, tc.limit, now); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestOrphanDecision(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).Unix()
	rec := store.RegisteredRunner{ID: 5, TargetID: 1, Name: "pitlane-dead", CreatedAt: old}
	offline := map[int64]gh.Runner{5: {ID: 5, Name: "pitlane-dead", Status: "offline"}}
	for _, tc := range []struct {
		name   string
		rec    store.RegisteredRunner
		live   map[string]bool
		remote map[int64]gh.Runner
		want   orphanAction
	}{
		{"offline idle without container", rec, nil, offline, orphanRemove},
		{"container still exists", rec, map[string]bool{"pitlane-dead": true}, offline, orphanKeep},
		{"recently registered", store.RegisteredRunner{ID: 5, Name: "pitlane-dead", CreatedAt: now.Unix()}, nil, offline, orphanKeep},
		{"online", rec, nil, map[int64]gh.Runner{5: {ID: 5, Name: "pitlane-dead", Status: "online"}}, orphanKeep},
		{"busy", rec, nil, map[int64]gh.Runner{5: {ID: 5, Name: "pitlane-dead", Status: "offline", Busy: true}}, orphanKeep},
		{"unknown status", rec, nil, map[int64]gh.Runner{5: {ID: 5, Name: "pitlane-dead", Status: ""}}, orphanKeep},
		{"id reused by another runner", rec, nil, map[int64]gh.Runner{5: {ID: 5, Name: "someone-else", Status: "offline"}}, orphanKeep},
		{"not listed", rec, nil, map[int64]gh.Runner{}, orphanVerify},
	} {
		if got := orphanDecision(tc.rec, now, tc.live, tc.remote); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestJobKeepsWorkflowRef(t *testing.T) {
	s := &scaler{t: &target{t: store.Target{ID: 3}}}
	j := s.job(scaleset.JobMessageBase{JobID: "j1", WorkflowRunID: 9, OwnerName: "acme", RepositoryName: "api",
		JobWorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main", EventName: "push"})
	if j.TargetID != 3 || j.WorkflowRef != "ci.yml" || j.WorkflowFullRef != "acme/api/.github/workflows/ci.yml@refs/heads/main" ||
		j.Event != "push" || j.RunID != 9 || j.Slot != -1 {
		t.Fatalf("unexpected job: %+v", j)
	}
}
