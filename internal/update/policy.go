package update

import (
	"fmt"
	"time"
)

// Window is a daily time range in local time, [Start, End) hours. End may be
// lower than Start for a window that crosses midnight (22 → 4).
type Window struct {
	Start int `json:"start"` // 0-23
	End   int `json:"end"`   // 0-23; equal to Start means the whole day
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool {
	h := t.Hour()
	switch {
	case w.Start == w.End:
		return true
	case w.Start < w.End:
		return h >= w.Start && h < w.End
	default:
		return h >= w.Start || h < w.End
	}
}

// Valid reports whether both hours are between 0 and 23.
func (w Window) Valid() bool { return w.Start >= 0 && w.Start < 24 && w.End >= 0 && w.End < 24 }

// AutoInput is everything the automatic update decision depends on.
type AutoInput struct {
	Enabled   bool
	Window    Window
	Now       time.Time
	Current   string // running version (main.version)
	Candidate Release
	Busy      bool // a job is running
	CanSelf   bool
}

// AutoDecision says whether to install Candidate now, and if not, why.
func AutoDecision(in AutoInput) (bool, string) {
	if !in.Enabled {
		return false, "automatic updates are off"
	}
	if !in.CanSelf {
		return false, "Pitlane cannot update itself in this installation"
	}
	cur, ok := ReleaseVersion(in.Current)
	if !ok {
		return false, "development build"
	}
	if in.Candidate.Tag == "" {
		return false, "no release known"
	}
	next, err := ParseVersion(in.Candidate.Tag)
	if err != nil {
		return false, err.Error()
	}
	if in.Candidate.Prerelease || in.Candidate.Draft || next.IsPrerelease() {
		return false, "pre-releases are never installed automatically"
	}
	if Compare(next, cur) <= 0 {
		return false, "up to date"
	}
	if next.Major != cur.Major {
		return false, fmt.Sprintf("%s is a new major version: install it manually", next)
	}
	if !in.Window.Contains(in.Now) {
		return false, fmt.Sprintf("outside the update window (%02d:00-%02d:00)", in.Window.Start, in.Window.End)
	}
	if in.Busy {
		return false, "a job is running"
	}
	return true, ""
}

// AutoCandidate is the highest stable release with the same major version as
// current, the only kind installed automatically.
func AutoCandidate(rels []Release, current string) (Release, bool) {
	cur, ok := ReleaseVersion(current)
	if !ok {
		return Release{}, false
	}
	r, _, ok := latest(rels, func(r Release, v Version) bool {
		return !r.Prerelease && !v.IsPrerelease() && v.Major == cur.Major && Compare(v, cur) > 0
	})
	return r, ok
}
