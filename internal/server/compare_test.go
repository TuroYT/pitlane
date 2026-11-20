package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

func TestCompareRows(t *testing.T) {
	got := compareRows([]repoJobs{
		{Repo: "acme/web", Jobs: []gh.JobRun{
			{Workflow: "ci", Name: "test", RunnerName: "pitlane-1", Seconds: 30},
			{Workflow: "ci", Name: "test", RunnerName: "pitlane-2", Seconds: 10},
			{Workflow: "ci", Name: "test", RunnerName: "pitlane-3", Seconds: 20},
			{Workflow: "ci", Name: "test", RunnerName: "GitHub Actions 12", Seconds: 90},
			{Workflow: "ci", Name: "lint", RunnerName: "my-own-runner", Seconds: 5}, // neither side
		}},
		{Repo: "acme/api", Jobs: []gh.JobRun{
			{Workflow: "ci", Name: "build", RunnerName: "GitHub Actions 3", Seconds: 60},
		}},
		{}, // a repository GitHub did not answer for
	})
	want := []compareRow{
		{Repo: "acme/api", Workflow: "ci", Job: "build", HostedMedian: 60, HostedCount: 1},
		{Repo: "acme/web", Workflow: "ci", Job: "test", HostedMedian: 90, HostedCount: 1, PitlaneMedian: 20, PitlaneCount: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if rows := compareRows(nil); rows == nil || len(rows) != 0 {
		t.Fatalf("no jobs: %#v, want an empty list (JSON [])", rows)
	}
}

func TestMedianLeavesInputAlone(t *testing.T) {
	v := []float64{5, 1, 3, 2}
	if m := median(v); m != 3 {
		t.Fatalf("median %v", m)
	}
	if !reflect.DeepEqual(v, []float64{5, 1, 3, 2}) {
		t.Fatalf("input changed: %v", v)
	}
}

func TestCompareCache(t *testing.T) {
	var c compareCache
	calls := 0
	compute := func() ([]compareRow, error) {
		calls++
		return []compareRow{{Repo: "acme/web"}}, nil
	}
	for range 2 {
		if _, err := c.get(false, compute); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("fresh rows recomputed: %d calls", calls)
	}
	if _, _ = c.get(true, compute); calls != 2 {
		t.Fatalf("refresh did not recompute: %d calls", calls)
	}
	c.at = time.Now().Add(-compareTTL)
	if _, _ = c.get(false, compute); calls != 3 {
		t.Fatalf("stale rows served: %d calls", calls)
	}
	boom := errors.New("boom")
	if _, err := c.get(true, func() ([]compareRow, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("error lost: %v", err)
	}
	if rows, _ := c.get(false, compute); calls != 3 || len(rows) != 1 {
		t.Fatalf("failed compute dropped the cache: %d calls, %v", calls, rows)
	}
}

func TestHandleCompare(t *testing.T) {
	s, st, _ := newErrServer(t)
	for _, tg := range []store.Target{
		{Kind: "repo", Owner: "acme", Repo: "web", Enabled: true},
		{Kind: "repo", Owner: "acme", Repo: "off"},  // disabled: not read
		{Kind: "org", Owner: "acme", Enabled: true}, // organisation: not read
	} {
		if _, err := st.CreateTarget(tg); err != nil {
			t.Fatal(err)
		}
	}
	var read []string
	s.jobHistory = func(_ context.Context, tg store.Target) ([]gh.JobRun, error) {
		read = append(read, tg.FullName())
		return []gh.JobRun{{Workflow: "ci", Name: "test", RunnerName: "pitlane-1", Seconds: 12}}, nil
	}
	w := httptest.NewRecorder()
	s.handleCompare(w, httptest.NewRequest("GET", "/api/compare", nil))
	var rows []compareRow
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body, err)
	}
	if !reflect.DeepEqual(read, []string{"acme/web"}) {
		t.Fatalf("read %v, want only the enabled repository", read)
	}
	if len(rows) != 1 || rows[0].Repo != "acme/web" || rows[0].PitlaneMedian != 12 {
		t.Fatalf("rows %+v", rows)
	}
}
