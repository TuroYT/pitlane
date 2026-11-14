package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memStore) Get(k, def string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[k]; ok {
		return v
	}
	return def
}

func (s *memStore) Set(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[k] = v
	return nil
}

type fakeImages struct {
	mu      sync.Mutex
	id      string
	pulls   int
	pullErr error
}

func (f *fakeImages) Pull(ctx context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls++
	if f.pullErr != nil {
		return f.pullErr
	}
	f.id = "sha256:new"
	return nil
}

func (f *fakeImages) Inspect(ctx context.Context, ref string) (ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return ImageInfo{ID: f.id, Digest: "ghcr.io/x/runner@" + f.id}, nil
}

func (f *fakeImages) RunnerVersion(ctx context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.id == "sha256:new" {
		return "2.340.0", nil
	}
	return "2.339.0", nil
}

func newTestUpdater(t *testing.T, pitlane, runner []Release) (*Updater, *fakeImages, *memStore) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/repos/"+Repo+"/"):
			if pitlane == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(pitlane)
		case strings.Contains(r.URL.Path, "/repos/"+RunnerRepo+"/"):
			_ = json.NewEncoder(w).Encode(runner)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	img := &fakeImages{id: "sha256:old"}
	st := &memStore{}
	p := fakeProbe()
	p.Getenv = func(k string) string {
		if k == "INVOCATION_ID" {
			return "x"
		}
		return ""
	}
	u := &Updater{
		Version: "0.2.0", Store: st, Images: img, Probe: p,
		GitHub:      &GitHub{HTTP: srv.Client(), APIBase: srv.URL, Hosts: []string{srv.Listener.Addr().String()}},
		RunnerImage: func() string { return "ghcr.io/x/runner:latest" },
		Now:         func() time.Time { return at(3) },
	}
	return u, img, st
}

func TestUpdaterCheckAndState(t *testing.T) {
	now := at(3)
	u, img, st := newTestUpdater(t,
		[]Release{{Tag: "v1.0.0", HTMLURL: "https://github.com/TuroYT/pitlane/releases/tag/v1.0.0"}, {Tag: "v0.2.1"}, {Tag: "v0.3.0-rc.1", Prerelease: true}},
		[]Release{{Tag: "v2.340.0", PublishedAt: now.Add(-24 * time.Hour)}, {Tag: "v2.339.0", PublishedAt: now.Add(-60 * 24 * time.Hour)}})
	u.Check(context.Background())
	s := u.State()
	if s.Pitlane.Latest != "v1.0.0" || !s.Pitlane.UpdateAvailable || s.Pitlane.AutoCandidate != "v0.2.1" || s.Pitlane.LatestURL == "" {
		t.Errorf("pitlane state %+v", s.Pitlane)
	}
	if !s.Pitlane.Capability.CanSelfUpdate || s.Pitlane.Capability.Mode != ModeSystemd || s.Pitlane.ManualCommand != "" {
		t.Errorf("capability %+v", s.Pitlane)
	}
	if s.Runner.Version != "2.339.0" || s.Runner.Latest != "v2.340.0" || s.Runner.Status != RunnerOutdated {
		t.Errorf("runner state %+v", s.Runner)
	}
	if !s.Settings.AutoPull || s.Settings.AutoUpdate || s.Settings.Window != (Window{3, 5}) {
		t.Errorf("default settings %+v", s.Settings)
	}

	if err := u.PullRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = u.State()
	if s.Runner.ImageID != "sha256:new" || s.Runner.Version != "2.340.0" || s.Runner.Status != RunnerCurrent || s.Runner.PulledAt != now.Unix() {
		t.Errorf("after pull %+v", s.Runner)
	}

	// Auto-pull is daily: a second tick right after does not pull again.
	img.pulls = 0
	u.tick(context.Background())
	if img.pulls != 0 {
		t.Errorf("pulled again %d times", img.pulls)
	}
	_ = st.Set(keyPulledAt, "1")
	u.tick(context.Background())
	if img.pulls != 1 {
		t.Errorf("daily pull not done: %d", img.pulls)
	}
}

func TestUpdaterPrivateRepo(t *testing.T) {
	u, _, _ := newTestUpdater(t, nil, nil)
	u.Check(context.Background())
	s := u.State()
	if !strings.Contains(s.Pitlane.CheckError, "private") || s.Pitlane.UpdateAvailable {
		t.Errorf("state %+v", s.Pitlane)
	}
	if err := u.Apply(false); err == nil {
		t.Error("apply without any release")
	}
}

func TestUpdaterPullError(t *testing.T) {
	u, img, _ := newTestUpdater(t, nil, nil)
	img.pullErr = errors.New("manifest unknown")
	if err := u.PullRunner(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if s := u.State(); s.Runner.PullError == "" || s.Runner.Pulling || s.Runner.PulledAt != 0 {
		t.Errorf("state %+v", s.Runner)
	}
}

func TestUpdaterAutoUpdateWaitsForIdle(t *testing.T) {
	u, _, st := newTestUpdater(t, []Release{{Tag: "v0.2.1"}}, nil)
	u.Check(context.Background())
	_ = st.Set(keyAutoPull, "false")
	busy := true
	u.Busy = func() bool { return busy }
	if err := u.SaveSettings(Settings{AutoUpdate: true, Window: Window{3, 5}}); err != nil {
		t.Fatal(err)
	}
	u.tick(context.Background())
	if s := u.State(); s.Pitlane.Phase != "" || s.Pitlane.AutoStatus != "a job is running" {
		t.Errorf("state %+v", s.Pitlane)
	}
	if err := u.SaveSettings(Settings{Window: Window{3, 24}}); err == nil {
		t.Error("invalid window accepted")
	}
}

func TestApplyRefusedWhenSelfUpdateImpossible(t *testing.T) {
	u, _, _ := newTestUpdater(t, []Release{{Tag: "v0.3.0"}}, nil)
	u.Probe.Exists = func(f string) bool { return f == "/.dockerenv" }
	u.Check(context.Background())
	err := u.Apply(false)
	if err == nil || !strings.Contains(err.Error(), "docker compose pull") {
		t.Errorf("err %v", err)
	}
	if s := u.State(); s.Pitlane.ManualCommand == "" {
		t.Error("no manual command")
	}
}
