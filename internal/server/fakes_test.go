package server

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// fakePool is a runnerPool that needs no Docker: it serves what the test
// sets and records what the server asks of it.
type fakePool struct {
	mu          sync.Mutex
	cfg         pool.Config
	strictSlots int
	mem         pool.MemoryState
	slots       []pool.SlotState
	wipes       map[string]int64
	pingErr     error
	diskTotal   int64
	diskFree    int64
	diskErr     error
	logText     string
	logErr      error
	jobOutput   string
	followed    bool // Logs was asked to follow
	configErr   error
	// configured receives every configuration Configure is given.
	configured chan pool.Config
}

func newFakePool() *fakePool {
	return &fakePool{
		cfg:        pool.Config{Slots: 4, CacheScope: "owner", Mirror: true, KeepBuildGB: 20, MinFreeDiskGB: 20, StrictSlots: pool.AutoStrictSlots},
		configured: make(chan pool.Config, 8),
		diskErr:    io.ErrUnexpectedEOF, // disk space unknown unless a test sets it
	}
}

func (f *fakePool) Config() pool.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakePool) Configure(_ context.Context, cfg pool.Config) error {
	f.mu.Lock()
	f.cfg = cfg
	err := f.configErr
	f.mu.Unlock()
	f.configured <- cfg
	return err
}

func (f *fakePool) StrictSlots() int { return f.strictSlots }

func (f *fakePool) Memory() pool.MemoryState { return f.mem }

func (f *fakePool) Slots() []pool.SlotState { return f.slots }

func (f *fakePool) SlotWipes() map[string]int64 { return f.wipes }

func (f *fakePool) Ping(context.Context) error { return f.pingErr }

func (f *fakePool) DiskSpace() (total, free int64, err error) {
	return f.diskTotal, f.diskFree, f.diskErr
}

func (f *fakePool) Logs(_ context.Context, _ string, follow bool, w io.Writer) error {
	f.mu.Lock()
	f.followed = follow
	f.mu.Unlock()
	_, _ = io.WriteString(w, f.logText)
	return f.logErr
}

func (f *fakePool) TailJobOutput(_ context.Context, _ string, w io.Writer) error {
	_, err := io.WriteString(w, f.jobOutput)
	return err
}

// apiServer is rolesServer (alice admin, oscar operator, vera viewer, olga
// viewer through the acme rule, each signed in with a session named after
// them) with a fake pool instead of a Docker one.
func apiServer(t *testing.T) (*Server, *store.Store, *fakePool, http.Handler) {
	t.Helper()
	s, st := rolesServer(t)
	fp := newFakePool()
	s.pool = fp
	return s, st, fp, s.Handler()
}

// dropTable removes a table behind the store's back, so the queries reading
// it fail while the rest of the store keeps working.
func dropTable(t *testing.T, dataDir, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "pitlane.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE " + table); err != nil {
		t.Fatalf("drop %s: %v", table, err)
	}
}

// storeAt opens a store whose directory the test keeps (see dropTable).
func storeAt(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// get runs a GET with the session of who ("" for none) through h.
func get(h http.Handler, target, who string) *httptest.ResponseRecorder {
	if who == "" {
		return send(h, "GET", target, "")
	}
	return send(h, "GET", target, "", session(who))
}

// bodyHas reports whether the response body contains want.
func bodyHas(w *httptest.ResponseRecorder, want string) bool {
	return strings.Contains(w.Body.String(), want)
}
