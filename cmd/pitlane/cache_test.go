package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartActionsCacheDisabled(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failListen := func(string, string) (net.Listener, error) { return nil, errors.New("address in use") }
	for _, tc := range []struct {
		name, dataDir, addr string
		listen              func(string, string) (net.Listener, error)
		wantLog             string
	}{
		{"empty", t.TempDir(), "", nil, ""},
		{"off", t.TempDir(), "off", nil, ""},
		{"no port", t.TempDir(), "127.0.0.1", nil, `msg="invalid --cache-listen address, actions cache disabled (jobs use GitHub's cache)" addr=127.0.0.1`},
		{"port zero", t.TempDir(), ":0", nil, "invalid --cache-listen address"},
		{"unusable data directory", file, ":8091", nil, `msg="actions cache disabled (jobs use GitHub's cache)" error=`},
		{"listen fails", t.TempDir(), ":8091", failListen, `error="address in use"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, buf := testLogger()
			if ac := startActionsCache(log, tc.dataDir, tc.addr, tc.listen); ac != nil {
				t.Fatal("cache started")
			}
			if tc.wantLog == "" && buf.String() != "" || !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log %q, want %q", buf.String(), tc.wantLog)
			}
		})
	}
}

func TestStartActionsCacheServesAndCloses(t *testing.T) {
	t.Parallel()
	var ln net.Listener
	listen := func(network, addr string) (net.Listener, error) {
		if addr != "127.0.0.1:8091" {
			t.Errorf("listening on %q", addr)
		}
		var err error
		ln, err = loopbackListen(t)(network, addr)
		return ln, err
	}
	log, buf := testLogger()
	dataDir := t.TempDir()
	ac := startActionsCache(log, dataDir, "127.0.0.1:8091", listen)
	if ac == nil {
		t.Fatalf("cache not started: %s", buf.String())
	}
	if ac.port != 8091 || ac.server() == nil {
		t.Errorf("port %d, server %v", ac.port, ac.server())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "actions-cache")); err != nil {
		t.Error(err)
	}
	if !strings.Contains(buf.String(), `msg="actions cache listening" addr=127.0.0.1:8091`) {
		t.Errorf("log %q", buf.String())
	}
	// Requests without a valid runner token are refused, but answered.
	resp, err := http.Get("http://" + ln.Addr().String() + "/x/o/r/_apis/artifactcache/cache")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	ac.close()
	if !refused(ln.Addr()) {
		t.Error("still listening after close")
	}
}

// failingListener fails Accept for good, as a dead socket would.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept: dead socket") }

func TestActionsCacheServeErrorIsLogged(t *testing.T) {
	t.Parallel()
	log, buf := testLogger()
	ac := startActionsCache(log, t.TempDir(), "127.0.0.1:8091", func(network, addr string) (net.Listener, error) {
		ln, err := loopbackListen(t)(network, addr)
		return failingListener{ln}, err
	})
	if ac == nil {
		t.Fatal("cache not started")
	}
	<-ac.done
	if !strings.Contains(buf.String(), `msg="actions cache server" error="accept: dead socket"`) {
		t.Errorf("log %q", buf.String())
	}
	ac.close() // after the server stopped on its own
}

func TestDisabledActionsCache(t *testing.T) {
	t.Parallel()
	var ac *actionsCache
	if ac.server() != nil {
		t.Error("server of a disabled cache")
	}
	ac.close() // no-op
}
