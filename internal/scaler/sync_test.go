package scaler

import (
	"io"
	"log/slog"
	"testing"

	"github.com/TuroYT/pitlane/internal/store"
)

// A target change while slots are still starting (HTTP is served before
// Start) must not start a listener: Start does the sync once it is ready.
func TestSyncBeforeStartIsDeferred(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateTarget(store.Target{Kind: "org", Owner: "acme", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	notified := 0
	m := New(st, nil, t.TempDir(), func() Credentials { return Credentials{PAT: "x"} }, func() { notified++ },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Sync()
	m.Restart()
	m.mu.Lock()
	n := len(m.targets)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("listener started before Start: %d targets", n)
	}
	if notified == 0 {
		t.Fatal("the UI must still be notified")
	}
}
