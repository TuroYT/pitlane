package main

import (
	"context"
	"net/http"
	"time"
)

// Timeouts of both HTTP servers. There is deliberately no ReadTimeout or
// WriteTimeout: they would cut the long-lived event streams and the actions
// cache uploads and downloads, which can take minutes.
const (
	// readHeaderTimeout bounds how long a client may take to send its headers.
	readHeaderTimeout = 10 * time.Second
	// idleTimeout closes keep-alive connections nobody uses.
	idleTimeout = 2 * time.Minute
	// shutdownTimeout is how long in-flight requests get on shutdown before
	// the remaining connections (open browser tabs' event streams) are cut.
	shutdownTimeout = 3 * time.Second
)

// newHTTPServer is the http.Server both listeners (UI and actions cache) use.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout}
}

// listenAddr is the address http.Server.ListenAndServe would listen on: an
// empty one means the HTTP port.
func listenAddr(addr string) string {
	if addr == "" {
		return ":http"
	}
	return addr
}

// shutdownHTTP stops hs gracefully, then closes the connections still open
// after timeout.
func shutdownHTTP(hs *http.Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := hs.Shutdown(ctx); err != nil {
		_ = hs.Close()
	}
}
