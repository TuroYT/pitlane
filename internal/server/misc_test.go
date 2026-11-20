package server

import (
	"crypto/tls"
	"math"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestFormatFloat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{math.NaN(), "NaN"}, {math.Inf(1), "+Inf"}, {math.Inf(-1), "-Inf"},
		{0, "0"}, {42, "42"}, {-7, "-7"}, {0.5, "0.5"}, {1.25e-7, "1.25e-07"},
		{1e15, "1e+15"}, {123456789012, "123456789012"},
	} {
		if got := formatFloat(tc.in); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDuplicateTarget(t *testing.T) {
	t.Parallel()
	org := store.Target{ID: 1, Kind: "org", Owner: "Acme", Enabled: true}
	repo := store.Target{ID: 2, Kind: "repo", Owner: "acme", Repo: "api", Enabled: true}
	for _, tc := range []struct {
		name string
		t    store.Target
		all  []store.Target
		want bool
	}{
		{"repo against its organization", repo, []store.Target{org}, true},
		{"organization against its repository", org, []store.Target{repo}, true},
		{"owners compare case-insensitively", repo, []store.Target{{ID: 1, Kind: "org", Owner: "ACME", Enabled: true}}, true},
		{"labels compare case-insensitively", store.Target{ID: 3, Kind: "repo", Owner: "acme", Repo: "x", Enabled: true, ScaleSetName: "Pitlane"}, []store.Target{org}, true},
		{"another label", store.Target{ID: 3, Kind: "repo", Owner: "acme", Repo: "x", Enabled: true, ScaleSetName: "gpu"}, []store.Target{org}, false},
		{"another owner", store.Target{ID: 3, Kind: "repo", Owner: "other", Repo: "x", Enabled: true}, []store.Target{org}, false},
		{"two repositories", repo, []store.Target{{ID: 3, Kind: "repo", Owner: "acme", Repo: "web", Enabled: true}}, false},
		{"the other is disabled", repo, []store.Target{{ID: 1, Kind: "org", Owner: "acme"}}, false},
		{"this one is disabled", store.Target{ID: 2, Kind: "repo", Owner: "acme", Repo: "api"}, []store.Target{org}, false},
		{"itself", org, []store.Target{org}, false},
	} {
		if _, got := duplicateTarget(tc.t, tc.all); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestHostStats(t *testing.T) {
	t.Parallel()
	var cpu cpuSampler
	h := readHost(t.TempDir(), &cpu)
	if h.CPUs < 1 || h.CPUPercent < 0 || h.CPUPercent > 100 {
		t.Fatalf("%+v", h)
	}
	if h.MemTotal <= 0 || h.MemAvail <= 0 || h.MemAvail > h.MemTotal || h.DiskTotal <= 0 || h.DiskFree < 0 || h.DiskFree > h.DiskTotal {
		t.Fatalf("memory or disk figures not plausible: %+v", h)
	}
	// A second sample less than a second later keeps the first answer, a later
	// one is a percentage again.
	cpu.sample()
	// Pretend the last sample is recent (in the future, so the test does not
	// depend on how fast it runs): the percentage is kept.
	cpu.mu.Lock()
	cpu.percent, cpu.lastWall = 42, time.Now().Add(time.Hour)
	cpu.mu.Unlock()
	if again := cpu.sample(); again != 42 {
		t.Errorf("sampled again within %v: got %v, want the previous 42", cpuSampleInterval, again)
	}
	cpu.mu.Lock()
	cpu.lastWall = time.Now().Add(-2 * time.Second)
	cpu.lastUse = 0
	cpu.mu.Unlock()
	if p := cpu.sample(); p < 0 || p > 100 {
		t.Errorf("percentage %v out of range", p)
	}
}

func TestBaseURLWithTLS(t *testing.T) {
	t.Parallel()
	s, _ := authServer(t, "alice")
	s.cfg.BaseURL = ""
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Host = "pitlane.example"
	r.TLS = &tls.ConnectionState{}
	if got := s.knownBaseURL(r); got != "https://pitlane.example" {
		t.Errorf("got %q", got)
	}
	// An IPv4-mapped IPv6 address of a trusted proxy counts as its IPv4 one.
	s.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[::ffff:10.1.2.3]:1"
	r.Host = "pitlane.example"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := s.knownBaseURL(r); got != "https://pitlane.example" {
		t.Errorf("mapped address: %q", got)
	}
	// A stored base URL beats what a proxy says; a configured one beats both.
	_ = s.store.Set("base_url", "https://stored.example")
	if got := s.knownBaseURL(r); got != "https://stored.example" {
		t.Errorf("stored: %q", got)
	}
	s.cfg.BaseURL = "https://configured.example///"
	if got := s.baseURL(r); got != "https://configured.example" {
		t.Errorf("configured: %q", got)
	}
}

func TestEnforcePublicReposStoreFailure(t *testing.T) {
	t.Parallel()
	s, _, dir, logs := serverAt(t)
	dropTable(t, dir, "targets")
	s.enforcePublicRepos(map[string]bool{"acme/api": false})
	if !strings.Contains(logs.String(), "load targets") {
		t.Errorf("not logged:\n%s", logs)
	}
}
