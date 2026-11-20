package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/pool"
)

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
	hs := newHTTPServer("", cs.Handler()) // the listener is already open
	go func() {
		log.Info("actions cache listening", "addr", listen)
		if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("actions cache server", "error", err)
		}
	}()
	return cs, hs, port
}
