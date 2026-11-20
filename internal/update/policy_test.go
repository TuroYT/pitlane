package update

import (
	"strings"
	"testing"
	"time"
)

func at(h int) time.Time { return time.Date(2026, 11, 14, h, 30, 0, 0, time.Local) }

func TestWindow(t *testing.T) {
	cases := []struct {
		w    Window
		hour int
		want bool
	}{
		{Window{3, 5}, 3, true}, {Window{3, 5}, 4, true}, {Window{3, 5}, 5, false}, {Window{3, 5}, 2, false},
		{Window{22, 4}, 23, true}, {Window{22, 4}, 0, true}, {Window{22, 4}, 3, true}, {Window{22, 4}, 4, false}, {Window{22, 4}, 12, false},
		{Window{0, 0}, 13, true},
	}
	for _, c := range cases {
		if got := c.w.Contains(at(c.hour)); got != c.want {
			t.Errorf("%v contains %d:30 = %v, want %v", c.w, c.hour, got, c.want)
		}
	}
	if (Window{24, 1}).Valid() || (Window{1, -1}).Valid() || !(Window{23, 0}).Valid() {
		t.Error("Valid")
	}
}

func TestAutoDecision(t *testing.T) {
	base := AutoInput{Enabled: true, Window: Window{3, 5}, Now: at(3), Current: "0.2.0", Candidate: Release{Tag: "v0.2.1"}, CanSelf: true}
	if ok, why := AutoDecision(base); !ok {
		t.Fatalf("patch in window refused: %s", why)
	}
	minor := base
	minor.Current, minor.Candidate = "1.2.0", Release{Tag: "v1.3.0"}
	if ok, why := AutoDecision(minor); !ok {
		t.Errorf("minor refused after 1.0: %s", why)
	}
	cases := map[string]func(*AutoInput){
		"off":                     func(in *AutoInput) { in.Enabled = false },
		"outside window":          func(in *AutoInput) { in.Now = at(12) },
		"a job is running":        func(in *AutoInput) { in.Busy = true },
		"new major":               func(in *AutoInput) { in.Current, in.Candidate = "1.4.0", Release{Tag: "v2.0.0"} },
		"new minor in 0.x":        func(in *AutoInput) { in.Candidate = Release{Tag: "v0.3.0"} },
		"pre-release flag":        func(in *AutoInput) { in.Candidate.Prerelease = true },
		"pre-release tag":         func(in *AutoInput) { in.Candidate = Release{Tag: "v0.3.0-rc.1"} },
		"older":                   func(in *AutoInput) { in.Candidate = Release{Tag: "v0.1.9"} },
		"same":                    func(in *AutoInput) { in.Candidate = Release{Tag: "v0.2.0"} },
		"dev build":               func(in *AutoInput) { in.Current = "dev" },
		"git describe":            func(in *AutoInput) { in.Current = "v0.2.0-4-gabcdef1" },
		"cannot self-update":      func(in *AutoInput) { in.CanSelf = false },
		"no release":              func(in *AutoInput) { in.Candidate = Release{} },
		"draft":                   func(in *AutoInput) { in.Candidate.Draft = true },
		"candidate not a version": func(in *AutoInput) { in.Candidate = Release{Tag: "nightly"} },
	}
	for name, mut := range cases {
		in := base
		mut(&in)
		if ok, why := AutoDecision(in); ok || why == "" {
			t.Errorf("%s: allowed (%q)", name, why)
		}
	}
}

func TestAutoCandidate(t *testing.T) {
	rels := []Release{
		{Tag: "v2.0.0"}, {Tag: "v1.5.0-rc.1", Prerelease: true}, {Tag: "v1.4.2"}, {Tag: "v1.4.1"}, {Tag: "v1.3.0"}, {Tag: "nightly"},
	}
	if r, ok := AutoCandidate(rels, "1.3.0"); !ok || r.Tag != "v1.4.2" {
		t.Errorf("got %v %v, want v1.4.2", r.Tag, ok)
	}
	if _, ok := AutoCandidate(rels, "1.4.2"); ok {
		t.Error("candidate when up to date within the major")
	}
	zero := []Release{{Tag: "v0.3.0"}, {Tag: "v0.2.3"}, {Tag: "v0.2.1"}}
	if r, ok := AutoCandidate(zero, "0.2.0"); !ok || r.Tag != "v0.2.3" {
		t.Errorf("0.x: got %v %v, want the latest patch v0.2.3", r.Tag, ok)
	}
	if _, ok := AutoCandidate(rels, "dev"); ok {
		t.Error("candidate for a dev build")
	}
	if r, v, ok := LatestStable(rels); !ok || r.Tag != "v2.0.0" || v.Major != 2 {
		t.Errorf("latest stable %v", r.Tag)
	}
	if ok, why := AutoDecision(AutoInput{Enabled: true, Current: "1.3.0", Candidate: Release{Tag: "v2.0.0"}, CanSelf: true}); ok || !strings.Contains(why, "major") {
		t.Errorf("new major: %v %q", ok, why)
	}
}
