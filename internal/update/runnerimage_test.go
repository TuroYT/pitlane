package update

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"
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

func TestRunnerVersionFromTarFailures(t *testing.T) {
	t.Parallel()
	archive := func(body string) *bytes.Reader {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "deps.json", Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
		_ = tw.Close()
		return bytes.NewReader(buf.Bytes())
	}
	if _, err := runnerVersionFromTar(archive(`{"targets":{}}`)); err == nil || !strings.Contains(err.Error(), "runner version not found") {
		t.Errorf("no version in the file: %v", err)
	}
	if _, err := runnerVersionFromTar(archive(`"Runner.Listener/2.3"`)); err == nil {
		t.Error("two-part version accepted")
	}
	// A truncated archive (the daemon dropped the connection) is an error too.
	whole := archive(`{"Runner.Listener/2.338.2":{}}`)
	b, _ := io.ReadAll(whole)
	if _, err := runnerVersionFromTar(bytes.NewReader(b[:520])); err == nil {
		t.Error("truncated archive accepted")
	}
	if v, err := runnerVersionFromTar(archive(`{"a":{"Runner.Listener/2.338.2":{}},"b":{"Runner.Listener/9.9.9":{}}}`)); err != nil || v != "2.338.2" {
		t.Errorf("first version wins: %q %v", v, err)
	}
}

func TestRunnerStatusEdges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	superseded := func(age time.Duration) []Release {
		return []Release{{Tag: "v2.2.0", PublishedAt: now.Add(-age)}}
	}
	tests := []struct {
		name string
		v    string
		rels []Release
		want string
	}{
		{"garbage version", "latest", superseded(time.Hour), RunnerUnknown},
		{"image newer than every release", "2.9.0", superseded(time.Hour), RunnerCurrent},
		{"only pre-releases are newer", "2.1.0", []Release{{Tag: "v2.2.0-rc.1", PublishedAt: now}, {Tag: "v2.2.0", Prerelease: true, PublishedAt: now}}, RunnerCurrent},
		{"unparsable tags are ignored", "2.1.0", []Release{{Tag: "nightly", PublishedAt: now.Add(-100 * 24 * time.Hour)}}, RunnerCurrent},
		{"exactly at the deadline is not expired", "2.1.0", superseded(RunnerGrace), RunnerExpiring},
		{"one second past", "2.1.0", superseded(RunnerGrace + time.Second), RunnerExpired},
		{"a week left is outdated", "2.1.0", superseded(RunnerGrace - 7*24*time.Hour), RunnerOutdated},
		{"just under a week left", "2.1.0", superseded(RunnerGrace - 7*24*time.Hour + time.Second), RunnerExpiring},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, _ := RunnerStatus(tc.v, tc.rels, now); got != tc.want {
				t.Errorf("%s, want %s", got, tc.want)
			}
		})
	}
	// The deadline counts from the oldest release newer than the image's.
	rels := []Release{{Tag: "v2.3.0", PublishedAt: now.Add(-time.Hour)}, {Tag: "v2.2.0", PublishedAt: now.Add(-20 * 24 * time.Hour)}}
	if _, d := RunnerStatus("2.1.0", rels, now); !d.Equal(now.Add(-20 * 24 * time.Hour).Add(RunnerGrace)) {
		t.Errorf("deadline %v", d)
	}
}
