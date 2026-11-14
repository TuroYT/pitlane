package update

import (
	"archive/tar"
	"bytes"
	"testing"
	"time"
)

func TestRunnerVersionFromTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := []byte(`{"targets":{".NETCoreApp,Version=v8.0/linux-x64":{"Runner.Listener/2.338.2":{"dependencies":{}}}}}`)
	_ = tw.WriteHeader(&tar.Header{Name: "Runner.Listener.deps.json", Mode: 0o644, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	v, err := runnerVersionFromTar(&buf)
	if err != nil || v != "2.338.2" {
		t.Fatalf("got %q %v", v, err)
	}
	if _, err := runnerVersionFromTar(bytes.NewReader(nil)); err == nil {
		t.Error("empty archive accepted")
	}
}

func TestRunnerStatus(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	rels := []Release{
		{Tag: "v2.340.0", PublishedAt: now.Add(-2 * 24 * time.Hour)},
		{Tag: "v2.341.0-beta", Prerelease: true, PublishedAt: now},
		{Tag: "v2.339.0", PublishedAt: now.Add(-25 * 24 * time.Hour)},
		{Tag: "v2.338.0", PublishedAt: now.Add(-40 * 24 * time.Hour)},
		{Tag: "v2.337.0", PublishedAt: now.Add(-60 * 24 * time.Hour)},
	}
	cases := map[string]string{
		"2.340.0": RunnerCurrent,
		"2.339.0": RunnerOutdated, // superseded 2 days ago
		"2.338.0": RunnerExpiring, // superseded 25 days ago: 5 days left
		"2.337.0": RunnerExpired,  // superseded 40 days ago
		"":        RunnerUnknown,
	}
	for v, want := range cases {
		got, deadline := RunnerStatus(v, rels, now)
		if got != want {
			t.Errorf("%s: %s, want %s", v, got, want)
		}
		if (want == RunnerCurrent || want == RunnerUnknown) != deadline.IsZero() {
			t.Errorf("%s: deadline %v", v, deadline)
		}
	}
	if s, _ := RunnerStatus("2.338.0", nil, now); s != RunnerUnknown {
		t.Errorf("no releases: %s", s)
	}
}
