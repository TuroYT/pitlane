package pool

import (
	"strings"
	"testing"
)

func TestRunnerImageFor(t *testing.T) {
	dev := runnerRepo + ":" + devRunnerTag
	for v, want := range map[string]string{
		"0.2.0":              runnerRepo + ":v0.2.0",
		"v0.2.0":             runnerRepo + ":v0.2.0",
		"v1.0.0-rc.1":        runnerRepo + ":v1.0.0-rc.1",
		"dev":                dev,
		"":                   dev,
		"v0.1.0-3-g1a2b3c4":  dev,
		"v0.1.0-dirty":       dev,
		"1a2b3c4":            dev,
		"v0.1.0-3-g1a2b3c4x": dev,
	} {
		if got := RunnerImageFor(v); got != want {
			t.Errorf("RunnerImageFor(%q) = %q, want %q", v, got, want)
		}
	}
}

func TestDefaultsFollowVersion(t *testing.T) {
	old := DefaultRunnerImage
	defer func() { DefaultRunnerImage = old }()
	SetVersion("0.3.0")
	for ref, want := range map[string]string{
		"":                                 runnerRepo + ":v0.3.0",
		legacyRunnerImage:                  runnerRepo + ":v0.3.0",
		runnerRepo + ":v0.1.0":             runnerRepo + ":v0.3.0",
		runnerRepo + ":v0.1.0@sha256:abcd": runnerRepo + ":v0.1.0@sha256:abcd",
		runnerRepo + ":custom":             runnerRepo + ":custom",
		OfficialRunnerImage:                OfficialRunnerImage,
		"example.com/runner:latest":        "example.com/runner:latest",
	} {
		c := Config{RunnerImage: ref}
		c.defaults()
		if c.RunnerImage != want {
			t.Errorf("RunnerImage %q -> %q, want %q", ref, c.RunnerImage, want)
		}
	}
	for ref, want := range map[string]string{"": DefaultDindImage, legacyDindImage: DefaultDindImage, "docker:27-dind": "docker:27-dind"} {
		c := Config{DindImage: ref}
		c.defaults()
		if c.DindImage != want {
			t.Errorf("DindImage %q -> %q, want %q", ref, c.DindImage, want)
		}
	}
}

func TestThirdPartyImagesPinned(t *testing.T) {
	for _, ref := range []string{OfficialRunnerImage, DefaultDindImage, binfmtImage, mirrorImage} {
		name, digest, ok := strings.Cut(ref, "@sha256:")
		if !ok || len(digest) != 64 || !strings.Contains(name, ":") {
			t.Errorf("%s is not pinned as name:tag@sha256:<digest>", ref)
		}
	}
}
