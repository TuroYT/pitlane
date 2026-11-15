package scaler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

const mib = int64(1) << 20

func TestEstimate(t *testing.T) {
	for _, tc := range []struct {
		samples []int64
		want    int64
		ok      bool
	}{
		{nil, 0, false},
		{[]int64{300}, 300, true},
		{[]int64{100, 900, 200, 300}, 900, true}, // under 5 runs: the highest
		{[]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 9, true},
		{[]int64{10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 10, 5000}, 10, true}, // one outlier in 20
		{[]int64{5, 1, 4, 2, 3}, 5, true},
	} {
		got, ok := estimate(tc.samples)
		if got != tc.want || ok != tc.ok {
			t.Errorf("estimate(%v) = %d, %v; want %d, %v", tc.samples, got, ok, tc.want, tc.ok)
		}
	}
}

// fakeHistory serves memory histories by level: "job:<repo>/<job>",
// "repo:<repo>" and "global".
type fakeHistory struct {
	data  map[string][]int64
	err   map[string]error
	calls int
}

func (f *fakeHistory) get(_ context.Context, owner, repo, workflow, job string, limit int) ([]int64, error) {
	f.calls++
	key := "global"
	switch {
	case workflow != "" || job != "":
		key = "job:" + owner + "/" + repo + "/" + workflow + "/" + job
	case repo != "":
		key = "repo:" + owner + "/" + repo
	}
	if err := f.err[key]; err != nil {
		return nil, err
	}
	v := f.data[key]
	if len(v) > limit {
		v = v[:limit]
	}
	return v, nil
}

func TestPredictorFallbacks(t *testing.T) {
	five := []int64{1000 * mib, 1000 * mib, 1000 * mib, 1000 * mib, 1000 * mib}
	for _, tc := range []struct {
		name   string
		data   map[string][]int64
		err    map[string]error
		key    jobKey
		want   int64
		source string
	}{
		{name: "same job", key: jobKey{"acme", "web", "ci.yml", "build"},
			data: map[string][]int64{"job:acme/web/ci.yml/build": {3100 * mib}, "repo:acme/web": {500 * mib}},
			want: 3100 * mib, source: "job"},
		{name: "new job: its repository", key: jobKey{"acme", "web", "ci.yml", "lint"},
			data: map[string][]int64{"repo:acme/web": {500 * mib, 2100 * mib}, "global": five},
			want: 2100 * mib, source: "repo"},
		{name: "new repository: every job", key: jobKey{"acme", "new", "ci.yml", "build"},
			data: map[string][]int64{"global": five},
			want: 1000 * mib, source: "global"},
		{name: "too few jobs overall: default", key: jobKey{"acme", "new", "ci.yml", "build"},
			data: map[string][]int64{"global": five[:4]},
			want: DefaultJobMemory, source: "default"},
		{name: "cold start", key: jobKey{"acme", "web", "ci.yml", "build"},
			want: DefaultJobMemory, source: "default"},
		{name: "warm runner of an organization", key: jobKey{owner: "acme"},
			data: map[string][]int64{"global": five},
			want: 1000 * mib, source: "global"},
		{name: "history error falls through", key: jobKey{"acme", "web", "ci.yml", "build"},
			data: map[string][]int64{"repo:acme/web": {700 * mib}},
			err:  map[string]error{"job:acme/web/ci.yml/build": errors.New("db locked")},
			want: 700 * mib, source: "repo"},
		{name: "floor", key: jobKey{"acme", "web", "ci.yml", "build"},
			data: map[string][]int64{"job:acme/web/ci.yml/build": {10 * mib}},
			want: minJobMemory, source: "job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeHistory{data: tc.data, err: tc.err}
			p := newPredictor(h.get)
			got, source := p.predict(context.Background(), tc.key)
			if got != tc.want || source != tc.source {
				t.Fatalf("got %d MiB from %s, want %d MiB from %s", got/mib, source, tc.want/mib, tc.source)
			}
		})
	}
}

func TestPredictorCache(t *testing.T) {
	h := &fakeHistory{data: map[string][]int64{"job:a/b/c/d": {900 * mib}}}
	p := newPredictor(h.get)
	now := time.Unix(1_000_000, 0)
	p.now = func() time.Time { return now }
	k := jobKey{"a", "b", "c", "d"}
	p.predict(context.Background(), k)
	p.predict(context.Background(), k)
	if h.calls != 1 {
		t.Fatalf("history read %d times, want 1 (cached)", h.calls)
	}
	h.data["job:a/b/c/d"] = []int64{1900 * mib}
	now = now.Add(predictTTL + time.Second)
	if got, _ := p.predict(context.Background(), k); got != 1900*mib {
		t.Fatalf("after the TTL: %d MiB", got/mib)
	}
	var nilP *predictor
	if got, src := nilP.predict(context.Background(), k); got != DefaultJobMemory || src != "default" {
		t.Fatal("nil predictor")
	}
}

func available(id, repo, job string) *scaleset.JobAvailable {
	return &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{JobID: id, OwnerName: "acme", RepositoryName: repo,
		JobWorkflowRef: "acme/" + repo + "/.github/workflows/ci.yml@refs/heads/main", JobDisplayName: job}}
}

// Pending jobs are predicted when GitHub assigns them; new runners reserve
// the heaviest first, and a runner's reservation becomes its job's once the
// job starts on it.
func TestReconcileReservesPendingJobs(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 8}, 8)
	hist := &fakeHistory{data: map[string][]int64{
		"job:acme/web/ci.yml/build": {3100 * mib},
		"job:acme/web/ci.yml/test":  {2100 * mib},
		"job:acme/web/ci.yml/lint":  {400 * mib},
	}}
	h.m.predict = newPredictor(hist.get)
	s := &scaler{t: h.t, session: h.api.session}
	ctx := context.Background()
	msg := &scaleset.RunnerScaleSetMessage{
		JobAvailableMessages: []*scaleset.JobAvailable{available("1", "web", "lint"), available("2", "web", "build"), available("3", "web", "test")},
		Statistics:           &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 3},
	}
	if err := s.Scale(ctx, msg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runners started", func() bool { return h.startingCount() == 0 && h.runnerCount() == 3 })
	h.p.mu.Lock()
	var got []int64
	for _, r := range h.p.requests {
		got = append(got, r.Predicted/mib)
	}
	h.p.mu.Unlock()
	if fmt.Sprint(got) != "[3100 2100 400]" {
		t.Fatalf("reservations %v MiB, want heaviest first", got)
	}
	// The lint job starts on the runner that reserved 3100 MiB.
	var name string
	h.t.mu.Lock()
	for n, r := range h.t.runners {
		if r.predicted == 3100*mib {
			name = n
		}
	}
	slot := h.t.runners[name].lease.Slot
	h.t.mu.Unlock()
	start := scaleset.JobStarted{RunnerName: name, JobMessageBase: available("1", "web", "lint").JobMessageBase}
	if err := s.Scale(ctx, &scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{&start}}); err != nil {
		t.Fatal(err)
	}
	h.p.mu.Lock()
	res := h.p.reserved[slot]
	h.p.mu.Unlock()
	if res != 400*mib {
		t.Fatalf("reservation after the job started: %d MiB, want 400", res/mib)
	}
	h.t.mu.Lock()
	pending := len(h.t.pending)
	h.t.mu.Unlock()
	if pending != 2 {
		t.Fatalf("%d pending jobs, want 2", pending)
	}
	for _, ts := range h.m.Snapshot() {
		for _, r := range ts.Runners {
			if r.Name == name && r.Predicted != 400*mib {
				t.Fatalf("snapshot predicted %d", r.Predicted)
			}
		}
	}
}

// Jobs refused for memory wait with that reason, shown in the snapshot; a
// target that got a runner while others still wait goes behind the others.
func TestReconcileWaitsForMemory(t *testing.T) {
	h := newHarness(t, store.Target{Kind: "org", Owner: "acme", Enabled: true, MaxRunners: 8}, 8)
	h.p.budget = 5 * 1024 * mib
	h.m.predict = newPredictor((&fakeHistory{data: map[string][]int64{"global": {3 << 30, 3 << 30, 3 << 30, 3 << 30, 3 << 30}}}).get)
	h.setDemand(3)
	h.t.mu.Lock()
	h.t.waiting, h.t.waitSince = 3, time.Now().Add(-time.Hour)
	h.t.mu.Unlock()
	h.t.reconcile()
	waitFor(t, "runner started", func() bool { return h.startingCount() == 0 && h.runnerCount() == 1 })
	h.t.mu.Lock()
	waiting, reason, since := h.t.waiting, h.t.waitReason, h.t.waitSince
	h.t.mu.Unlock()
	if waiting != 2 || reason != pool.WaitMemory {
		t.Fatalf("waiting %d for %q, want 2 for memory", waiting, reason)
	}
	if time.Since(since) > time.Minute {
		t.Fatal("a target that got a runner must go behind the others")
	}
	var found bool
	for _, ts := range h.m.Snapshot() {
		if ts.ID == h.t.cfg().ID {
			found = ts.WaitReason == pool.WaitMemory && ts.Waiting == 2
		}
	}
	if !found {
		t.Fatal("snapshot does not show the memory wait")
	}
	// Demand served: no reason left.
	h.setDemand(1)
	h.t.reconcile()
	h.t.mu.Lock()
	reason = h.t.waitReason
	h.t.mu.Unlock()
	if reason != "" {
		t.Fatalf("reason %q with nothing waiting", reason)
	}
}
