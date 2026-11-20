package main

import (
	"net"
	"net/http"
	"testing"
)

func TestNewHTTPServerTimeouts(t *testing.T) {
	t.Parallel()
	srv := newHTTPServer(":0", http.NotFoundHandler())
	if srv.Addr != ":0" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.IdleTimeout != idleTimeout {
		t.Errorf("ReadHeaderTimeout=%v IdleTimeout=%v, want %v and %v", srv.ReadHeaderTimeout, srv.IdleTimeout, readHeaderTimeout, idleTimeout)
	}
	// Event streams and cache transfers outlive any fixed deadline.
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Errorf("ReadTimeout=%v WriteTimeout=%v, want none", srv.ReadTimeout, srv.WriteTimeout)
	}
}

func TestListenAddr(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": ":http", ":8080": ":8080", "127.0.0.1:9": "127.0.0.1:9"} {
		if got := listenAddr(in); got != want {
			t.Errorf("listenAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// serveOn serves h on a loopback listener; the returned channel gets
// Serve's result.
func serveOn(t *testing.T, h http.Handler) (*http.Server, net.Addr, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := newHTTPServer("", h)
	done := make(chan error, 1)
	go func() { done <- hs.Serve(ln) }()
	return hs, ln.Addr(), done
}

func TestShutdownHTTPGraceful(t *testing.T) {
	t.Parallel()
	hs, addr, done := serveOn(t, http.NotFoundHandler())
	shutdownHTTP(hs, shutdownTimeout)
	if err := <-done; err != http.ErrServerClosed {
		t.Errorf("Serve returned %v", err)
	}
	if !refused(addr) {
		t.Error("still listening")
	}
}

// A request still running when the timeout ends (an open event stream) has
// its connection cut.
func TestShutdownHTTPCutsLongRequests(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	hs, addr, done := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	reqDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr.String())
		if err == nil {
			_, err = resp.Body.Read(make([]byte, 1))
			_ = resp.Body.Close()
		}
		reqDone <- err
	}()
	<-entered
	shutdownHTTP(hs, 0)
	if err := <-done; err != http.ErrServerClosed {
		t.Errorf("Serve returned %v", err)
	}
	if err := <-reqDone; err == nil {
		t.Error("the long request was not cut")
	}
}
