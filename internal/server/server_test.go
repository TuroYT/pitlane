package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"pitlane", "my-repo", "a.b_c", "TuroYT"} {
		if !validName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "x y", "a;rm", string(make([]byte, 101))} {
		if validName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestNormalizeUsers(t *testing.T) {
	if got := normalizeUsers(" @alice, bob\nAlice  carol,"); got != "alice,bob,carol" {
		t.Fatalf("got %q", got)
	}
}

func TestMedian(t *testing.T) {
	if m := median([]float64{5, 1, 3}); m != 3 {
		t.Fatal(m)
	}
	if m := median(nil); m != 0 {
		t.Fatal(m)
	}
}

func TestCSRFRequiresHeader(t *testing.T) {
	s := &Server{}
	h := s.csrf(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		method, path, header string
		want                 int
	}{
		{"POST", "/api/targets", "", http.StatusForbidden},
		{"DELETE", "/api/targets/1", "", http.StatusForbidden},
		{"POST", "/api/targets", "1", 204},
		{"GET", "/api/state", "", 204},
		{"GET", "/setup/callback", "", 204},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.header != "" {
			r.Header.Set("X-Pitlane", tc.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s header=%q: got %d want %d", tc.method, tc.path, tc.header, w.Code, tc.want)
		}
	}
}

func TestNewTargetDefaults(t *testing.T) {
	tg, opts, err := newTarget(strings.NewReader(`{"kind":"repo","owner":"acme","repo":"api"}`), 4)
	if err != nil {
		t.Fatal(err)
	}
	if opts.MinRunners != nil || opts.Strict != nil {
		t.Fatalf("nothing asked: %+v", opts)
	}
	if tg.MinRunners != store.DefaultMinRunners || tg.MaxRunners != 4 || tg.MaxJobMinutes != store.DefaultMaxJobMinutes || !tg.Enabled {
		t.Fatalf("defaults: %+v", tg)
	}
	tg, _, _ = newTarget(strings.NewReader(`{"kind":"org","owner":"acme","repo":"x","minRunners":0}`), 4)
	if tg.MinRunners != 0 || tg.Repo != "" {
		t.Fatalf("explicit 0 warm runners not kept: %+v", tg)
	}
	tg, _, _ = newTarget(strings.NewReader(`{"kind":"repo","owner":"acme","repo":"api","minRunners":9,"maxRunners":2}`), 4)
	if tg.MinRunners != 2 {
		t.Fatalf("warm runners not capped by max parallel: %+v", tg)
	}
	if _, _, err := newTarget(strings.NewReader(`{"kind":"repo","owner":"acme"}`), 4); err == nil {
		t.Fatal("repo target without a repository accepted")
	}
}

func TestGitHubJobRedirect(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(Config{}, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	_ = st.UpsertJob(store.Job{TargetID: 99, JobID: "j1", RunID: 123, Owner: "acme", Repo: "api", RunnerName: "pitlane-a", Slot: -1})
	_ = st.UpsertJob(store.Job{TargetID: 99, JobID: "j2", RunID: 124, Owner: "acme", Repo: "api", RunnerName: "pitlane-b", Slot: -1})
	s.jobIDs.Store("pitlane-b", int64(555))

	get := func(name string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/runners/"+name+"/github", nil)
		r.SetPathValue("name", name)
		w := httptest.NewRecorder()
		s.handleGitHubJob(w, r)
		return w
	}
	// Unknown target: falls back to the run page.
	if w := get("pitlane-a"); w.Code != http.StatusFound || w.Header().Get("Location") != "https://github.com/acme/api/actions/runs/123" {
		t.Fatalf("fallback: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("pitlane-b"); w.Header().Get("Location") != "https://github.com/acme/api/actions/runs/124/job/555" {
		t.Fatalf("cached: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get("pitlane-none"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown runner: %d", w.Code)
	}
}

// The actions cache is on by default and its settings survive a round trip.
func TestActionsCacheSettings(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if c := LoadPoolConfig(st, t.TempDir()); !c.ActionsCache || c.ActionsCacheGB != 20 {
		t.Fatalf("defaults: %v %d", c.ActionsCache, c.ActionsCacheGB)
	}
	_ = st.Set("actions_cache", "false")
	_ = st.Set("actions_cache_gb", "50")
	if c := LoadPoolConfig(st, t.TempDir()); c.ActionsCache || c.ActionsCacheGB != 50 {
		t.Fatalf("stored: %v %d", c.ActionsCache, c.ActionsCacheGB)
	}
}
