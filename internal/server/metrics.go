package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
)

// ------------------------------------------------------------------ readyz

// readiness reports whether Pitlane can run jobs: Docker answers and at least
// one slot is up. A slot running or cleaning up after a job counts: a fully
// busy Pitlane is healthy.
func readiness(pingErr error, slots []pool.SlotState) (bool, string) {
	if pingErr != nil {
		return false, "docker unreachable: " + pingErr.Error()
	}
	for _, s := range slots {
		switch s.Status {
		case "ready", "busy", "cleaning":
			return true, ""
		}
	}
	return false, "no slot is ready"
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	err := s.pool.Ping(ctx)
	ok, reason := readiness(err, s.pool.Slots())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: %s\n", reason)
		return
	}
	io.WriteString(w, "ready\n")
}

// ----------------------------------------------------------------- metrics

const metricsTokenKey = "metrics_token"

// isLoopback reports whether the request comes straight from this host. A
// request relayed by a local reverse proxy (forwarding headers) does not
// count: the real client is elsewhere.
func isLoopback(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// metricsAllowed: with a metrics token configured, only that bearer token is
// accepted; otherwise signed-in users and loopback clients.
func (s *Server) metricsAllowed(r *http.Request) bool {
	tok, err := s.store.GetSecret(metricsTokenKey)
	if err != nil {
		return false // fail closed: a token exists but cannot be read
	}
	if tok != "" {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(tok)) == 1
	}
	if _, ok := s.currentUser(r); ok {
		return true
	}
	return isLoopback(r)
}

// metricsData is everything /metrics reports.
type metricsData struct {
	Version   string
	StartTime time.Time // zero when unknown
	DockerUp  bool
	Slots     []pool.SlotState
	Targets   []scaler.TargetState
	Results   map[string]int64
	Durations []float64 // seconds, jobs finished in the window
	Waits     []float64 // seconds, jobs started in the window
	MemPeaks  []float64 // bytes, measured jobs finished in the last 7 days
	DiskTotal int64
	DiskFree  int64
	DiskOK    bool
	// ActionsCache is nil when the actions cache server is not running.
	ActionsCache *cache.Stats
	// SlotWipes counts slot rebuilds before a job since start, by reason.
	SlotWipes map[string]int64
	Memory    pool.MemoryState
}

const metricsWindow = 24 * time.Hour

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAllowed(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pitlane"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	d := metricsData{Version: s.cfg.Version, StartTime: s.started, Slots: s.pool.Slots(), SlotWipes: s.pool.SlotWipes(), Memory: s.pool.Memory()}
	err := s.pool.Ping(ctx)
	d.DockerUp = err == nil
	if s.mgr != nil {
		d.Targets = s.mgr.Snapshot()
	}
	d.Results, _ = s.store.JobResultCounts(ctx)
	d.Durations, d.Waits, _ = s.store.JobTimings(ctx, time.Now().Add(-metricsWindow).Unix())
	d.MemPeaks, _ = s.store.JobMemoryPeaks(ctx, time.Now().Add(-memoryWindow).Unix())
	if total, free, err := s.pool.DiskSpace(); err == nil {
		d.DiskTotal, d.DiskFree, d.DiskOK = total, free, true
	}
	if s.cache != nil {
		st := s.cache.Stats()
		d.ActionsCache = &st
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	writeMetrics(w, d)
}

// promWriter writes the Prometheus text exposition format.
type promWriter struct{ w io.Writer }

func (p promWriter) head(name, typ, help string) {
	fmt.Fprintf(p.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func (p promWriter) sample(name string, labels []string, v float64) {
	var b strings.Builder
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escapeLabel(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(p.w, "%s %s\n", b.String(), formatFloat(v))
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case v == math.Trunc(v) && math.Abs(v) < 1e15:
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

// quantile returns the nearest-rank quantile of sorted values (NaN if empty).
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func (p promWriter) summary(name, help string, values []float64) {
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	p.head(name, "summary", help)
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	for _, q := range []float64{0.5, 0.9, 0.99} {
		p.sample(name, []string{"quantile", formatFloat(q)}, quantile(v, q))
	}
	p.sample(name+"_sum", nil, sum)
	p.sample(name+"_count", nil, float64(len(v)))
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func writeMetrics(w io.Writer, d metricsData) {
	p := promWriter{w}
	p.head("pitlane_info", "gauge", "Pitlane version.")
	p.sample("pitlane_info", []string{"version", d.Version}, 1)
	if !d.StartTime.IsZero() {
		p.head("pitlane_start_time_seconds", "gauge", "Start time of the Pitlane process, in seconds since the Unix epoch.")
		p.sample("pitlane_start_time_seconds", nil, float64(d.StartTime.Unix()))
	}
	p.head("pitlane_docker_up", "gauge", "1 if the Docker daemon answers.")
	p.sample("pitlane_docker_up", nil, boolFloat(d.DockerUp))

	p.head("pitlane_slots", "gauge", "Slots by status.")
	counts := map[string]int{}
	for _, st := range []string{"starting", "ready", "busy", "cleaning", "error", "retiring"} {
		counts[st] = 0
	}
	for _, s := range d.Slots {
		counts[s.Status]++
	}
	statuses := make([]string, 0, len(counts))
	for st := range counts {
		statuses = append(statuses, st)
	}
	sort.Strings(statuses)
	for _, st := range statuses {
		p.sample("pitlane_slots", []string{"status", st}, float64(counts[st]))
	}

	p.head("pitlane_slot_wipes_total", "counter", "Slots rebuilt empty before a job since start (lost warm cache), by reason.")
	for _, r := range pool.WipeReasons {
		p.sample("pitlane_slot_wipes_total", []string{"reason", r}, float64(d.SlotWipes[r]))
	}

	p.head("pitlane_runners", "gauge", "Runners per target and state.")
	for _, t := range d.Targets {
		busy := 0
		for _, r := range t.Runners {
			if r.Busy {
				busy++
			}
		}
		name := t.FullName()
		p.sample("pitlane_runners", []string{"target", name, "label", t.ScaleSetName, "state", "busy"}, float64(busy))
		p.sample("pitlane_runners", []string{"target", name, "label", t.ScaleSetName, "state", "idle"}, float64(len(t.Runners)-busy))
	}
	p.head("pitlane_jobs_waiting", "gauge", "Jobs assigned by GitHub that wait, per target and reason (slot, memory, strict_slot).")
	for _, t := range d.Targets {
		reason := t.WaitReason
		if reason == "" {
			reason = pool.WaitSlot
		}
		for _, r := range pool.WaitReasons {
			n := 0
			if r == reason {
				n = t.Waiting
			}
			p.sample("pitlane_jobs_waiting", []string{"target", t.FullName(), "label", t.ScaleSetName, "reason", r}, float64(n))
		}
	}

	p.head("pitlane_memory_budget_bytes", "gauge", "Memory jobs may use: host memory minus the reserve (0 when unknown).")
	p.sample("pitlane_memory_budget_bytes", nil, float64(d.Memory.Budget))
	p.head("pitlane_memory_predicted_bytes", "gauge", "Memory reserved for the running jobs by prediction.")
	p.sample("pitlane_memory_predicted_bytes", nil, float64(d.Memory.Predicted))
	p.head("pitlane_memory_committed_bytes", "gauge", "Memory the admission counts for the running jobs: per job the larger of its prediction and its use.")
	p.sample("pitlane_memory_committed_bytes", nil, float64(d.Memory.Committed))
	p.head("pitlane_memory_admission_enabled", "gauge", "1 when jobs are admitted by predicted memory.")
	p.sample("pitlane_memory_admission_enabled", nil, boolFloat(d.Memory.Enabled))

	p.head("pitlane_jobs_completed", "gauge", "Finished jobs in the job history by result (history retention applies).")
	results := make([]string, 0, len(d.Results))
	for r := range d.Results {
		results = append(results, r)
	}
	sort.Strings(results)
	for _, r := range results {
		p.sample("pitlane_jobs_completed", []string{"result", r}, float64(d.Results[r]))
	}

	p.summary("pitlane_job_duration_seconds", "Run time of the jobs finished in the last 24 hours.", d.Durations)
	p.summary("pitlane_job_wait_seconds", "Queue time (queued to started) of the jobs started in the last 24 hours.", d.Waits)
	p.summary("pitlane_job_memory_peak_bytes", "Peak memory (runner plus slot, page cache included) of the measured jobs finished in the last 7 days.", d.MemPeaks)

	if d.DiskOK {
		p.head("pitlane_disk_free_bytes", "gauge", "Free bytes on Docker's filesystem.")
		p.sample("pitlane_disk_free_bytes", nil, float64(d.DiskFree))
		p.head("pitlane_disk_total_bytes", "gauge", "Size of Docker's filesystem in bytes.")
		p.sample("pitlane_disk_total_bytes", nil, float64(d.DiskTotal))
	}
	if c := d.ActionsCache; c != nil {
		p.head("pitlane_actions_cache_bytes", "gauge", "Bytes stored by the actions cache.")
		p.sample("pitlane_actions_cache_bytes", nil, float64(c.Bytes))
		p.head("pitlane_actions_cache_entries", "gauge", "Entries stored by the actions cache.")
		p.sample("pitlane_actions_cache_entries", nil, float64(c.Entries))
		p.head("pitlane_actions_cache_requests_total", "counter", "Actions cache restores (hit or miss) and saves since start.")
		p.sample("pitlane_actions_cache_requests_total", []string{"result", "hit"}, float64(c.Hits))
		p.sample("pitlane_actions_cache_requests_total", []string{"result", "miss"}, float64(c.Misses))
		p.sample("pitlane_actions_cache_requests_total", []string{"result", "save"}, float64(c.Saves))
		p.head("pitlane_actions_cache_evictions_total", "counter", "Actions cache entries evicted since start.")
		p.sample("pitlane_actions_cache_evictions_total", nil, float64(c.Evictions))
	}
}
