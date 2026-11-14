// Command pitlane runs fast, ephemeral, autoscaled self-hosted GitHub Actions
// runners on a single Docker host, with a web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/server"
	"github.com/TuroYT/pitlane/internal/store"
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
		case "setup-token":
			dir := env("PITLANE_DATA", "/var/lib/pitlane")
			if len(os.Args) > 2 {
				dir = os.Args[2]
			}
			b, err := os.ReadFile(filepath.Join(dir, "setup-token"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "no setup token: setup is already complete (or Pitlane never started)")
				os.Exit(1)
			}
			fmt.Print(string(b))
			return
		}
	}

	listen := flag.String("listen", env("PITLANE_LISTEN", ":8080"), "HTTP listen address (PITLANE_LISTEN)")
	dataDir := flag.String("data", env("PITLANE_DATA", "/var/lib/pitlane"), "data directory (PITLANE_DATA)")
	baseURL := flag.String("base-url", env("PITLANE_BASE_URL", ""), "public URL of the UI, e.g. http://pitlane.lan:8080 (PITLANE_BASE_URL)")
	logLevel := flag.String("log-level", env("PITLANE_LOG_LEVEL", "info"), "debug, info, warn or error (PITLANE_LOG_LEVEL)")
	noAuth := flag.Bool("insecure-no-auth", env("PITLANE_INSECURE_NO_AUTH", "") == "true", "disable login (development only)")
	flag.Parse()

	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(*logLevel))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	if err := run(log, *listen, *dataDir, *baseURL, *noAuth); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen, dataDir, baseURL string, noAuth bool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	pool.SetVersion(version) // default runner image released with this binary
	p, err := pool.New(server.LoadPoolConfig(st, dataDir), log.With("component", "pool"))
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	if _, err := p.Docker().Ping(ctx); err != nil {
		return fmt.Errorf("cannot reach Docker (is the socket mounted / is Docker running?): %w", err)
	}

	srv := server.New(server.Config{
		Version: version, DataDir: dataDir, BaseURL: baseURL,
		PAT: strings.TrimSpace(os.Getenv("PITLANE_GITHUB_TOKEN")), InsecureNoAuth: noAuth,
	}, st, p, log.With("component", "http"), web.Dist())
	mgr := scaler.New(st, p, dataDir, srv.Credentials, srv.Notify, log.With("component", "scaler"))
	mgr.Version = version
	mgr.ListRunners = srv.GitHubRunners
	srv.SetManager(mgr)

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

	<-ctx.Done()
	log.Info("shutting down (busy runners keep running and are adopted on restart)")
	mgr.Shutdown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		_ = httpSrv.Close() // long-lived event streams (open browser tabs)
	}
	return nil
}
