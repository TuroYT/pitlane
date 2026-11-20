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

// actionsCache is the running actions cache: its store and the HTTP server
// runners reach it on. A nil *actionsCache is a disabled cache.
type actionsCache struct {
	store *cache.Server
	http  *http.Server
	port  int
	done  chan struct{} // closed when the HTTP server has stopped
}

// startActionsCache opens the actions cache and starts its listener. On any
// failure Pitlane runs without it (nil): jobs use GitHub's cache.
func startActionsCache(log *slog.Logger, dataDir, addr string, listen func(network, address string) (net.Listener, error)) *actionsCache {
	if addr == "" || addr == "off" {
		return nil
	}
	port := pool.CachePort(addr)
	if port == 0 {
		log.Error("invalid --cache-listen address, actions cache disabled (jobs use GitHub's cache)", "addr", addr)
		return nil
	}
	cs, err := cache.Open(filepath.Join(dataDir, "actions-cache"), log)
	if err != nil {
		log.Error("actions cache disabled (jobs use GitHub's cache)", "error", err)
		return nil
	}
	ln, err := listen("tcp", addr)
	if err != nil {
		log.Error("actions cache disabled (jobs use GitHub's cache)", "error", err)
		return nil
	}
	ac := &actionsCache{store: cs, http: newHTTPServer("", cs.Handler()), port: port, done: make(chan struct{})}
	log.Info("actions cache listening", "addr", addr)
	go ac.serve(log, ln)
	return ac
}

func (ac *actionsCache) serve(log *slog.Logger, ln net.Listener) {
	defer close(ac.done)
	if err := ac.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("actions cache server", "error", err)
	}
}

// server is the cache store, nil when the cache is disabled.
func (ac *actionsCache) server() *cache.Server {
	if ac == nil {
		return nil
	}
	return ac.store
}

// close stops the cache listener and waits for its server. Busy runners
// keep running: their transfers are retried by the client (or the step
// goes on without the cache).
func (ac *actionsCache) close() {
	if ac == nil {
		return
	}
	_ = ac.http.Close()
	<-ac.done
}
