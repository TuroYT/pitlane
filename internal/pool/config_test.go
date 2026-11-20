package pool

import (
	"testing"

	"github.com/TuroYT/pitlane/internal/cache"
)

func TestConfigDefaults(t *testing.T) {
	var c Config
	c.MemoryReserveMB = -1
	c.StrictSlots = -5
	c.defaults()
	want := Config{
		Slots: 2, RunnerImage: DefaultRunnerImage, DindImage: DefaultDindImage, CacheScope: "owner",
		KeepBuildGB: 20, ImageOS: "ubuntu24", ActionsCacheGB: 20, ActionsCacheOwnerPct: cache.DefaultOwnerPercent,
		MemoryReserveMB: DefaultMemoryReserveMB, StrictSlots: AutoStrictSlots,
	}
	if c != want {
		t.Fatalf("defaults:\n got %+v\nwant %+v", c, want)
	}
}

func TestConfigDefaultsKeepSetValues(t *testing.T) {
	set := Config{
		Slots: 5, RunnerImage: "example/runner:1", DindImage: "example/dind:1", CacheScope: "shared",
		KeepBuildGB: 3, ImageOS: "ubuntu22", ActionsCacheGB: 7, ActionsCacheOwnerPct: 100,
		MemoryReserveMB: 0, StrictSlots: 2,
	}
	c := set
	c.defaults()
	if c != set {
		t.Fatalf("set values changed:\n got %+v\nwant %+v", c, set)
	}
}

func TestConfigDefaultsRules(t *testing.T) {
	cases := []struct {
		name string
		in   Config
		step func(*Config)
		ok   func(Config) bool
	}{
		{"legacy runner image", Config{RunnerImage: legacyRunnerImage}, (*Config).defaultImages,
			func(c Config) bool { return c.RunnerImage == DefaultRunnerImage }},
		{"release runner image follows the binary", Config{RunnerImage: runnerRepo + ":v0.1.0"}, (*Config).defaultImages,
			func(c Config) bool { return c.RunnerImage == DefaultRunnerImage }},
		{"legacy dind image", Config{DindImage: legacyDindImage}, (*Config).defaultImages,
			func(c Config) bool { return c.DindImage == DefaultDindImage }},
		{"owner share above 100", Config{ActionsCacheOwnerPct: 101}, (*Config).defaultCaches,
			func(c Config) bool { return c.ActionsCacheOwnerPct == cache.DefaultOwnerPercent }},
		{"negative slots", Config{Slots: -1}, (*Config).defaultSlots,
			func(c Config) bool { return c.Slots == 2 }},
		{"auto strict slots kept", Config{StrictSlots: AutoStrictSlots}, (*Config).defaultSlots,
			func(c Config) bool { return c.StrictSlots == AutoStrictSlots }},
		{"zero reserve kept", Config{MemoryReserveMB: 0}, (*Config).defaultMemory,
			func(c Config) bool { return c.MemoryReserveMB == 0 }},
	}
	for _, tc := range cases {
		c := tc.in
		tc.step(&c)
		if !tc.ok(c) {
			t.Errorf("%s: got %+v", tc.name, c)
		}
	}
}
