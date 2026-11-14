// Package server exposes Pitlane's HTTP API, the GitHub App setup flow,
// GitHub login and the embedded web UI.
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
}

// Server is the HTTP server.
type Server struct {
	cfg   Config
	store *store.Store
	pool  *pool.Pool
	mgr   *scaler.Manager
	log   *slog.Logger
	web   fs.FS
	hub   *hub

	setupToken string
	compare    compareCache
	mu         sync.Mutex
	states     map[string]time.Time // OAuth / manifest states
}

// New creates the server. web is the built UI (may be nil).
func New(cfg Config, st *store.Store, p *pool.Pool, log *slog.Logger, web fs.FS) *Server {
	s := &Server{cfg: cfg, store: st, pool: p, log: log, web: web, hub: newHub(), states: map[string]time.Time{}}
	return s
}

// SetManager wires the scaler manager (created after the server so it can notify the hub).
func (s *Server) SetManager(m *scaler.Manager) { s.mgr = m }

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

func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	if v := s.store.Get("base_url", ""); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// EnsureSetupToken creates the one-time token protecting the first setup.
func (s *Server) EnsureSetupToken() string {
	file := filepath.Join(s.cfg.DataDir, "setup-token")
	if s.store.Get("allowed_users", "") != "" {
		_ = os.Remove(file)
		return ""
	}
	if b, err := os.ReadFile(file); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		s.setupToken = strings.TrimSpace(string(b))
		return s.setupToken
	}
	s.setupToken = randomHex(12)
	_ = os.WriteFile(file, []byte(s.setupToken+"\n"), 0o600)
	return s.setupToken
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
	mux.Handle("GET /api/runners/{name}/logs", s.auth(s.handleLogs))
	mux.Handle("GET /api/runners/{name}/job-log", s.auth(s.handleJobLog))
	mux.Handle("GET /api/settings", s.auth(s.handleGetSettings))
	mux.Handle("GET /api/compare", s.auth(s.handleCompare))
	mux.Handle("PUT /api/settings", s.auth(s.handlePutSettings))

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

// ------------------------------------------------------------------- auth

const (
	sessionCookie = "pitlane_session"
	setupCookie   = "pitlane_setup"
	stateCookie   = "pitlane_state"
)

func (s *Server) currentUser(r *http.Request) (string, bool) {
	if s.cfg.InsecureNoAuth {
		return "admin", true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	login, ok := s.store.SessionLogin(c.Value)
	if !ok || !s.allowed(login) {
		return "", false
	}
	return login, true
}

func (s *Server) allowed(login string) bool {
	for _, u := range strings.Split(s.store.Get("allowed_users", ""), ",") {
		if strings.EqualFold(strings.TrimSpace(u), login) && login != "" {
			return true
		}
	}
	return false
}

func (s *Server) hasSetupCookie(r *http.Request) bool {
	if s.setupToken == "" {
		return false
	}
	c, err := r.Cookie(setupCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.setupToken)) == 1
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		h(w, r)
	})
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true,
		Secure: strings.HasPrefix(s.baseURL(r), "https://"), SameSite: http.SameSiteLaxMode})
}

// newState returns a random state bound to a purpose ("manifest" or "oauth")
// so one flow's state can never be replayed in the other.
func (s *Server) newState(purpose string) string {
	st := purpose + "." + randomHex(16)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.states {
		if time.Since(t) > 15*time.Minute {
			delete(s.states, k)
		}
	}
	s.states[st] = time.Now()
	return st
}

func (s *Server) consumeState(purpose, st string) bool {
	if !strings.HasPrefix(st, purpose+".") {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.states[st]
	delete(s.states, st)
	return ok && time.Since(t) < 15*time.Minute
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	user, signedIn := s.currentUser(r)
	mode := "none"
	switch {
	case s.cfg.PAT != "":
		mode = "pat"
	case s.appConfigured():
		mode = "app"
	}
	writeJSON(w, map[string]any{
		"version":       s.cfg.Version,
		"mode":          mode,
		"needsSetup":    s.store.Get("allowed_users", "") == "" && !s.cfg.InsecureNoAuth,
		"setupUnlocked": s.hasSetupCookie(r),
		"signedIn":      signedIn,
		"user":          user,
		"baseUrl":       s.baseURL(r),
		"app": map[string]string{
			"slug":    s.store.Get("app_slug", ""),
			"name":    s.store.Get("app_name", ""),
			"htmlUrl": s.store.Get("app_html_url", ""),
			"owner":   s.store.Get("app_owner", ""),
		},
	})
}

func (s *Server) handleSetupUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || s.setupToken == "" ||
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Token)), []byte(s.setupToken)) != 1 {
		writeErr(w, http.StatusForbidden, "invalid setup token")
		return
	}
	s.setCookie(w, r, setupCookie, s.setupToken, 3600)
	writeJSON(w, map[string]bool{"ok": true})
}

// handleSetupManifest returns what the browser must POST to github.com to
// create the GitHub App.
func (s *Server) handleSetupManifest(w http.ResponseWriter, r *http.Request) {
	if !s.canManageApp(r) {
		writeErr(w, http.StatusForbidden, "setup is locked")
		return
	}
	var body struct {
		BaseURL string `json:"baseUrl"`
		Org     string `json:"org"`
		Name    string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	base := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		base = s.baseURL(r)
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		writeErr(w, http.StatusBadRequest, "base URL must start with http:// or https://")
		return
	}
	_ = s.store.Set("base_url", base)
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Pitlane " + randomHex(3)
	}
	manifest, _ := json.Marshal(gh.Manifest(name, base))
	state := s.newState("manifest")
	s.setCookie(w, r, stateCookie, state, 900)
	action := "https://github.com/settings/apps/new?state=" + state
	if org := strings.TrimSpace(body.Org); org != "" {
		action = "https://github.com/organizations/" + org + "/settings/apps/new?state=" + state
	}
	writeJSON(w, map[string]string{"action": action, "manifest": string(manifest)})
}

// canManageApp reports whether the request may create the GitHub App: during
// first setup it needs the setup token cookie, afterwards a signed-in admin.
func (s *Server) canManageApp(r *http.Request) bool {
	if !s.appConfigured() && s.store.Get("allowed_users", "") == "" {
		return s.hasSetupCookie(r)
	}
	_, signedIn := s.currentUser(r)
	return signedIn && !s.cfg.InsecureNoAuth || (s.cfg.InsecureNoAuth && !s.appConfigured())
}

func (s *Server) handleSetupCallback(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query().Get("state")
	c, err := r.Cookie(stateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(st)) != 1 || !s.canManageApp(r) || !s.consumeState("manifest", st) {
		http.Redirect(w, r, "/setup?error=state", http.StatusFound)
		return
	}
	app, err := gh.ConvertManifest(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		s.log.Error("github app conversion", "error", err)
		http.Redirect(w, r, "/setup?error=conversion", http.StatusFound)
		return
	}
	for k, v := range map[string]string{
		"app_id": strconv.FormatInt(app.ID, 10), "app_slug": app.Slug, "app_name": app.Name,
		"app_client_id": app.ClientID, "app_html_url": app.HTMLURL, "app_owner": app.Owner.Login,
	} {
		_ = s.store.Set(k, v)
	}
	_ = s.store.SetSecret("app_pem", app.PEM)
	_ = s.store.SetSecret("app_client_secret", app.ClientSecret)
	_ = s.store.SetSecret("app_webhook_secret", app.WebhookSecret)
	s.setCookie(w, r, stateCookie, "", -1)
	s.log.Info("GitHub App created", "app", app.Slug)
	if s.mgr != nil {
		s.mgr.Restart()
	}
	http.Redirect(w, r, "/setup?step=install", http.StatusFound)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	clientID := s.store.Get("app_client_id", "")
	if clientID == "" {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	st := s.newState("oauth")
	s.setCookie(w, r, stateCookie, st, 900)
	http.Redirect(w, r, gh.AuthorizeURL(clientID, s.baseURL(r)+"/auth/callback", st), http.StatusFound)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query().Get("state")
	c, err := r.Cookie(stateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(st)) != 1 || !s.consumeState("oauth", st) {
		http.Redirect(w, r, "/login?error=state", http.StatusFound)
		return
	}
	secret, _ := s.store.GetSecret("app_client_secret")
	login, err := gh.ExchangeCode(r.Context(), s.store.Get("app_client_id", ""), secret, r.URL.Query().Get("code"), s.baseURL(r)+"/auth/callback")
	if err != nil {
		s.log.Error("oauth", "error", err)
		http.Redirect(w, r, "/login?error=oauth", http.StatusFound)
		return
	}
	// The first person to sign in with the setup token becomes the admin.
	if s.store.Get("allowed_users", "") == "" && s.hasSetupCookie(r) {
		_ = s.store.Set("allowed_users", login)
		s.EnsureSetupToken() // deletes the token file
		s.setupToken = ""
		s.log.Info("first admin registered", "login", login)
	}
	if !s.allowed(login) {
		s.log.Warn("sign-in refused", "login", login)
		http.Redirect(w, r, "/login?error=forbidden&login="+login, http.StatusFound)
		return
	}
	token := randomHex(32)
	if err := s.store.CreateSession(token, login, 30*24*time.Hour); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setCookie(w, r, sessionCookie, token, 30*24*3600)
	s.setCookie(w, r, stateCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(c.Value)
	}
	s.setCookie(w, r, sessionCookie, "", -1)
	writeJSON(w, map[string]bool{"ok": true})
}

// ------------------------------------------------------------------- state

type hostStats struct {
	CPUs       int     `json:"cpus"`
	CPUPercent float64 `json:"cpuPercent"` // 0-100, whole machine/container`
	MemTotal   int64   `json:"memTotal"`
	MemAvail   int64   `json:"memAvailable"`
	DiskTotal  int64   `json:"diskTotal"`
	DiskFree   int64   `json:"diskFree"`
}

// cpuSampler measures real CPU usage. It reads the cgroup v2 counter of the
// container Pitlane runs in (correct inside LXC/Docker, where /proc/loadavg
// reports the whole host) and falls back to /proc/stat.
type cpuSampler struct {
	mu       sync.Mutex
	lastUse  float64 // CPU-seconds
	lastWall time.Time
	percent  float64
}

var cpu = &cpuSampler{}

func cpuSeconds() (float64, bool) {
	if b, err := os.ReadFile("/sys/fs/cgroup/cpu.stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "usage_usec" {
				v, err := strconv.ParseFloat(f[1], 64)
				return v / 1e6, err == nil
			}
		}
	}
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		f := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		if len(f) > 8 && f[0] == "cpu" {
			var busy float64
			for i, x := range f[1:9] {
				if i == 3 || i == 4 { // idle, iowait
					continue
				}
				v, _ := strconv.ParseFloat(x, 64)
				busy += v
			}
			return busy / 100, true // USER_HZ
		}
	}
	return 0, false
}

func (c *cpuSampler) sample() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	use, ok := cpuSeconds()
	now := time.Now()
	if !ok {
		return 0
	}
	if !c.lastWall.IsZero() {
		if dt := now.Sub(c.lastWall).Seconds(); dt >= 1 {
			c.percent = min(100, max(0, (use-c.lastUse)/dt/float64(runtime.NumCPU())*100))
			c.lastUse, c.lastWall = use, now
		}
	} else {
		c.lastUse, c.lastWall = use, now
	}
	return c.percent
}

func readHost(dataDir string) hostStats {
	h := hostStats{CPUs: runtime.NumCPU(), CPUPercent: cpu.sample()}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				h.MemTotal = v * 1024
			case "MemAvailable:":
				h.MemAvail = v * 1024
			}
		}
	}
	dir := "/var/lib/docker"
	if _, err := os.Stat(dir); err != nil {
		dir = dataDir
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		h.DiskTotal = int64(st.Blocks) * int64(st.Bsize)
		h.DiskFree = int64(st.Bavail) * int64(st.Bsize)
	}
	return h
}

func (s *Server) snapshot() map[string]any {
	return map[string]any{
		"targets": s.mgr.Snapshot(),
		"slots":   s.pool.Slots(),
		"host":    readHost(s.cfg.DataDir),
		"time":    time.Now().Unix(),
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.snapshot()) }

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	send := func() bool {
		b, _ := json.Marshal(s.snapshot())
		if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	heartbeat := time.NewTicker(5 * time.Second) // also refreshes host stats
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			time.Sleep(150 * time.Millisecond) // coalesce bursts
			if !send() {
				return
			}
		case <-heartbeat.C:
			if !send() {
				return
			}
		}
	}
}

// ------------------------------------------------------------- installations

func (s *Server) handleInstallations(w http.ResponseWriter, r *http.Request) {
	type repo struct {
		Name     string `json:"name"`
		FullName string `json:"fullName"`
		Private  bool   `json:"private"`
	}
	type inst struct {
		ID      int64  `json:"id"`
		Account string `json:"account"`
		Type    string `json:"type"`
		Repos   []repo `json:"repos"`
		Error   string `json:"error,omitempty"`
	}
	out := map[string]any{"installations": []inst{}, "installUrl": ""}
	if slug := s.store.Get("app_slug", ""); slug != "" {
		out["installUrl"] = "https://github.com/apps/" + slug + "/installations/new"
	}
	if !s.appConfigured() {
		writeJSON(w, out)
		return
	}
	pem, _ := s.store.GetSecret("app_pem")
	appID, _ := strconv.ParseInt(s.store.Get("app_id", "0"), 10, 64)
	client, err := gh.NewAppClient(appID, pem)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	insts, err := client.Installations(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	list := make([]inst, len(insts))
	var wg sync.WaitGroup
	for i, in := range insts {
		list[i] = inst{ID: in.ID, Account: in.Account.Login, Type: in.Account.Type, Repos: []repo{}}
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			repos, err := client.Repositories(ctx, id)
			if err != nil {
				list[i].Error = err.Error()
				return
			}
			for _, rp := range repos {
				list[i].Repos = append(list[i].Repos, repo{Name: rp.Name, FullName: rp.FullName, Private: rp.Private})
			}
		}(i, in.ID)
	}
	wg.Wait()
	out["installations"] = list
	writeJSON(w, out)
}

// ------------------------------------------------------------------ targets

var validName = func(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if !(c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	t := store.Target{MaxJobMinutes: store.DefaultMaxJobMinutes}
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if (t.Kind != "repo" && t.Kind != "org") || !validName(t.Owner) || (t.Kind == "repo" && !validName(t.Repo)) {
		writeErr(w, http.StatusBadRequest, "invalid target")
		return
	}
	if t.Kind == "org" {
		t.Repo = ""
	}
	if t.ScaleSetName != "" && !validName(t.ScaleSetName) {
		writeErr(w, http.StatusBadRequest, "invalid runner label")
		return
	}
	if t.MaxRunners <= 0 {
		t.MaxRunners = max(1, s.pool.Config().Slots)
	}
	t.Enabled = true
	created, err := s.store.CreateTarget(t)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.mgr.Sync()
	writeJSON(w, created)
}

func (s *Server) targetFromPath(w http.ResponseWriter, r *http.Request) (store.Target, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.store.Target(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "target not found")
		return t, false
	}
	return t, true
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.targetFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled      *bool   `json:"enabled"`
		MinRunners   *int    `json:"minRunners"`
		MaxRunners   *int    `json:"maxRunners"`
		ScaleSetName *string `json:"scaleSetName"`
		Strict       *bool   `json:"strict"`
		MaxJobMins   *int    `json:"maxJobMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Enabled != nil {
		t.Enabled = *body.Enabled
	}
	if body.MinRunners != nil {
		t.MinRunners = max(0, *body.MinRunners)
	}
	if body.Strict != nil {
		t.Strict = *body.Strict
	}
	if body.MaxJobMins != nil {
		t.MaxJobMinutes = max(0, *body.MaxJobMins)
	}
	if body.MaxRunners != nil {
		t.MaxRunners = max(1, *body.MaxRunners)
	}
	if body.ScaleSetName != nil && *body.ScaleSetName != t.ScaleSetName {
		if !validName(*body.ScaleSetName) {
			writeErr(w, http.StatusBadRequest, "invalid runner label")
			return
		}
		t.ScaleSetName, t.ScaleSetID = *body.ScaleSetName, 0
	}
	t.MinRunners = min(t.MinRunners, t.MaxRunners)
	if err := s.store.UpdateTarget(t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mgr.Sync()
	writeJSON(w, t)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.targetFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTarget(t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mgr.Sync()
	writeJSON(w, map[string]bool{"ok": true})
}

// --------------------------------------------------------------- jobs, logs

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	jobs, err := s.store.Jobs(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, jobs)
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "invalid runner name")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	if id, ok := s.mgr.RunnerContainer(name); ok {
		fl, _ := w.(http.Flusher)
		var out io.Writer = w
		if fl != nil {
			out = flushWriter{w, fl}
		}
		_ = s.pool.Logs(r.Context(), id, r.URL.Query().Get("follow") == "1", out)
		return
	}
	f, err := os.Open(s.mgr.LogFile(name))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no logs for this runner")
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

// handleJobLog streams the step output of the job a runner ran: live from the
// runner while it runs, from the GitHub API once it is done.
func (s *Server) handleJobLog(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(name) {
		writeErr(w, http.StatusBadRequest, "invalid runner name")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	if id, ok := s.mgr.RunnerContainer(name); ok {
		fl, _ := w.(http.Flusher)
		var out io.Writer = w
		if fl != nil {
			out = flushWriter{w, fl}
		}
		_ = s.pool.TailJobOutput(r.Context(), id, out)
		return
	}
	job, err := s.store.JobByRunner(r.Context(), name)
	if err != nil || job.RunID == 0 {
		writeErr(w, http.StatusNotFound, "no job recorded for this runner")
		return
	}
	token := s.cfg.PAT
	if token == "" {
		t, err := s.store.Target(job.TargetID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "repository is no longer enabled")
			return
		}
		pem, _ := s.store.GetSecret("app_pem")
		appID, _ := strconv.ParseInt(s.store.Get("app_id", "0"), 10, 64)
		client, err := gh.NewAppClient(appID, pem)
		if err == nil {
			token, err = client.InstallationToken(r.Context(), t.InstallationID)
		}
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	rc, err := gh.JobLog(r.Context(), token, job.Owner, job.Repo, job.RunID, name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
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
		AllowedUsers: s.store.Get("allowed_users", ""), HistoryDays: s.store.GetInt("history_days", 30), BaseURL: s.baseURL(r),
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
	http.ServeFileFS(w, r, s.web, p)
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
