package scaler

import (
	"testing"
	"time"
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
