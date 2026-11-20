package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/client"
)

// dockerAPI is a fake Docker Engine API for the pool's Docker paths. Routes
// are keyed by "METHOD /path" (API version prefix and query dropped); any
// other call answers 501, so a test fails on calls it did not expect.
type dockerAPI struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	calls  []string
}

// newDockerAPI serves routes and points p at them.
func newDockerAPI(t *testing.T, p *Pool, routes map[string]http.HandlerFunc) *dockerAPI {
	t.Helper()
	d := &dockerAPI{t: t, routes: routes}
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	p.cli = cli
	return d
}

func (d *dockerAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path[1:], "/"); strings.HasPrefix(path, "/v") && i > 0 {
		path = path[i+1:]
	}
	key := r.Method + " " + path
	d.mu.Lock()
	d.calls = append(d.calls, key)
	h := d.routes[key]
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if h == nil {
		// A missing route is a hole in the test, not a Docker error: code
		// that discards the error would hide it. Explicit replyStatus routes
		// stand for the errors a test wants.
		d.t.Errorf("unexpected Docker API call %s", key)
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"message":"unexpected ` + key + `"}`))
		return
	}
	h(w, r)
}

// called returns the calls received, in order.
func (d *dockerAPI) called() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// reply answers v as JSON.
func reply(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(v) }
}

// replyStatus answers an empty body (or a Docker error message) with code.
func replyStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		if code >= 400 {
			_, _ = w.Write([]byte(`{"message":"fake error"}`))
		}
	}
}
