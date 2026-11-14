package scaler

import (
	"context"
	"errors"
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

func TestEnforceStrictAll(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pub, _ := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "pub", Enabled: true})
	unknown, _ := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "unknown", Enabled: true})
	priv, _ := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "priv", Enabled: true, Strict: true, StrictRequired: store.StrictPublicRepo})
	m := New(st, nil, t.TempDir(), func() Credentials { return Credentials{} }, func() {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.StrictReason = func(_ context.Context, tg store.Target) (string, error) {
		switch tg.ID {
		case pub.ID:
			return store.StrictPublicRepo, nil
		case priv.ID:
			return "", nil
		}
		return "", errors.New("github unreachable")
	}
	m.EnforceStrictAll(context.Background())
	if got, _ := st.Target(pub.ID); !got.Strict || got.StrictRequired != store.StrictPublicRepo || got.StrictForcedAt == 0 {
		t.Errorf("public repository not switched: %+v", got)
	}
	if got, _ := st.Target(unknown.ID); got.Strict {
		t.Errorf("a failed check must keep the current mode: %+v", got)
	}
	if got, _ := st.Target(priv.ID); !got.Strict || got.StrictRequired != "" {
		t.Errorf("a repository made private keeps strict, without the requirement: %+v", got)
	}
}
