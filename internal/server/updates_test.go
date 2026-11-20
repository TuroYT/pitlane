package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A body longer than the audit cap reaches the update API whole.
func TestUpdatesForwardsWholeBody(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{InsecureNoAuth: true}}
	var got string
	s.upd = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusBadRequest) // no audit entry: no store needed
	})
	body := `{"x":"` + strings.Repeat("a", 2*maxUpdatesAudit) + `"}`
	s.handleUpdates(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/updates/preferences", strings.NewReader(body)))
	if got != body {
		t.Fatalf("forwarded %d bytes, want %d", len(got), len(body))
	}
}
