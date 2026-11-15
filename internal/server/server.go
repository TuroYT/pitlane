// Package server exposes Pitlane's HTTP API, the GitHub App setup flow,
// GitHub login and the embedded web UI.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// Config of the HTTP server.
type Config struct {
	Version        string
	DataDir        string
	BaseURL        string // public URL of the UI, overrides the stored value
	PAT            string // optional personal access token instead of a GitHub App
	InsecureNoAuth bool   // disable login (development only)
	// Listen is the HTTP listen address: the base URL falls back to it.
	Listen string
	// TrustedProxies may set the Host and X-Forwarded-Proto headers the
	// base URL is derived from when none is configured (loopback always may).
	TrustedProxies []netip.Prefix
}

// Server is the HTTP server.
type Server struct {
	cfg   Config
	store *store.Store
	pool  *pool.Pool
	mgr   *scaler.Manager
	upd   http.Handler  // /api/updates (internal/update)
	cache *cache.Server // actions cache, nil when not serving
	log   *slog.Logger
	web   fs.FS
	hub   *hub

	setupToken string
	compare    compareCache
	// strictCheck replaces StrictReason in tests.
	strictCheck func(ctx context.Context, t store.Target) (string, error)
	// exchangeCode replaces gh.ExchangeCode (OAuth code -> login, user
	// token) in tests.
	exchangeCode func(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, string, error)
	// member replaces isMember (membership checks on GitHub) in tests.
	member func(ctx context.Context, userToken, login, org, team string) (bool, error)
	jobIDs sync.Map // runner name -> GitHub job ID (see handleGitHubJob)
	mu     sync.Mutex
	states map[string]time.Time // OAuth / manifest states
}

// New creates the server. web is the built UI (may be nil).
func New(cfg Config, st *store.Store, p *pool.Pool, log *slog.Logger, web fs.FS) *Server {
	s := &Server{cfg: cfg, store: st, pool: p, log: log, web: web, hub: newHub(), states: map[string]time.Time{}}
	return s
}

// SetManager wires the scaler manager (created after the server so it can notify the hub).
func (s *Server) SetManager(m *scaler.Manager) { s.mgr = m }

// SetActionsCache wires the actions cache server (its stats are shown in
// Settings and /metrics); nil when it is not serving.
func (s *Server) SetActionsCache(c *cache.Server) { s.cache = c }

// SetUpdates wires the update API handler, served under /api/updates.
func (s *Server) SetUpdates(h http.Handler) { s.upd = h }

// Notify signals UI subscribers that state changed.
func (s *Server) Notify() { s.hub.notify() }

// Credentials returns the GitHub credentials currently configured.
func (s *Server) Credentials() scaler.Credentials {
	if s.cfg.PAT != "" {
		return scaler.Credentials{PAT: s.cfg.PAT}
	}
	pem, _ := s.store.GetSecret("app_pem")
	return scaler.Credentials{AppClientID: s.store.Get("app_client_id", ""), AppPEM: pem}
}

func (s *Server) appConfigured() bool { return s.store.Get("app_client_id", "") != "" }

// baseURL is the public URL of the UI (OAuth callback, Secure cookies).
func (s *Server) baseURL(r *http.Request) string {
	if v := s.knownBaseURL(r); v != "" {
		return v
	}
	return s.listenURL()
}

// knownBaseURL is --base-url, else the URL saved by the setup, else, for a
// request from loopback or a trusted proxy, the one its Host and
// X-Forwarded-Proto headers give. Anyone else cannot choose it: "".
func (s *Server) knownBaseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	if v := s.store.Get("base_url", ""); v != "" {
		return v
	}
	if !s.trustedProxy(r) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ParseTrustedProxies parses a comma-separated list of IP addresses and
// CIDR prefixes (PITLANE_TRUSTED_PROXIES).
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: not an IP address or CIDR prefix", f)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// trustedProxy reports whether a request comes from loopback or from one of
// the configured trusted proxies.
func (s *Server) trustedProxy(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr := ap.Addr().Unmap()
	if addr.IsLoopback() {
		return true
	}
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// listenURL derives a URL from the listen address (the machine's host name
// when it listens on every interface), used when no base URL is known.
func (s *Server) listenURL() string {
	host, port, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil || port == "" {
		host, port = "", "8080"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host, _ = os.Hostname()
		if host == "" {
			host = "localhost"
		}
	}
	return "http://" + net.JoinHostPort(host, port)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// route is an HTTP route and the least role it needs ("" for a public
// route, which checks what it needs itself).
type route struct {
	pattern string
	role    string
	h       http.HandlerFunc
}

const (
	viewer   = store.RoleViewer
	operator = store.RoleOperator
	admin    = store.RoleAdmin
)

// routes lists every route. The role is the floor: a few handlers check
// more (an operator may only pause or resume a target).
func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", "", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) }},
		{"GET /readyz", "", s.handleReadyz},
		{"GET /metrics", "", s.handleMetrics}, // own authorization (token, session or loopback)

		// Public: status, setup and login.
		{"GET /api/status", "", s.handleStatus},
		{"POST /api/setup/unlock", "", s.handleSetupUnlock},
		{"POST /api/setup/manifest", "", s.handleSetupManifest}, // setup token, then admins only
		{"GET /setup/callback", "", s.handleSetupCallback},
		{"GET /auth/login", "", s.handleLogin},
		{"GET /auth/callback", "", s.handleAuthCallback},
		{"POST /api/logout", "", s.handleLogout},

		// Read only.
		{"GET /api/state", viewer, s.handleState},
		{"GET /api/events", viewer, s.handleEvents},
		{"GET /api/jobs", viewer, s.handleJobs},
		{"GET /api/jobs/repos", viewer, s.handleJobRepos},
		{"GET /api/jobs/memory", viewer, s.handleJobMemory},
		{"GET /api/runners/{name}/logs", viewer, s.handleLogs},
		{"GET /api/runners/{name}/job-log", viewer, s.handleJobLog},
		{"GET /api/runners/{name}/github", viewer, s.handleGitHubJob},
		{"GET /api/compare", viewer, s.handleCompare},

		// Operators: pause or resume targets (checked in the handler), updates
		// status, update check, runner image pull.
		{"PATCH /api/targets/{id}", operator, s.handleUpdateTarget},
		{"GET /api/updates", operator, s.handleUpdates},
		{"POST /api/updates/check", operator, s.handleUpdates},
		{"POST /api/updates/pull-runner", operator, s.handleUpdates},

		// Admins.
		{"GET /api/installations", admin, s.handleInstallations},
		{"POST /api/targets", admin, s.handleCreateTarget},
		{"DELETE /api/targets/{id}", admin, s.handleDeleteTarget},
		{"GET /api/settings", admin, s.handleGetSettings},
		{"PUT /api/settings", admin, s.handlePutSettings},
		{"/api/updates", admin, s.handleUpdates},  // anything else under /api/updates:
		{"/api/updates/", admin, s.handleUpdates}, // apply, preferences
		{"GET /api/users", admin, s.handleUsers},
		{"PUT /api/users/{login}", admin, s.handlePutUser},
		{"DELETE /api/users/{login}", admin, s.handleDeleteUser},
		{"POST /api/access-rules", admin, s.handlePutRule},
		{"DELETE /api/access-rules/{id}", admin, s.handleDeleteRule},
		{"GET /api/audit", admin, s.handleAudit},

		{"/", "", s.handleWeb},
	}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler(nil) }

// handler builds the HTTP handler; stub, in tests, replaces each route's
// handler (the role checks stay).
func (s *Server) handler(stub func(route) http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		h := rt.h
		if stub != nil {
			h = stub(rt)
		}
		if rt.role == "" {
			mux.HandleFunc(rt.pattern, h)
		} else {
			mux.Handle(rt.pattern, s.require(rt.role, h))
		}
	}
	return s.csrf(mux)
}

// csrf rejects state-changing API calls that do not carry the custom header
// set by the UI (browsers cannot send it cross-site without a CORS preflight).
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/") &&
			r.Header.Get("X-Pitlane") != "1" {
			http.Error(w, "missing X-Pitlane header", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------------------ compare

type compareRow struct {
	Repo          string  `json:"repo"`
	Workflow      string  `json:"workflow"`
	Job           string  `json:"job"`
	HostedMedian  float64 `json:"hostedMedian"`
	HostedCount   int     `json:"hostedCount"`
	PitlaneMedian float64 `json:"pitlaneMedian"`
	PitlaneCount  int     `json:"pitlaneCount"`
}

type compareCache struct {
	mu   sync.Mutex
	at   time.Time
	rows []compareRow
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[len(v)/2]
}

// errAppKey is a GitHub App private key that cannot be read back.
var errAppKey = errors.New("cannot decrypt app_pem, the GitHub App private key (was master.key replaced or lost?)")

// appClient returns a client authenticated as the GitHub App saved by the
// setup. A key that cannot be decrypted (master.key replaced or lost) is
// reported as such, not as an unparsable key.
func (s *Server) appClient() (*gh.AppClient, error) {
	pem, err := s.store.GetSecret("app_pem")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppKey, err)
	}
	appID, err := strconv.ParseInt(s.store.Get("app_id", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stored GitHub App ID: %w", err)
	}
	return gh.NewAppClient(appID, pem)
}

// token returns a GitHub token able to read Actions data of a target.
func (s *Server) token(ctx context.Context, t store.Target) (string, error) {
	if s.cfg.PAT != "" {
		return s.cfg.PAT, nil
	}
	client, err := s.appClient()
	if err != nil {
		return "", err
	}
	return client.InstallationToken(ctx, t.InstallationID)
}

// GitHubRunners lists the self-hosted runners of a target through the REST
// API (used by the scaler to find orphan runners).
func (s *Server) GitHubRunners(ctx context.Context, t store.Target) ([]gh.Runner, error) {
	tok, err := s.token(ctx, t)
	if err != nil {
		return nil, err
	}
	repo := ""
	if t.Kind == "repo" {
		repo = t.Repo
	}
	return gh.ListRunners(ctx, tok, t.Owner, repo)
}

// handleCompare compares the median duration of each successful job on
// Pitlane with the same job on GitHub-hosted runners (from the Actions
// history of every enabled repository). Cached for 15 minutes.
func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	c := &s.compare
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Query().Get("refresh") != "1" && time.Since(c.at) < 15*time.Minute && c.rows != nil {
		writeJSON(w, c.rows)
		return
	}
	targets, err := s.store.Targets()
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "targets_list", "cannot list the targets", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	type key struct{ repo, wf, job string }
	type acc struct{ hosted, pitlane []float64 }
	groups := map[key]*acc{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range targets {
		if t.Kind != "repo" || !t.Enabled {
			continue
		}
		wg.Add(1)
		go func(t store.Target) {
			defer wg.Done()
			tok, err := s.token(ctx, t)
			if err != nil {
				return
			}
			jobs, err := gh.RecentJobs(ctx, tok, t.Owner, t.Repo, 60)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, j := range jobs {
				k := key{t.FullName(), j.Workflow, j.Name}
				a := groups[k]
				if a == nil {
					a = &acc{}
					groups[k] = a
				}
				switch {
				case strings.HasPrefix(j.RunnerName, "pitlane-"):
					a.pitlane = append(a.pitlane, j.Seconds)
				case strings.HasPrefix(j.RunnerName, "GitHub Actions"):
					a.hosted = append(a.hosted, j.Seconds)
				}
			}
		}(t)
	}
	wg.Wait()
	rows := []compareRow{}
	for k, a := range groups {
		if len(a.pitlane) == 0 && len(a.hosted) == 0 {
			continue
		}
		rows = append(rows, compareRow{Repo: k.repo, Workflow: k.wf, Job: k.job,
			HostedMedian: median(a.hosted), HostedCount: len(a.hosted), PitlaneMedian: median(a.pitlane), PitlaneCount: len(a.pitlane)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Workflow+rows[i].Job < rows[j].Workflow+rows[j].Job
	})
	c.rows, c.at = rows, time.Now()
	writeJSON(w, rows)
}

// ----------------------------------------------------------------- settings

// Settings editable from the UI.
type Settings struct {
	Slots         int    `json:"slots"`
	RunnerImage   string `json:"runnerImage"`
	DindImage     string `json:"dindImage"`
	CPUShares     int64  `json:"cpuShares"`
	MemoryMB      int64  `json:"memoryMb"`
	CacheScope    string `json:"cacheScope"`
	Mirror        bool   `json:"mirror"`
	QEMU          bool   `json:"qemu"`
	KeepBuildGB   int    `json:"keepBuildGb"`
	MinFreeDiskGB int    `json:"minFreeDiskGb"`
	HistoryDays   int    `json:"historyDays"`
	BaseURL       string `json:"baseUrl"`
	// Bearer token required by /metrics. Never returned: GET only says
	// whether one is set; PUT changes it only when the field is present
	// ("" removes it).
	MetricsToken    *string `json:"metricsToken,omitempty"`
	MetricsTokenSet bool    `json:"metricsTokenSet"`
	// Actions cache served from the host (internal/cache). Its stats are
	// read-only, nil when the cache listener is not running.
	ActionsCache      bool         `json:"actionsCache"`
	ActionsCacheGB    int          `json:"actionsCacheGb"`
	ActionsCacheStats *cache.Stats `json:"actionsCacheStats,omitempty"`
	// Memory admission (pool.Admit) and its reserve; slots reserved for
	// strict jobs (pool.AutoStrictSlots = -1: one while a strict target
	// is enabled). StrictSlotsActive is what applies now (read-only).
	MemoryAdmission   bool  `json:"memoryAdmission"`
	MemoryReserveMB   int64 `json:"memoryReserveMb"`
	StrictSlots       int   `json:"strictSlots"`
	StrictSlotsActive int   `json:"strictSlotsActive"`
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.upd == nil {
		writeErr(w, http.StatusNotFound, "updates are not available")
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		s.upd.ServeHTTP(w, r)
		return
	}
	// Mutations go to the audit log, with the preferences saved.
	var detail string
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "cannot read the request body")
			return
		}
		if json.Valid(b) && len(b) > 2 {
			detail = string(b)
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
	s.upd.ServeHTTP(sw, r)
	if sw.code < 400 {
		s.audit(r, "updates."+strings.TrimPrefix(r.URL.Path, "/api/updates/"), "", detail)
	}
}

// statusWriter remembers the status code of a response.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// LoadPoolConfig reads the pool configuration from the store.
func LoadPoolConfig(st *store.Store, dataDir string) pool.Config {
	return pool.Config{
		StateFile:       filepath.Join(dataDir, "slots.json"),
		Slots:           st.GetInt("slots", DefaultSlots()),
		RunnerImage:     st.Get("runner_image", ""),
		DindImage:       st.Get("dind_image", ""),
		CPUShares:       int64(st.GetInt("cpu_shares", 0)),
		MemoryMB:        int64(st.GetInt("memory_mb", 0)),
		CacheScope:      st.Get("cache_scope", "owner"),
		Mirror:          st.Get("mirror", "true") == "true",
		NoQEMU:          st.Get("qemu", "true") != "true",
		KeepBuildGB:     st.GetInt("keep_build_gb", 20),
		MinFreeDiskGB:   st.GetInt("min_free_disk_gb", 20),
		ActionsCache:    st.Get("actions_cache", "true") == "true",
		ActionsCacheGB:  st.GetInt("actions_cache_gb", 20),
		MemoryAdmission: st.Get("memory_admission", "true") == "true",
		MemoryReserveMB: int64(st.GetInt("memory_reserve_mb", pool.DefaultMemoryReserveMB)),
		StrictSlots:     st.GetInt("strict_slots", pool.AutoStrictSlots),
	}
}

// DefaultSlots is half the CPUs, between 1 and 8.
func DefaultSlots() int { return min(8, max(1, runtime.NumCPU()/2)) }

func (s *Server) settings(r *http.Request) Settings {
	c := s.pool.Config()
	return Settings{
		Slots: c.Slots, RunnerImage: c.RunnerImage, DindImage: c.DindImage, CPUShares: c.CPUShares, MemoryMB: c.MemoryMB,
		CacheScope: c.CacheScope, Mirror: c.Mirror, QEMU: !c.NoQEMU, KeepBuildGB: c.KeepBuildGB, MinFreeDiskGB: c.MinFreeDiskGB,
		HistoryDays: s.store.GetInt("history_days", 30), BaseURL: s.knownBaseURL(r),
		MetricsTokenSet: s.store.Get(metricsTokenKey, "") != "",
		ActionsCache:    c.ActionsCache, ActionsCacheGB: c.ActionsCacheGB, ActionsCacheStats: s.actionsCacheStats(),
		MemoryAdmission: c.MemoryAdmission, MemoryReserveMB: c.MemoryReserveMB, StrictSlots: c.StrictSlots, StrictSlotsActive: s.pool.StrictSlots(),
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settings(r))
}

// settingKeys are the settings PUT /api/settings saves, in the order the
// audit log lists their changes.
var settingKeys = []string{"slots", "runner_image", "dind_image", "cpu_shares", "memory_mb", "cache_scope", "mirror", "qemu",
	"keep_build_gb", "min_free_disk_gb", "actions_cache", "actions_cache_gb", "memory_admission", "memory_reserve_mb", "strict_slots",
	"history_days", "base_url"}

// settingValues are the stored values of the settings in.
func settingValues(in Settings) map[string]string {
	if in.CacheScope != "owner" && in.CacheScope != "shared" {
		in.CacheScope = "owner"
	}
	kv := map[string]string{
		"slots": strconv.Itoa(in.Slots), "runner_image": strings.TrimSpace(in.RunnerImage), "dind_image": strings.TrimSpace(in.DindImage),
		"cpu_shares": strconv.FormatInt(max(0, in.CPUShares), 10), "memory_mb": strconv.FormatInt(max(0, in.MemoryMB), 10),
		"cache_scope": in.CacheScope, "mirror": strconv.FormatBool(in.Mirror), "qemu": strconv.FormatBool(in.QEMU), "keep_build_gb": strconv.Itoa(max(1, in.KeepBuildGB)),
		"history_days":     strconv.Itoa(max(1, in.HistoryDays)),
		"min_free_disk_gb": strconv.Itoa(max(0, in.MinFreeDiskGB)),
		"actions_cache":    strconv.FormatBool(in.ActionsCache), "actions_cache_gb": strconv.Itoa(max(1, in.ActionsCacheGB)),
		"memory_admission": strconv.FormatBool(in.MemoryAdmission), "memory_reserve_mb": strconv.FormatInt(max(0, in.MemoryReserveMB), 10),
		"strict_slots": strconv.Itoa(max(pool.AutoStrictSlots, in.StrictSlots)),
	}
	if b := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"); b != "" {
		kv["base_url"] = b
	}
	return kv
}

func (s *Server) actionsCacheStats() *cache.Stats {
	if s.cache == nil {
		return nil
	}
	st := s.cache.Stats()
	return &st
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	// Fields missing from the request keep their current value.
	cur := s.pool.Config()
	in := Settings{MinFreeDiskGB: cur.MinFreeDiskGB, ActionsCache: cur.ActionsCache, ActionsCacheGB: cur.ActionsCacheGB,
		MemoryAdmission: cur.MemoryAdmission, MemoryReserveMB: cur.MemoryReserveMB, StrictSlots: cur.StrictSlots}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badJSON(w)
		return
	}
	if in.Slots < 1 || in.Slots > 64 {
		writeErr(w, http.StatusBadRequest, "parallel jobs must be between 1 and 64")
		return
	}
	if in.StrictSlots < pool.AutoStrictSlots || in.StrictSlots >= in.Slots {
		writeErr(w, http.StatusBadRequest, "slots reserved for strict jobs must be -1 (auto) or between 0 and the number of parallel jobs minus one")
		return
	}
	if in.MemoryReserveMB < 0 {
		writeErr(w, http.StatusBadRequest, "the memory reserve cannot be negative")
		return
	}
	before := settingValues(s.settings(r))
	var metricsAudit string
	if in.MetricsToken != nil {
		tok := strings.TrimSpace(*in.MetricsToken)
		if tok != "" && len(tok) < 16 {
			writeErr(w, http.StatusBadRequest, "the metrics token must be at least 16 characters")
			return
		}
		var err error
		if tok == "" {
			if s.store.Get(metricsTokenKey, "") != "" {
				metricsAudit = "metrics_token removed"
			}
			err = s.store.Set(metricsTokenKey, "")
		} else {
			err = s.store.SetSecret(metricsTokenKey, tok)
			metricsAudit = "metrics_token changed"
		}
		if err != nil {
			s.internalErr(w, r, http.StatusInternalServerError, "settings_save", "cannot save the settings", err)
			return
		}
	}
	kv := settingValues(in)
	for k, v := range kv {
		if err := s.store.Set(k, v); err != nil {
			s.internalErr(w, r, http.StatusInternalServerError, "settings_save", "cannot save the settings", err)
			return
		}
	}
	if detail := strings.Join(slices.DeleteFunc([]string{changes(settingKeys, before, kv), metricsAudit}, func(x string) bool { return x == "" }), "; "); detail != "" {
		s.audit(r, "settings.update", "", detail)
	}
	go func() {
		if err := s.pool.Configure(context.Background(), LoadPoolConfig(s.store, s.cfg.DataDir)); err != nil {
			s.log.Error("apply settings", "error", err)
		}
		s.Notify()
	}()
	s.handleGetSettings(w, r)
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------- web

func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	if s.web == nil {
		http.Error(w, "UI not built", http.StatusNotFound)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if strings.HasPrefix(p, "api/") {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if _, err := fs.Stat(s.web, p); errors.Is(err, fs.ErrNotExist) {
		p = "index.html" // SPA routes
	}
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	// s.web is the embedded UI: nothing outside it can be served.
	http.ServeFileFS(w, r, s.web, p) //nolint:gosec // G703: embedded read-only FS
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// apiError is the body of an error response. Code and RequestID are only
// set for failures on Pitlane's side (see internalErr).
type apiError struct {
	Error     string `json:"error"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// internalErr answers a request that failed on Pitlane's side (database,
// GitHub API): the client gets msg, a stable code and a request ID, while
// err itself only goes to the log, under the same request ID. msg must not
// carry any error text.
func (s *Server) internalErr(w http.ResponseWriter, r *http.Request, status int, code, msg string, err error) {
	id := requestID(r)
	if s.log != nil {
		s.log.Error(msg, "code", code, "request_id", id, "method", r.Method, "path", r.URL.Path, "status", status, "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", id)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: fmt.Sprintf("%s (error %s, request %s)", msg, code, id), Code: code, RequestID: id})
}

// requestID is the X-Request-Id a reverse proxy set on the request, when it
// looks like one, else a new random ID.
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" && len(v) <= 64 && strings.IndexFunc(v, func(c rune) bool {
		return c != '-' && c != '_' && c != '.' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z')
	}) < 0 {
		return v
	}
	return randomHex(8)
}

// badJSON answers a request body that is not the JSON expected.
func badJSON(w http.ResponseWriter) { writeErr(w, http.StatusBadRequest, "invalid JSON body") }

type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]bool
}

func newHub() *hub { return &hub{subs: map[chan struct{}]bool{}} }

func (h *hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
