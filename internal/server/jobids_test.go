package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestJobIDCacheIsBounded(t *testing.T) {
	c := jobIDCache{max: 3}
	for i := range 3 {
		c.put(jobKey{"r" + strconv.Itoa(i), 1}, int64(i))
	}
	if _, ok := c.get(jobKey{"r0", 1}); !ok { // r0 is now the most recent
		t.Fatal("r0 missing")
	}
	c.put(jobKey{"r3", 1}, 3) // drops r1, the least recently used
	if _, ok := c.get(jobKey{"r1", 1}); ok {
		t.Fatal("least recently used entry kept")
	}
	for _, k := range []string{"r0", "r2", "r3"} {
		if _, ok := c.get(jobKey{k, 1}); !ok {
			t.Fatalf("%s dropped", k)
		}
	}
	c.put(jobKey{"r3", 1}, 33) // update in place
	if id, _ := c.get(jobKey{"r3", 1}); id != 33 || c.len() != 3 {
		t.Fatalf("update: id %d, %d entries", id, c.len())
	}
	if _, ok := c.get(jobKey{"r3", 2}); ok {
		t.Fatal("another run of the same runner name hit the cache")
	}

	var d jobIDCache // zero value: default bound
	for i := range maxJobIDs + 100 {
		d.put(jobKey{"r", int64(i)}, int64(i))
	}
	if d.len() != maxJobIDs {
		t.Fatalf("%d entries, want %d", d.len(), maxJobIDs)
	}
}

// Lookups of runners the store does not know, or whose job GitHub does not
// find, add nothing to the cache.
func TestGitHubJobCachesOnlyFoundJobs(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s := New(Config{}, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	_ = st.UpsertJob(store.Job{TargetID: 99, JobID: "j1", RunID: 123, Owner: "acme", Repo: "api", RunnerName: "pitlane-a", Slot: -1})
	for i := range 50 {
		name := "pitlane-x" + strconv.Itoa(i)
		r := httptest.NewRequest("GET", "/api/runners/"+name+"/github", nil)
		r.SetPathValue("name", name)
		w := httptest.NewRecorder()
		s.handleGitHubJob(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", name, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/api/runners/pitlane-a/github", nil)
	r.SetPathValue("name", "pitlane-a")
	s.handleGitHubJob(httptest.NewRecorder(), r) // target unknown: run page
	if n := s.jobIDs.len(); n != 0 {
		t.Fatalf("%d entries cached", n)
	}
}
