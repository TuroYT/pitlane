// Package server exposes Pitlane's HTTP API, the GitHub App setup flow,
// GitHub login and the embedded web UI.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
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
	// started is when the server was created (pitlane_start_time_seconds).
	started time.Time

	setupToken string
	compare    compareCache
	// strictCheck replaces StrictReason in tests.
	strictCheck func(ctx context.Context, t store.Target) (string, error)
	// exchangeCode replaces gh.ExchangeCode (OAuth code -> login, user
	// token) in tests.
	exchangeCode func(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, string, error)
	// member replaces isMember (membership checks on GitHub) in tests.
	member func(ctx context.Context, userToken, login, org, team string) (bool, error)
	// jobHistory replaces recentJobs (a repository's recent jobs on GitHub)
	// in tests.
	jobHistory func(ctx context.Context, t store.Target) ([]gh.JobRun, error)
	jobIDs     jobIDCache // runner job -> GitHub job ID (see handleGitHubJob)
	mu         sync.Mutex
	states     map[string]time.Time // OAuth / manifest states
}

// New creates the server. web is the built UI (may be nil).
func New(cfg Config, st *store.Store, p *pool.Pool, log *slog.Logger, web fs.FS) *Server {
	s := &Server{cfg: cfg, store: st, pool: p, log: log, web: web, hub: newHub(), started: time.Now(), states: map[string]time.Time{}}
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
