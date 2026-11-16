// Command pitlane runs fast, ephemeral, autoscaled self-hosted GitHub Actions
// runners on a single Docker host, with a web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/TuroYT/pitlane/internal/cache"
	// Installed as the runner's externals/node*/bin/node, the binary runs as
	// the Node shim from this package's init, before anything else.
	_ "github.com/TuroYT/pitlane/internal/nodeshim"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/server"
	"github.com/TuroYT/pitlane/internal/store"
	"github.com/TuroYT/pitlane/internal/update"
	"github.com/TuroYT/pitlane/web"
)

var version = "dev"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("pitlane", version)
			return
		case "cache-forward":
			// Run by Pitlane itself, in the actions cache forwarder container
			// (see internal/pool/actionscache.go): cache-forward <listen> <target>.
			if len(os.Args) != 4 {
				fmt.Fprintln(os.Stderr, "usage: pitlane cache-forward <listen> <target>")
				os.Exit(2)
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := cache.Forward(ctx, os.Args[2], os.Args[3], log); err != nil {
				log.Error("fatal", "error", err)
				os.Exit(1)
			}
			return
		case "setup-token":
			dir := env("PITLANE_DATA", "/var/lib/pitlane")
			if len(os.Args) > 2 {
				dir = os.Args[2]
			}
			// The operator's own command line chooses the directory.
			b, err := os.ReadFile(filepath.Join(dir, "setup-token")) //nolint:gosec // G703: local CLI argument, not remote input
			if err != nil {
				fmt.Fprintln(os.Stderr, "no setup token: setup is already complete (or Pitlane never started)")
				os.Exit(1)
			}
			fmt.Print(string(b))
			return
		}
	}

	listen := flag.String("listen", env("PITLANE_LISTEN", ":8080"), "HTTP listen address (PITLANE_LISTEN)")
	cacheListen := flag.String("cache-listen", env("PITLANE_CACHE_LISTEN", ":8091"),
		"listen address of the actions cache that runners reach on the slot networks' gateway, \"off\" to disable (PITLANE_CACHE_LISTEN)")
	dataDir := flag.String("data", env("PITLANE_DATA", "/var/lib/pitlane"), "data directory (PITLANE_DATA)")
	baseURL := flag.String("base-url", env("PITLANE_BASE_URL", ""), "public URL of the UI, e.g. http://pitlane.lan:8080 (PITLANE_BASE_URL)")
	trusted := flag.String("trusted-proxies", env("PITLANE_TRUSTED_PROXIES", ""),
		"comma-separated IPs or CIDRs of reverse proxies whose Host and X-Forwarded-Proto headers are trusted when no base URL is set; loopback always is (PITLANE_TRUSTED_PROXIES)")
	logLevel := flag.String("log-level", env("PITLANE_LOG_LEVEL", "info"), "debug, info, warn or error (PITLANE_LOG_LEVEL)")
	noAuth := flag.Bool("insecure-no-auth", env("PITLANE_INSECURE_NO_AUTH", "") == "true", "disable login (development only, loopback listen address only) (PITLANE_INSECURE_NO_AUTH)")
	noAuthAnyAddr := flag.Bool("insecure-no-auth-any-address", env("PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS", "") == "true",
		"allow --insecure-no-auth on a non-loopback listen address, e.g. behind an authenticating proxy (PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS)")
	flag.Parse()

	lvl, err := parseLogLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pitlane:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(log) // packages without a logger of their own (store)
	if *noAuth {
		if err := checkNoAuthListen(*listen, *noAuthAnyAddr); err != nil {
			log.Error("fatal", "error", err)
			os.Exit(1)
		}
		log.Warn("login is disabled (--insecure-no-auth): anyone reaching this address controls Pitlane", "addr", *listen)
	}
	proxies, err := server.ParseTrustedProxies(*trusted)
	if err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
	err = run(log, server.Config{Version: version, DataDir: *dataDir, BaseURL: *baseURL, InsecureNoAuth: *noAuth,
		Listen: *listen, TrustedProxies: proxies, PAT: strings.TrimSpace(os.Getenv("PITLANE_GITHUB_TOKEN"))}, *cacheListen)
	if errors.Is(err, errRestart) {
		reexec(log)
	}
	if err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// parseLogLevel parses --log-level (debug, info, warn or error, any case;
// slog's "info+2" offsets are accepted too).
func parseLogLevel(s string) (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return 0, fmt.Errorf("invalid --log-level / PITLANE_LOG_LEVEL %q: want debug, info, warn or error", s)
	}
	return lvl, nil
}

// checkNoAuthListen refuses to serve without authentication on anything but
// a loopback address, unless explicitly allowed: whoever reaches the UI can
// register runners and read job logs.
func checkNoAuthListen(listen string, anyAddress bool) error {
	if anyAddress || isLoopback(listen) {
		return nil
	}
	return fmt.Errorf("--insecure-no-auth needs a loopback listen address (127.0.0.1, ::1 or localhost), not %q; "+
		"to serve it on another address behind your own authentication, also set --insecure-no-auth-any-address "+
		"(PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS=true)", listen)
}

// isLoopback reports whether a listen address only accepts local
// connections (":8080" listens on every interface).
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// errRestart asks main to start the (just updated) binary again.
var errRestart = errors.New("restart requested")

// reexec replaces the process with the binary now at its path, keeping the
// PID (systemd keeps tracking it) and the arguments. Busy runners are Docker
// containers, not children, so they keep running and are adopted on start.
func reexec(log *slog.Logger) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err == nil {
		log.Info("restarting into the updated binary", "path", exe)
		// The path is our own binary, just replaced by a verified release.
		err = syscall.Exec(exe, os.Args, os.Environ()) //nolint:gosec // G204: own executable
	}
	// Exec failed: exit non-zero so the service manager starts us again.
	log.Error("re-exec failed, exiting so the service manager restarts Pitlane", "error", err)
	os.Exit(1)
}

// startActionsCache opens the actions cache and starts its listener. On any
// failure Pitlane runs without it: jobs use GitHub's cache.
func startActionsCache(log *slog.Logger, dataDir, listen string) (*cache.Server, *http.Server, int) {
	if listen == "" || listen == "off" {
		return nil, nil, 0
	}
	port := pool.CachePort(listen)
	if port == 0 {
		log.Error("invalid --cache-listen address, actions cache disabled (jobs use GitHub's cache)", "addr", listen)
		return nil, nil, 0
	}
	cs, err := cache.Open(filepath.Join(dataDir, "actions-cache"), log)
	if err != nil {
		log.Error("actions cache disabled (jobs use GitHub's cache)", "error", err)
		return nil, nil, 0
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Error("actions cache disabled (jobs use GitHub's cache)", "error", err)
		return nil, nil, 0
	}
	// No write timeout: archives can take minutes to stream.
	hs := &http.Server{Handler: cs.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		log.Info("actions cache listening", "addr", listen)
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("actions cache server", "error", err)
		}
	}()
	return cs, hs, port
}

func run(log *slog.Logger, cfg server.Config, cacheListen string) error {
	listen, dataDir := cfg.Listen, cfg.DataDir
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	pool.SetVersion(version) // default runner image released with this binary
	p, err := pool.New(server.LoadPoolConfig(st, dataDir), log.With("component", "pool"))
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		return fmt.Errorf("cannot reach Docker (is the socket mounted / is Docker running?): %w", err)
	}

	// Before the scaler adopts runners left by a previous run: they keep
	// their actions cache token.
	cs, cacheSrv, cachePort := startActionsCache(log.With("component", "cache"), dataDir, cacheListen)
	if cs != nil {
		p.SetCache(cs, cachePort)
	}

	srv := server.New(cfg, st, p, log.With("component", "http"), web.Dist())
	srv.SetActionsCache(cs)
	mgr := scaler.New(st, p, dataDir, srv.Credentials, srv.Notify, log.With("component", "scaler"))
	mgr.Version = version
	mgr.ListRunners = srv.GitHubRunners
	mgr.StrictReason = srv.StrictReason
	mgr.JobQueuedAt = srv.JobQueuedAt
	srv.SetManager(mgr)

	var restart atomic.Bool
	updLog := log.With("component", "update")
	ghReleases := &update.GitHub{}
	verify, attested := update.ReleaseVerifier(os.Getenv, ghReleases, dataDir, updLog)
	if !attested {
		updLog.Warn("self-update will NOT verify build attestations (" + update.SkipAttestationEnv + "=true): only the checksum is checked, which is unsafe")
	}
	upd := &update.Updater{
		Version: version, Store: st, GitHub: ghReleases, Probe: update.DefaultProbe(version), Verify: verify, Attested: attested,
		Images: update.DockerImages{Cli: p.Docker(), PullFunc: func(ctx context.Context, ref string) error {
			return p.EnsureImage(ctx, ref, true)
		}},
		RunnerImage: func() string { return p.Config().RunnerImage },
		Busy: func() bool {
			for _, sl := range p.Slots() {
				if sl.Status == "busy" {
					return true
				}
			}
			return false
		},
		Restart: func() { restart.Store(true); stop() },
		Notify:  srv.Notify,
		Log:     updLog,
	}
	srv.SetUpdates(upd.Handler())

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("pitlane listening", "addr", listen, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()
	if tok := srv.EnsureSetupToken(); tok != "" {
		log.Info("first-time setup: open the web UI and enter this setup token", "token", tok)
	}

	log.Info("starting warm slots (first start pulls images and may take a minute)")
	if err := p.Start(ctx); err != nil {
		return err
	}
	if err := mgr.Start(ctx); err != nil {
		return err
	}
	srv.Notify()
	go upd.Run(ctx)

	<-ctx.Done()
	log.Info("shutting down (busy runners keep running and are adopted on restart)")
	mgr.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		_ = httpSrv.Close() // long-lived event streams (open browser tabs)
	}
	if cacheSrv != nil {
		// Busy runners keep running: their transfers are retried by the
		// client (or the step goes on without the cache).
		_ = cacheSrv.Close()
	}
	if restart.Load() {
		return errRestart
	}
	return nil
}
