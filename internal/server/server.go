// Package server exposes Pitlane's HTTP API, the GitHub App setup flow,
// GitHub login and the embedded web UI.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	upd   http.Handler // /api/updates (internal/update)
	log   *slog.Logger
	web   fs.FS
	hub   *hub

	setupToken string
	compare    compareCache
	// strictCheck replaces StrictReason in tests.
	strictCheck func(ctx context.Context, t store.Target) (string, error)
	// exchangeCode replaces gh.ExchangeCode (OAuth code -> login) in tests.
	exchangeCode func(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, error)
	jobIDs       sync.Map // runner name -> GitHub job ID (see handleGitHubJob)
	mu           sync.Mutex
	states       map[string]time.Time // OAuth / manifest states
}

// New creates the server. web is the built UI (may be nil).
func New(cfg Config, st *store.Store, p *pool.Pool, log *slog.Logger, web fs.FS) *Server {
	s := &Server{cfg: cfg, store: st, pool: p, log: log, web: web, hub: newHub(), states: map[string]time.Time{}}
	return s
}

// SetManager wires the scaler manager (created after the server so it can notify the hub).
func (s *Server) SetManager(m *scaler.Manager) { s.mgr = m }

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

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics) // own authorization (token, session or loopback)

	// Public: status, setup and login.
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/setup/unlock", s.handleSetupUnlock)
	mux.HandleFunc("POST /api/setup/manifest", s.handleSetupManifest)
	mux.HandleFunc("GET /setup/callback", s.handleSetupCallback)
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleAuthCallback)
	mux.HandleFunc("POST /api/logout", s.handleLogout)

	// Authenticated API.
	mux.Handle("GET /api/state", s.auth(s.handleState))
	mux.Handle("GET /api/events", s.auth(s.handleEvents))
	mux.Handle("GET /api/installations", s.auth(s.handleInstallations))
	mux.Handle("POST /api/targets", s.auth(s.handleCreateTarget))
	mux.Handle("PATCH /api/targets/{id}", s.auth(s.handleUpdateTarget))
	mux.Handle("DELETE /api/targets/{id}", s.auth(s.handleDeleteTarget))
	mux.Handle("GET /api/jobs", s.auth(s.handleJobs))
	mux.Handle("GET /api/jobs/repos", s.auth(s.handleJobRepos))
	mux.Handle("GET /api/jobs/memory", s.auth(s.handleJobMemory))
	mux.Handle("GET /api/runners/{name}/logs", s.auth(s.handleLogs))
	mux.Handle("GET /api/runners/{name}/job-log", s.auth(s.handleJobLog))
	mux.Handle("GET /api/runners/{name}/github", s.auth(s.handleGitHubJob))
	mux.Handle("GET /api/settings", s.auth(s.handleGetSettings))
	mux.Handle("GET /api/compare", s.auth(s.handleCompare))
	mux.Handle("PUT /api/settings", s.auth(s.handlePutSettings))
	mux.Handle("/api/updates", s.auth(s.handleUpdates))
	mux.Handle("/api/updates/", s.auth(s.handleUpdates))

	mux.HandleFunc("/", s.handleWeb)
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

// token returns a GitHub token able to read Actions data of a target.
func (s *Server) token(ctx context.Context, t store.Target) (string, error) {
	if s.cfg.PAT != "" {
		return s.cfg.PAT, nil
	}
	pem, _ := s.store.GetSecret("app_pem")
	appID, _ := strconv.ParseInt(s.store.Get("app_id", "0"), 10, 64)
	client, err := gh.NewAppClient(appID, pem)
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
		writeErr(w, http.StatusInternalServerError, err.Error())
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
	AllowedUsers  string `json:"allowedUsers"`
	HistoryDays   int    `json:"historyDays"`
	BaseURL       string `json:"baseUrl"`
	// Bearer token required by /metrics. Never returned: GET only says
	// whether one is set; PUT changes it only when the field is present
	// ("" removes it).
	MetricsToken    *string `json:"metricsToken,omitempty"`
	MetricsTokenSet bool    `json:"metricsTokenSet"`
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.upd == nil {
		writeErr(w, http.StatusNotFound, "updates are not available")
		return
	}
	s.upd.ServeHTTP(w, r)
}

// LoadPoolConfig reads the pool configuration from the store.
func LoadPoolConfig(st *store.Store, dataDir string) pool.Config {
	return pool.Config{
		StateFile:     filepath.Join(dataDir, "slots.json"),
		Slots:         st.GetInt("slots", DefaultSlots()),
		RunnerImage:   st.Get("runner_image", ""),
		DindImage:     st.Get("dind_image", ""),
		CPUShares:     int64(st.GetInt("cpu_shares", 0)),
		MemoryMB:      int64(st.GetInt("memory_mb", 0)),
		CacheScope:    st.Get("cache_scope", "owner"),
		Mirror:        st.Get("mirror", "true") == "true",
		NoQEMU:        st.Get("qemu", "true") != "true",
		KeepBuildGB:   st.GetInt("keep_build_gb", 20),
		MinFreeDiskGB: st.GetInt("min_free_disk_gb", 20),
	}
}

// DefaultSlots is half the CPUs, between 1 and 8.
func DefaultSlots() int { return min(8, max(1, runtime.NumCPU()/2)) }

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	c := s.pool.Config()
	writeJSON(w, Settings{
		Slots: c.Slots, RunnerImage: c.RunnerImage, DindImage: c.DindImage, CPUShares: c.CPUShares, MemoryMB: c.MemoryMB,
		CacheScope: c.CacheScope, Mirror: c.Mirror, QEMU: !c.NoQEMU, KeepBuildGB: c.KeepBuildGB, MinFreeDiskGB: c.MinFreeDiskGB,
		AllowedUsers: s.store.Get("allowed_users", ""), HistoryDays: s.store.GetInt("history_days", 30), BaseURL: s.knownBaseURL(r),
		MetricsTokenSet: s.store.Get(metricsTokenKey, "") != "",
	})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	// Fields missing from the request keep their current value.
	in := Settings{MinFreeDiskGB: s.pool.Config().MinFreeDiskGB}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	user, _ := s.currentUser(r)
	in.AllowedUsers = normalizeUsers(in.AllowedUsers)
	if !s.cfg.InsecureNoAuth && !containsFold(strings.Split(in.AllowedUsers, ","), user) {
		writeErr(w, http.StatusBadRequest, "you cannot remove yourself from the allowed users")
		return
	}
	if in.Slots < 1 || in.Slots > 64 {
		writeErr(w, http.StatusBadRequest, "parallel jobs must be between 1 and 64")
		return
	}
	if in.MetricsToken != nil {
		tok := strings.TrimSpace(*in.MetricsToken)
		if tok != "" && len(tok) < 16 {
			writeErr(w, http.StatusBadRequest, "the metrics token must be at least 16 characters")
			return
		}
		var err error
		if tok == "" {
			err = s.store.Set(metricsTokenKey, "")
		} else {
			err = s.store.SetSecret(metricsTokenKey, tok)
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if in.CacheScope != "owner" && in.CacheScope != "shared" {
		in.CacheScope = "owner"
	}
	kv := map[string]string{
		"slots": strconv.Itoa(in.Slots), "runner_image": strings.TrimSpace(in.RunnerImage), "dind_image": strings.TrimSpace(in.DindImage),
		"cpu_shares": strconv.FormatInt(max(0, in.CPUShares), 10), "memory_mb": strconv.FormatInt(max(0, in.MemoryMB), 10),
		"cache_scope": in.CacheScope, "mirror": strconv.FormatBool(in.Mirror), "qemu": strconv.FormatBool(in.QEMU), "keep_build_gb": strconv.Itoa(max(1, in.KeepBuildGB)),
		"allowed_users": in.AllowedUsers, "history_days": strconv.Itoa(max(1, in.HistoryDays)),
		"min_free_disk_gb": strconv.Itoa(max(0, in.MinFreeDiskGB)),
	}
	if b := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"); b != "" {
		kv["base_url"] = b
	}
	for k, v := range kv {
		if err := s.store.Set(k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	go func() {
		if err := s.pool.Configure(context.Background(), LoadPoolConfig(s.store, s.cfg.DataDir)); err != nil {
			s.log.Error("apply settings", "error", err)
		}
		s.Notify()
	}()
	s.handleGetSettings(w, r)
}

func normalizeUsers(s string) string {
	var out []string
	for _, u := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if u = strings.TrimPrefix(strings.TrimSpace(u), "@"); u != "" && !containsFold(out, u) {
			out = append(out, u)
		}
	}
	return strings.Join(out, ",")
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
