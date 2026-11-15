package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// A finished runner's log is served from its archive; a name that is not a
// plain runner name never reaches the file system.
func TestHandleLogsArchived(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{InsecureNoAuth: true}, st, &pool.Pool{}, log, nil)
	s.SetManager(scaler.New(st, nil, t.TempDir(), func() scaler.Credentials { return scaler.Credentials{} }, func() {}, log))
	if err := os.WriteFile(s.mgr.LogFile("pitlane-ab12"), []byte("job output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/runners/x/logs", nil)
		r.SetPathValue("name", name)
		w := httptest.NewRecorder()
		s.handleLogs(w, r)
		return w
	}
	if w := get("pitlane-ab12"); w.Code != http.StatusOK || w.Body.String() != "job output\n" ||
		!strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("archived log: %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	if w := get("pitlane-gone"); w.Code != http.StatusNotFound {
		t.Fatalf("missing log: %d", w.Code)
	}
	for _, bad := range []string{"../master.key", "a/b", ""} {
		if w := get(bad); w.Code != http.StatusBadRequest {
			t.Errorf("name %q: %d, want 400", bad, w.Code)
		}
	}
}
