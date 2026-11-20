package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/server"
	"github.com/TuroYT/pitlane/internal/store"
	"github.com/TuroYT/pitlane/internal/update"
	"github.com/TuroYT/pitlane/web"
)

// run opens the store and Docker, wires every component, serves until a
// signal or a restart request, and shuts down. It returns errRestart when
// an update asked to restart into the new binary.
func run(ctx context.Context, log *slog.Logger, cfg server.Config, cacheListen string, sys system) error {
	ctx, stop := sys.signals(ctx)
	defer stop()

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	p, err := openPool(ctx, log, st, cfg.DataDir, sys.ping)
	if err != nil {
		return err
	}
	a, err := build(log, cfg, cacheListen, st, p, sys, stop)
	if err != nil {
		return err
	}
	return a.serve(ctx)
}

// openPool creates the pool from the stored settings and checks that
// Docker answers, before anything is started.
func openPool(ctx context.Context, log *slog.Logger, st *store.Store, dataDir string, ping func(context.Context, *pool.Pool) error) (*pool.Pool, error) {
	pool.SetVersion(version) // default runner image released with this binary
	p, err := pool.New(server.LoadPoolConfig(st, dataDir), log.With("component", "pool"))
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if err := ping(ctx, p); err != nil {
		return nil, fmt.Errorf("cannot reach Docker (is the socket mounted / is Docker running?): %w", err)
	}
	return p, nil
}

// build wires the actions cache, the web server, the scaler and the
// updater, and opens the UI listener. Nothing runs yet but the cache
// listener, which is closed again on failure. stop ends serve.
func build(log *slog.Logger, cfg server.Config, cacheListen string, st *store.Store, p *pool.Pool, sys system, stop context.CancelFunc) (*app, error) {
	a := &app{log: log, slots: p, stop: stop}
	// Before the scaler adopts runners left by a previous run: they keep
	// their actions cache token.
	a.cache = startActionsCache(log.With("component", "cache"), cfg.DataDir, cacheListen, sys.listen)
	if a.cache != nil {
		p.SetCache(a.cache.store, a.cache.port)
	}

	srv := server.New(cfg, st, p, log.With("component", "http"), web.Dist())
	srv.SetActionsCache(a.cache.server())
	mgr := newManager(log, st, p, srv, cfg.DataDir)
	upd := newUpdater(log, sys.getenv, cfg.DataDir, st, p, srv.Notify, a.requestRestart)
	srv.SetUpdates(upd.Handler())
	a.ui, a.scaler, a.updates = srv, mgr, upd

	ln, err := sys.listen("tcp", listenAddr(cfg.Listen))
	if err != nil {
		a.cache.close()
		return nil, fmt.Errorf("listen on %q: %w", cfg.Listen, err)
	}
	a.http, a.ln = newHTTPServer(cfg.Listen, srv.Handler()), ln
	return a, nil
}

// newManager creates the scaler and wires it to the web server, both ways.
func newManager(log *slog.Logger, st *store.Store, p *pool.Pool, srv *server.Server, dataDir string) *scaler.Manager {
	mgr := scaler.New(st, p, dataDir, srv.Credentials, srv.Notify, log.With("component", "scaler"))
	mgr.Version = version
	mgr.ListRunners = srv.GitHubRunners
	mgr.StrictReason = srv.StrictReason
	mgr.JobQueuedAt = srv.JobQueuedAt
	srv.SetManager(mgr)
	return mgr
}

// newUpdater creates the self-updater. restart is called once a new binary
// is installed.
func newUpdater(log *slog.Logger, getenv func(string) string, dataDir string, st *store.Store, p *pool.Pool, notify, restart func()) *update.Updater {
	updLog := log.With("component", "update")
	ghReleases := &update.GitHub{}
	verify, attested := update.ReleaseVerifier(getenv, ghReleases, dataDir, updLog)
	if !attested {
		updLog.Warn("self-update will NOT verify build attestations (" + update.SkipAttestationEnv + "=true): only the checksum is checked, which is unsafe")
	}
	return &update.Updater{
		Version: version, Store: st, GitHub: ghReleases, Probe: update.DefaultProbe(version), Verify: verify, Attested: attested,
		Images: update.DockerImages{Cli: p.Docker(), PullFunc: func(ctx context.Context, ref string) error {
			return p.EnsureImage(ctx, ref, true)
		}},
		RunnerImage: func() string { return p.Config().RunnerImage },
		Busy:        func() bool { return anyBusy(p.Slots()) },
		Restart:     restart,
		Notify:      notify,
		Log:         updLog,
	}
}

// anyBusy reports whether a slot runs a job: the updater waits for none.
func anyBusy(slots []pool.SlotState) bool {
	for _, sl := range slots {
		if sl.Status == "busy" {
			return true
		}
	}
	return false
}

// The parts of the components that app starts and stops, so tests can run
// the lifecycle without Docker or GitHub.
type (
	webUI interface {
		EnsureSetupToken() server.SetupToken
		Notify()
	}
	slotPool interface {
		Start(context.Context) error
	}
	runnerScaler interface {
		Start(context.Context) error
		Shutdown()
	}
	updater interface {
		Run(context.Context)
	}
)

// app is Pitlane wired and ready to serve.
type app struct {
	log     *slog.Logger
	ui      webUI        // *server.Server
	slots   slotPool     // *pool.Pool
	scaler  runnerScaler // *scaler.Manager
	updates updater      // *update.Updater
	http    *http.Server
	ln      net.Listener // the UI's, already open
	cache   *actionsCache
	// stop ends the context serve waits on.
	stop    context.CancelFunc
	restart atomic.Bool
}

// requestRestart is the updater's Restart: shut down, then re-exec.
func (a *app) requestRestart() {
	a.restart.Store(true)
	a.stop()
}

// serve starts the UI, the warm slots, the scaler and the updater, waits
// for ctx to end and shuts down: the scaler first, so no new runner starts
// while the servers go away.
func (a *app) serve(ctx context.Context) error {
	httpDone := a.startHTTP()
	logSetupToken(a.log, a.ui.EnsureSetupToken())
	if err := a.start(ctx); err != nil {
		_ = a.http.Close()
		<-httpDone
		a.cache.close()
		return err
	}
	<-ctx.Done()
	a.log.Info("shutting down (busy runners keep running and are adopted on restart)")
	a.scaler.Shutdown()
	shutdownHTTP(a.http, shutdownTimeout)
	<-httpDone
	a.cache.close()
	if a.restart.Load() {
		return errRestart
	}
	return nil
}

// startHTTP serves the UI in the background. A serving error stops
// Pitlane. The returned channel is closed once the server has stopped.
func (a *app) startHTTP() <-chan struct{} {
	done := make(chan struct{})
	a.log.Info("pitlane listening", "addr", a.http.Addr, "version", version)
	go func() {
		defer close(done)
		if err := a.http.Serve(a.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.log.Error("http server", "error", err)
			a.stop()
		}
	}()
	return done
}

// start brings up the warm slots, then the scaler, which adopts the runners
// a previous process left, then the updater.
func (a *app) start(ctx context.Context) error {
	a.log.Info("starting warm slots (first start pulls images and may take a minute)")
	if err := a.slots.Start(ctx); err != nil {
		return fmt.Errorf("start slots: %w", err)
	}
	if err := a.scaler.Start(ctx); err != nil {
		return fmt.Errorf("start scaler: %w", err)
	}
	a.ui.Notify()
	go a.updates.Run(ctx)
	return nil
}

// logSetupToken tells the operator how to get the setup token. The token
// itself only goes to the log when it could not be saved: logs are often
// shipped elsewhere, the data directory is not. That fallback is a warning
// so that it shows at any log level but error: it is the only way left to
// get the token.
func logSetupToken(log *slog.Logger, tok server.SetupToken) {
	switch {
	case tok.Value == "":
	case tok.Saved:
		log.Info("first-time setup: open the web UI and enter the setup token saved in this file", "file", tok.File)
	default:
		log.Warn("first-time setup: open the web UI and enter this setup token", "token", tok.Value)
	}
}
