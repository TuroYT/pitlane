package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/server"
	"github.com/TuroYT/pitlane/internal/store"
	"github.com/TuroYT/pitlane/internal/update"
	"github.com/TuroYT/pitlane/web"
)

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

	httpSrv := newHTTPServer(listen, srv.Handler())
	go func() {
		log.Info("pitlane listening", "addr", listen, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()
	logSetupToken(log, srv.EnsureSetupToken())

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
