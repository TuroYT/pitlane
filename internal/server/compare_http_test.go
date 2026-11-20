package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestCompareEndpoint(t *testing.T) {
	s, st, _, h := apiServer(t)
	for _, tg := range []store.Target{
		{Kind: "repo", Owner: "acme", Repo: "web", Enabled: true},
		{Kind: "repo", Owner: "acme", Repo: "down", Enabled: true}, // GitHub cannot answer
	} {
		if _, err := st.CreateTarget(tg); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int32
	s.jobHistory = func(_ context.Context, tg store.Target) ([]gh.JobRun, error) {
		calls.Add(1)
		if tg.Repo == "down" {
			return nil, errors.New("502 bad gateway")
		}
		return []gh.JobRun{
			{Workflow: "ci", Name: "test", RunnerName: "pitlane-1", Seconds: 10},
			{Workflow: "ci", Name: "test", RunnerName: "GitHub Actions 4", Seconds: 40},
		}, nil
	}
	rows := func(path, who string) ([]compareRow, int) {
		w := get(h, path, who)
		var out []compareRow
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatalf("%s: %v (%s)", path, err, w.Body)
			}
		}
		return out, w.Code
	}

	if _, code := rows("/api/compare", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
	got, code := rows("/api/compare", "vera")
	if code != http.StatusOK || len(got) != 1 || got[0].Repo != "acme/web" || got[0].HostedMedian != 40 || got[0].PitlaneMedian != 10 {
		t.Fatalf("%d %+v", code, got)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d repositories read, want 2", calls.Load())
	}
	// Served from the cache, until ?refresh=1.
	rows("/api/compare", "vera")
	if calls.Load() != 2 {
		t.Fatalf("a fresh comparison was computed again: %d reads", calls.Load())
	}
	rows("/api/compare?refresh=1", "vera")
	if calls.Load() != 4 {
		t.Fatalf("refresh did not read GitHub again: %d reads", calls.Load())
	}
	// Any other value of refresh does not skip the cache.
	rows("/api/compare?refresh=true", "vera")
	if calls.Load() != 4 {
		t.Fatalf("refresh=true skipped the cache: %d reads", calls.Load())
	}
}

// Without a GitHub App or token, the repositories cannot be read: the
// comparison is empty, not an error.
func TestCompareWithoutGitHubAccess(t *testing.T) {
	_, st, _, h := apiServer(t)
	if _, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "web", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	w := get(h, "/api/compare", "vera")
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
}
