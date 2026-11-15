package server

import (
	"errors"
	"math"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestReadiness(t *testing.T) {
	if ok, why := readiness(errors.New("dial unix"), []pool.SlotState{{Status: "ready"}}); ok || !strings.Contains(why, "docker") {
		t.Fatalf("docker down must not be ready: %v %q", ok, why)
	}
	if ok, _ := readiness(nil, []pool.SlotState{{Status: "starting"}, {Status: "error"}}); ok {
		t.Fatal("no usable slot must not be ready")
	}
	if ok, _ := readiness(nil, nil); ok {
		t.Fatal("no slot must not be ready")
	}
	for _, st := range []string{"ready", "busy", "cleaning"} {
		if ok, _ := readiness(nil, []pool.SlotState{{Status: "error"}, {Status: st}}); !ok {
			t.Fatalf("a %s slot must be ready", st)
		}
	}
}

func TestMetricsAuth(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{store: st}

	req := func(remote string, hdr map[string]string) bool {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return s.metricsAllowed(r)
	}
	if !req("127.0.0.1:5555", nil) || !req("[::1]:5555", nil) {
		t.Fatal("loopback must be allowed without a token")
	}
	if req("192.168.1.20:5555", nil) {
		t.Fatal("remote anonymous client allowed")
	}
	if req("127.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.9"}) {
		t.Fatal("request relayed by a local proxy treated as loopback")
	}
	if err := st.SetSecret(metricsTokenKey, "0123456789abcdef-token"); err != nil {
		t.Fatal(err)
	}
	if req("127.0.0.1:5555", nil) {
		t.Fatal("with a token, loopback without it must be refused")
	}
	if req("192.168.1.20:5555", map[string]string{"Authorization": "Bearer wrong"}) {
		t.Fatal("wrong token accepted")
	}
	if !req("192.168.1.20:5555", map[string]string{"Authorization": "Bearer 0123456789abcdef-token"}) {
		t.Fatal("valid token refused")
	}
}

func TestWriteMetrics(t *testing.T) {
	var b strings.Builder
	writeMetrics(&b, metricsData{
		Version:  "v1",
		DockerUp: true,
		Slots:    []pool.SlotState{{Status: "ready"}, {Status: "busy"}, {Status: "busy"}},
		Targets: []scaler.TargetState{{
			Target:  store.Target{Kind: "repo", Owner: "acme", Repo: `we"ird`, ScaleSetName: "pitlane"},
			Waiting: 2,
			Runners: []scaler.RunnerState{{Busy: true}, {Busy: false}, {Busy: true}},
		}},
		Results:   map[string]int64{"succeeded": 3, "failed": 1},
		Durations: []float64{10, 20, 30, 40},
		MemPeaks:  []float64{1 << 30, 2 << 30, 3 << 30},
		DiskFree:  5 << 30, DiskTotal: 100 << 30, DiskOK: true,
		ActionsCache: &cache.Stats{Entries: 3, Bytes: 1024, Hits: 5, Misses: 2, Saves: 3},
		SlotWipes:    map[string]int64{pool.WipeOwnerChange: 4, pool.WipeStrict: 2},
	})
	out := b.String()
	for _, want := range []string{
		`pitlane_info{version="v1"} 1`,
		"pitlane_docker_up 1",
		`pitlane_slots{status="busy"} 2`,
		`pitlane_slots{status="error"} 0`,
		"# TYPE pitlane_slot_wipes_total counter",
		`pitlane_slot_wipes_total{reason="owner_change"} 4`,
		`pitlane_slot_wipes_total{reason="strict"} 2`,
		`pitlane_slot_wipes_total{reason="unknown_owner"} 0`,
		`pitlane_runners{target="acme/we\"ird",label="pitlane",state="busy"} 2`,
		`pitlane_runners{target="acme/we\"ird",label="pitlane",state="idle"} 1`,
		`pitlane_jobs_waiting{target="acme/we\"ird",label="pitlane"} 2`,
		`pitlane_jobs_completed{result="failed"} 1`,
		"# TYPE pitlane_job_duration_seconds summary",
		`pitlane_job_duration_seconds{quantile="0.5"} 20`,
		`pitlane_job_duration_seconds{quantile="0.99"} 40`,
		"pitlane_job_duration_seconds_sum 100",
		"pitlane_job_duration_seconds_count 4",
		`pitlane_job_wait_seconds{quantile="0.5"} NaN`,
		"pitlane_job_wait_seconds_count 0",
		"# TYPE pitlane_job_memory_peak_bytes summary",
		`pitlane_job_memory_peak_bytes{quantile="0.5"} 2147483648`,
		`pitlane_job_memory_peak_bytes{quantile="0.9"} 3221225472`,
		"pitlane_job_memory_peak_bytes_count 3",
		"pitlane_disk_free_bytes 5368709120",
		"pitlane_actions_cache_bytes 1024",
		"pitlane_actions_cache_entries 3",
		`pitlane_actions_cache_requests_total{result="hit"} 5`,
		`pitlane_actions_cache_requests_total{result="save"} 3`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestQuantile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if q := quantile(v, 0.9); q != 9 {
		t.Fatal(q)
	}
	if q := quantile(v, 0.5); q != 5 {
		t.Fatal(q)
	}
	if !math.IsNaN(quantile(nil, 0.5)) {
		t.Fatal("empty must be NaN")
	}
}
