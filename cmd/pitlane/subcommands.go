package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/TuroYT/pitlane/internal/cache"
)

// printVersion implements `pitlane version` (also --version and -v).
func (s system) printVersion() int {
	fmt.Fprintf(s.stdout, "pitlane %s\n", version)
	return 0
}

// cacheForward implements `pitlane cache-forward <listen> <target>`. Pitlane
// runs it itself, in the actions cache forwarder container (see
// internal/pool/actionscache.go), until SIGINT or SIGTERM.
func (s system) cacheForward(ctx context.Context, args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(s.stderr, "usage: pitlane cache-forward <listen> <target>\n")
		return 2
	}
	log := slog.New(slog.NewTextHandler(s.stderr, nil))
	ctx, stop := s.signals(ctx)
	defer stop()
	if err := cache.Forward(ctx, args[0], args[1], log); err != nil {
		log.Error("fatal", "error", err)
		return 1
	}
	return 0
}

// setupToken implements `pitlane setup-token [data dir]`: it prints the
// first-time setup token, which the daemon saves in the data directory
// rather than in its log.
func (s system) setupToken(args []string) int {
	dir := envOr(s.getenv, "PITLANE_DATA", defaultDataDir)
	if len(args) > 0 {
		dir = args[0]
	}
	// The operator's own command line chooses the directory.
	b, err := os.ReadFile(filepath.Join(dir, "setup-token")) //nolint:gosec // G703: local CLI argument, not remote input
	if err != nil {
		fmt.Fprintf(s.stderr, "no setup token: setup is already complete (or Pitlane never started)\n")
		return 1
	}
	fmt.Fprintf(s.stdout, "%s", b)
	return 0
}
