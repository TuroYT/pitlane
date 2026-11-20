// Command pitlane runs fast, ephemeral, autoscaled self-hosted GitHub Actions
// runners on a single Docker host, with a web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/TuroYT/pitlane/internal/cache"
	// Installed as the runner's externals/node*/bin/node, the binary runs as
	// the Node shim from this package's init, before anything else.
	_ "github.com/TuroYT/pitlane/internal/nodeshim"
	"github.com/TuroYT/pitlane/internal/server"
)

var version = "dev"

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
