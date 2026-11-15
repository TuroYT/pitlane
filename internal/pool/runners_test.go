package pool

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

// fakeDocker serves the few Docker API calls StartRunner makes and records
// the containers removed.
type fakeDocker struct {
	mu      sync.Mutex
	removed []string
	create  func(w http.ResponseWriter, r *http.Request)
	start   func(w http.ResponseWriter, r *http.Request)
	remove  int // status returned by DELETE (0: 204)
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if i := strings.Index(path[1:], "/"); strings.HasPrefix(path, "/v") && i > 0 {
		path = path[i+1:] // drop the API version
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && path == "/containers/create":
		if f.create != nil {
			f.create(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"Id":"c1"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		f.start(w, r)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
		f.mu.Lock()
		f.removed = append(f.removed, strings.TrimPrefix(path, "/containers/"))
		f.mu.Unlock()
		if f.remove != 0 {
			w.WriteHeader(f.remove)
			_, _ = io.WriteString(w, `{"message":"boom"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"message":"unexpected `+r.Method+" "+path+`"}`, http.StatusNotImplemented)
	}
}

func (f *fakeDocker) removedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

func dockerPool(t *testing.T, f *fakeDocker) (*Pool, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	p := newTestPool("owner", SlotState{Index: 0, Status: "busy"})
	var logs bytes.Buffer
	p.cli, p.log = cli, slog.New(slog.NewTextHandler(&logs, nil))
	for _, v := range []string{"pitlane-cache-acme", "pitlane-toolcache-acme"} {
		p.chowned[v] = true // no volume preparation
	}
	return p, &logs
}

// hang cancels the caller's context and holds the request until the client
// gives up on it (the body is read first: the server notices a closed
// connection only after that).
func hang(r *http.Request, cancel context.CancelFunc) {
	_, _ = io.Copy(io.Discard, r.Body)
	cancel()
	select {
	case <-r.Context().Done():
	case <-time.After(5 * time.Second):
	}
}

var testSpec = RunnerSpec{Name: "pitlane-test", JITConfig: "jit", TargetID: 1, Owner: "acme", Lease: Lease{Slot: 0, Owner: "acme", TargetID: 1}}

// A target stopped while its runner starts cancels the start: the created
// container must still be removed, not left with its JIT config.
func TestStartRunnerCanceledRemovesContainer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeDocker{start: func(w http.ResponseWriter, r *http.Request) {
		hang(r, cancel)
	}}
	p, _ := dockerPool(t, f)
	if _, err := p.StartRunner(ctx, testSpec); err == nil {
		t.Fatal("start must fail")
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("removed %v, want [c1]", got)
	}
}

// A create canceled half-way may still have created the container on the
// daemon: it is removed by name.
func TestStartRunnerCanceledCreateRemovesByName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeDocker{create: func(w http.ResponseWriter, r *http.Request) {
		hang(r, cancel)
	}}
	p, _ := dockerPool(t, f)
	if _, err := p.StartRunner(ctx, testSpec); err == nil {
		t.Fatal("start must fail")
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != testSpec.Name {
		t.Fatalf("removed %v, want [%s]", got, testSpec.Name)
	}
}

func TestStartRunnerStartErrorRemovesContainer(t *testing.T) {
	f := &fakeDocker{start: func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"no such image"}`, http.StatusInternalServerError)
	}}
	p, _ := dockerPool(t, f)
	if _, err := p.StartRunner(context.Background(), testSpec); err == nil || !strings.Contains(err.Error(), "start runner") {
		t.Fatalf("err = %v", err)
	}
	if got := f.removedIDs(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("removed %v, want [c1]", got)
	}
}

func TestRemoveLogsFailuresOnly(t *testing.T) {
	for _, c := range []struct {
		status int
		logged bool
	}{{0, false}, {http.StatusNotFound, false}, {http.StatusConflict, false}, {http.StatusInternalServerError, true}} {
		f := &fakeDocker{remove: c.status}
		p, logs := dockerPool(t, f)
		p.Remove(context.Background(), "c1")
		if got := strings.Contains(logs.String(), "remove container"); got != c.logged {
			t.Errorf("status %d: logged=%v, want %v (%s)", c.status, got, c.logged, logs.String())
		}
	}
}
