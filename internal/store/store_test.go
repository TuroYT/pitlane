package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSecretsRoundTrip(t *testing.T) {
	s := open(t)
	if err := s.SetSecret("app_pem", "-----BEGIN KEY-----"); err != nil {
		t.Fatal(err)
	}
	if raw := s.Get("app_pem", ""); raw == "" || raw == "-----BEGIN KEY-----" {
		t.Fatalf("secret stored in clear: %q", raw)
	}
	got, err := s.GetSecret("app_pem")
	if err != nil || got != "-----BEGIN KEY-----" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestUpsertJobKeepsKnownFields(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", RunID: 42, JobName: "build", Slot: -1, QueuedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", RunnerName: "pitlane-x", Slot: 3, StartedAt: 105}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", Slot: -1, FinishedAt: 130, Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.Jobs(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs=%v err=%v", jobs, err)
	}
	j := jobs[0]
	if j.RunID != 42 || j.JobName != "build" || j.RunnerName != "pitlane-x" || j.Slot != 3 ||
		j.QueuedAt != 100 || j.StartedAt != 105 || j.FinishedAt != 130 || j.Result != "succeeded" {
		t.Fatalf("unexpected merge: %+v", j)
	}
}

func TestJobWorkflowRef(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	full := "acme/api/.github/workflows/ci.yml@refs/pull/42/merge"
	if err := s.UpsertJob(Job{JobID: "j1", RunID: 7, WorkflowRef: "ci.yml", WorkflowFullRef: full, Event: "pull_request", Slot: -1, QueuedAt: 100}); err != nil {
		t.Fatal(err)
	}
	// A later message without the reference must not erase it.
	if err := s.UpsertJob(Job{JobID: "j1", RunnerName: "pitlane-x", Slot: 1, StartedAt: 105}); err != nil {
		t.Fatal(err)
	}
	j, err := s.JobByRunner(ctx, "pitlane-x")
	if err != nil {
		t.Fatal(err)
	}
	if j.WorkflowFullRef != full || j.Ref != "refs/pull/42/merge" || j.WorkflowRef != "ci.yml" || j.Event != "pull_request" {
		t.Fatalf("unexpected job: %+v", j)
	}
	jobs, err := s.Jobs(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].Ref != "refs/pull/42/merge" {
		t.Fatalf("jobs=%+v err=%v", jobs, err)
	}
}

func TestWorkflowRefParts(t *testing.T) {
	for _, c := range []struct{ in, path, ref string }{
		{"acme/api/.github/workflows/ci.yml@refs/heads/main", "acme/api/.github/workflows/ci.yml", "refs/heads/main"},
		{"acme/api/.github/workflows/ci.yml@refs/heads/feat@2", "acme/api/.github/workflows/ci.yml", "refs/heads/feat@2"},
		{"acme/api/.github/workflows/ci.yml", "acme/api/.github/workflows/ci.yml", ""},
		{"", "", ""},
	} {
		if p, r := WorkflowRefParts(c.in); p != c.path || r != c.ref {
			t.Errorf("WorkflowRefParts(%q) = %q, %q", c.in, p, r)
		}
	}
}

func TestTargets(t *testing.T) {
	s := open(t)
	created, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if created.ScaleSetName != "pitlane" || created.MaxRunners != 4 {
		t.Fatalf("defaults not applied: %+v", created)
	}
	if _, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api"}); err == nil {
		t.Fatal("duplicate target accepted")
	}
	if created.URL() != "https://github.com/acme/api" {
		t.Fatal(created.URL())
	}
}

func TestTargetMaxJobMinutes(t *testing.T) {
	s := open(t)
	created, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api", MaxJobMinutes: DefaultMaxJobMinutes})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Target(created.ID)
	if err != nil || got.MaxJobMinutes != 360 {
		t.Fatalf("got %+v, %v", got, err)
	}
	got.MaxJobMinutes = 0 // unlimited
	if err := s.UpdateTarget(got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Target(created.ID); got.MaxJobMinutes != 0 {
		t.Fatalf("unlimited not stored: %+v", got)
	}
	// Rows created before the migration get the default.
	if _, err := s.db.Exec(`INSERT INTO targets (kind, owner, repo, created_at) VALUES ('org', 'old', '', 0)`); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.Targets()
	for _, x := range ts {
		if x.Owner == "old" && x.MaxJobMinutes != DefaultMaxJobMinutes {
			t.Fatalf("column default not applied: %+v", x)
		}
	}
}

func TestRegisteredRunners(t *testing.T) {
	s := open(t)
	if err := s.AddRunner(RegisteredRunner{ID: 7, TargetID: 1, Name: "pitlane-a"}); err != nil {
		t.Fatal(err)
	}
	_ = s.AddRunner(RegisteredRunner{ID: 8, TargetID: 2, Name: "pitlane-b"})
	if id := s.RunnerIDByName("pitlane-a"); id != 7 {
		t.Fatal(id)
	}
	if id := s.RunnerIDByName("nope"); id != 0 {
		t.Fatal(id)
	}
	rs, err := s.RegisteredRunners(1)
	if err != nil || len(rs) != 1 || rs[0].Name != "pitlane-a" || rs[0].CreatedAt == 0 {
		t.Fatalf("%+v %v", rs, err)
	}
	_ = s.DeleteRunner(7)
	if rs, _ := s.RegisteredRunners(1); len(rs) != 0 {
		t.Fatalf("not deleted: %+v", rs)
	}
}

func TestJobMetrics(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	_ = s.UpsertJob(Job{JobID: "a", Slot: -1, QueuedAt: 1000, StartedAt: 1010, FinishedAt: 1100, Result: "succeeded"})
	_ = s.UpsertJob(Job{JobID: "b", Slot: -1, QueuedAt: 1000, StartedAt: 1030, FinishedAt: 1060, Result: "failed"})
	_ = s.UpsertJob(Job{JobID: "c", Slot: -1, QueuedAt: 1000, StartedAt: 1005})                                  // running
	_ = s.UpsertJob(Job{JobID: "d", Slot: -1, QueuedAt: 10, StartedAt: 20, FinishedAt: 30, Result: "succeeded"}) // too old
	counts, err := s.JobResultCounts(ctx)
	if err != nil || counts["succeeded"] != 2 || counts["failed"] != 1 || len(counts) != 2 {
		t.Fatalf("%v %v", counts, err)
	}
	d, w, err := s.JobTimings(ctx, 500)
	if err != nil || len(d) != 2 || len(w) != 3 || d[0]+d[1] != 120 || w[0]+w[1]+w[2] != 45 {
		t.Fatalf("durations %v waits %v err %v", d, w, err)
	}
}

func TestInFlightJobs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, j := range []Job{
		{TargetID: 1, JobID: "running", RunnerName: "pitlane-a", Slot: -1, StartedAt: 100},
		{TargetID: 1, JobID: "done", RunnerName: "pitlane-b", Slot: -1, StartedAt: 100, FinishedAt: 200, Result: "succeeded"},
		{TargetID: 1, JobID: "unregistered", RunnerName: "pitlane-c", Slot: -1, StartedAt: 100},
		{TargetID: 1, JobID: "queued", Slot: -1, QueuedAt: 100},
	} {
		if err := s.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	for i, name := range []string{"pitlane-a", "pitlane-b"} {
		if err := s.AddRunner(RegisteredRunner{ID: int64(i + 1), TargetID: 1, Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := s.InFlightJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].JobID != "running" || jobs[0].RunnerName != "pitlane-a" || jobs[0].TargetID != 1 {
		t.Fatalf("got %+v", jobs)
	}
}

func TestSessionsStoreTokenHash(t *testing.T) {
	s := open(t)
	if err := s.CreateSession("secret-token", "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT token FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "secret-token" || stored != sessionKey("secret-token") {
		t.Fatalf("token stored as %q", stored)
	}
	if login, ok := s.SessionLogin("secret-token"); !ok || login != "alice" {
		t.Fatalf("lookup by token failed: %q %v", login, ok)
	}
	if _, ok := s.SessionLogin(stored); ok {
		t.Fatal("the stored hash must not work as a token")
	}
	if _, ok := s.SessionLogin(""); ok {
		t.Fatal("empty token accepted")
	}
	if err := s.DeleteSession("secret-token"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.SessionLogin("secret-token"); ok {
		t.Fatal("deleted session still valid")
	}
}

func TestPruneSessions(t *testing.T) {
	s := open(t)
	if err := s.CreateSession("old", "alice", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("new", "bob", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneSessions(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sessions left: %d %v", n, err)
	}
	if _, ok := s.SessionLogin("new"); !ok {
		t.Fatal("valid session pruned")
	}
}

func TestOpenRestrictsPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pitlane.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("data dir mode %v", st.Mode().Perm())
	}
	for _, f := range []string{"pitlane.db", "pitlane.db-wal", "pitlane.db-shm"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v %v", f, st.Mode().Perm(), err)
		}
	}
}

func TestOpenDataDirOwnedByAnotherUser(t *testing.T) {
	defer func(c func(string, os.FileMode) error, d func(string) (int, os.FileMode, error)) {
		chmod, dirInfo = c, d
	}(chmod, dirInfo)
	chmod = func(string, os.FileMode) error { return os.ErrPermission }
	other := os.Geteuid() + 1

	// Not ours (e.g. a volume owned by another user), not writable by
	// others: logged, not fatal, and the database files are still private.
	dirInfo = func(string) (int, os.FileMode, error) { return other, os.ModeDir | 0o750, nil }
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("a data dir owned by another user must not prevent the start: %v", err)
	}
	st, err := os.Stat(filepath.Join(dir, "pitlane.db"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("database mode %v %v", st.Mode().Perm(), err)
	}
	s.Close()

	for name, info := range map[string]func(string) (int, os.FileMode, error){
		"ours but chmod fails":       func(string) (int, os.FileMode, error) { return os.Geteuid(), os.ModeDir | 0o755, nil },
		"group-writable, not ours":   func(string) (int, os.FileMode, error) { return other, os.ModeDir | 0o770, nil },
		"world-writable, not ours":   func(string) (int, os.FileMode, error) { return other, os.ModeDir | 0o757, nil },
		"owner cannot be determined": func(string) (int, os.FileMode, error) { return 0, 0, errors.New("stat failed") },
	} {
		dirInfo = info
		if _, err := Open(t.TempDir()); err == nil {
			t.Errorf("%s: Open must fail", name)
		}
	}
}

func TestStrictRequirement(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tg, err := s.CreateTarget(Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if flipped, err := s.SetStrictRequirement(tg.ID, "public repository"); err != nil || !flipped {
		t.Fatalf("flipped=%v err=%v", flipped, err)
	}
	got, _ := s.Target(tg.ID)
	if !got.Strict || got.StrictRequired != "public repository" || got.StrictForcedAt == 0 {
		t.Fatalf("not switched to strict: %+v", got)
	}
	if flipped, _ := s.SetStrictRequirement(tg.ID, "public repository"); flipped {
		t.Fatal("already strict: not flipped again")
	}
	if _, err := s.SetStrictRequirement(tg.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Target(tg.ID)
	if !got.Strict || got.StrictRequired != "" {
		t.Fatalf("clearing the requirement must keep strict on: %+v", got)
	}
	// Other writers do not touch the requirement.
	if _, err := s.SetStrictRequirement(tg.ID, "public repository"); err != nil {
		t.Fatal(err)
	}
	got.MaxRunners = 7
	if err := s.UpdateTarget(got); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScaleSetID(tg.ID, 42); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Target(tg.ID)
	if got.StrictRequired != "public repository" || got.ScaleSetID != 42 || got.MaxRunners != 7 || !got.Strict {
		t.Fatalf("%+v", got)
	}
	// A writer holding a copy read before the requirement cannot turn
	// strict off.
	got.Strict = false
	if err := s.UpdateTarget(got); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.Target(tg.ID); !got.Strict {
		t.Fatal("strict written off while required")
	}
}

func TestJobsPage(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	add := func(id, owner, repo, ref, result string, queued, finished int64) {
		t.Helper()
		if err := s.UpsertJob(Job{TargetID: 1, JobID: id, Owner: owner, Repo: repo, Slot: -1, QueuedAt: queued, FinishedAt: finished, Result: result,
			WorkflowFullRef: owner + "/" + repo + "/.github/workflows/ci.yml@" + ref}); err != nil {
			t.Fatal(err)
		}
	}
	add("a", "acme", "api", "refs/heads/main", "succeeded", 100, 110)
	add("b", "acme", "api", "refs/heads/feat/Login", "Failed", 200, 210)
	add("c", "acme", "web", "refs/pull/12/merge", "", 300, 0)
	add("d", "other", "x", "refs/heads/main", "succeeded", 400, 410)

	ids := func(f JobFilter, limit, offset int) (string, int) {
		t.Helper()
		jobs, total, err := s.JobsPage(ctx, f, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, j := range jobs {
			out += j.JobID
		}
		return out, total
	}
	for name, tc := range map[string]struct {
		f      JobFilter
		limit  int
		offset int
		want   string
		total  int
	}{
		"newest first":        {JobFilter{}, 10, 0, "dcba", 4},
		"page 2":              {JobFilter{}, 2, 2, "ba", 4},
		"past the end":        {JobFilter{}, 2, 10, "", 4},
		"repository":          {JobFilter{Owner: "ACME", Repo: "api"}, 10, 0, "ba", 2},
		"running":             {JobFilter{Result: "running"}, 10, 0, "c", 1},
		"result, any case":    {JobFilter{Result: "failed"}, 10, 0, "b", 1},
		"branch substring":    {JobFilter{Ref: "login"}, 10, 0, "b", 1},
		"pull request number": {JobFilter{Ref: "pull/12"}, 10, 0, "c", 1},
		"ref only, not path":  {JobFilter{Ref: "workflows"}, 10, 0, "", 0},
		"filters combine":     {JobFilter{Owner: "acme", Repo: "api", Ref: "main", Result: "succeeded"}, 10, 0, "a", 1},
	} {
		got, total := ids(tc.f, tc.limit, tc.offset)
		if got != tc.want || total != tc.total {
			t.Errorf("%s: got %q (total %d), want %q (total %d)", name, got, total, tc.want, tc.total)
		}
	}
	repos, err := s.JobRepos(ctx)
	if err != nil || strings.Join(repos, ",") != "acme/api,acme/web,other/x" {
		t.Fatalf("repos %v %v", repos, err)
	}
}

func TestJobUsage(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	i := func(v int64) *int64 { return &v }
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j1", RunnerName: "pitlane-a", Slot: 0, QueuedAt: 100, StartedAt: 110, FinishedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertJob(Job{TargetID: 1, JobID: "j2", RunnerName: "pitlane-b", Slot: 1, QueuedAt: 100, StartedAt: 110, FinishedAt: 200}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.SetJobUsage(ctx, "pitlane-a", JobUsage{WallMs: 10_000,
		Runner: ContainerUsage{MemPeak: i(1 << 30), CPUUsec: i(5_000_000), CPUSomeUsec: i(1_000_000), IOFullUsec: i(0)},
		Slot:   ContainerUsage{MemPeak: i(2 << 30), CPUUsec: i(1_000_000)}})
	if err != nil || !ok {
		t.Fatalf("SetJobUsage: %v, %v", ok, err)
	}
	if ok, err := s.SetJobUsage(ctx, "pitlane-unknown", JobUsage{WallMs: 1}); err != nil || ok {
		t.Fatalf("unknown runner: %v, %v", ok, err)
	}
	j, err := s.JobByRunner(ctx, "pitlane-a")
	if err != nil {
		t.Fatal(err)
	}
	u := j.Usage
	if u == nil || u.WallMs != 10_000 || *u.MemPeak != 3<<30 || *u.Runner.CPUUsec != 5_000_000 || u.Runner.MemFullUsec != nil {
		t.Fatalf("usage: %+v", u)
	}
	if *u.Runner.CPUPressure != 10 || *u.Runner.IOPressure != 0 || u.Runner.MemPressure != nil || u.Slot.CPUPressure != nil {
		t.Errorf("pressure: %+v", u.Runner)
	}
	jobs, err := s.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if (j.Usage != nil) != (j.RunnerName == "pitlane-a") {
			t.Errorf("job %s usage %+v", j.JobID, j.Usage)
		}
	}
	peaks, err := s.JobMemoryPeaks(ctx, 150)
	if err != nil || len(peaks) != 1 || peaks[0] != 3<<30 {
		t.Errorf("peaks: %v, %v", peaks, err)
	}
	if peaks, _ := s.JobMemoryPeaks(ctx, 300); len(peaks) != 0 {
		t.Errorf("peaks after: %v", peaks)
	}
}

func TestMemoryHistory(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	i := func(v int64) *int64 { return &v }
	add := func(n int, owner, repo, wf, job, result string, runner, slot, anon *int64) {
		name := fmt.Sprintf("r-%d", n)
		if err := s.UpsertJob(Job{TargetID: 1, JobID: fmt.Sprint(n), Owner: owner, Repo: repo, WorkflowRef: wf, JobName: job,
			RunnerName: name, Slot: 0, StartedAt: 1, FinishedAt: 2, Result: result}); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.SetJobUsage(ctx, name, JobUsage{WallMs: 1, Runner: ContainerUsage{MemPeak: runner}, Slot: ContainerUsage{MemPeak: slot, AnonPeak: anon}}); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	add(1, "acme", "web", "ci.yml", "build", "success", i(3000), i(8000), nil)    // old row: slot/4
	add(2, "acme", "web", "ci.yml", "build", "success", i(3100), i(9000), i(500)) // anon known
	add(3, "acme", "web", "ci.yml", "build", "canceled", i(10), nil, nil)         // ignored
	add(4, "acme", "web", "ci.yml", "test", "failure", i(2100), nil, nil)         // other job
	add(5, "other", "web", "ci.yml", "build", "success", i(9999), nil, nil)       // other owner
	add(6, "acme", "api", "ci.yml", "build", "success", nil, nil, nil)            // not measured
	for _, tc := range []struct {
		name                 string
		owner, repo, wf, job string
		want                 []int64
	}{
		{"job", "acme", "web", "ci.yml", "build", []int64{3600, 5000}},
		{"repo", "acme", "web", "", "", []int64{2100, 3600, 5000}},
		{"global", "", "", "", "", []int64{9999, 2100, 3600, 5000}},
		{"unknown job", "acme", "web", "ci.yml", "lint", nil},
	} {
		got, err := s.MemoryHistory(ctx, tc.owner, tc.repo, tc.wf, tc.job, 10)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if got, _ := s.MemoryHistory(ctx, "", "", "", "", 1); len(got) != 1 {
		t.Errorf("limit: %v", got)
	}
}

func TestPruneJobsKeepsRecentJobsWithoutQueueTime(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Unix()
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	for _, j := range []Job{
		{JobID: "recent-no-queue", StartedAt: now, FinishedAt: now}, // GitHub sent no queue time
		{JobID: "no-dates"}, // nothing known: kept
		{JobID: "old", QueuedAt: old, StartedAt: old, FinishedAt: old},          // pruned
		{JobID: "queued-long-ago-finished-now", QueuedAt: old, FinishedAt: now}, // still recent
	} {
		j.TargetID, j.Slot = 1, -1
		if err := s.UpsertJob(j); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PruneJobs(30 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	jobs, _, err := s.JobsPage(context.Background(), JobFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, j := range jobs {
		kept = append(kept, j.JobID)
	}
	slices.Sort(kept)
	if want := []string{"no-dates", "queued-long-ago-finished-now", "recent-no-queue"}; !slices.Equal(kept, want) {
		t.Fatalf("kept %v, want %v", kept, want)
	}
}

func TestUpsertJobKeepsEarliestQueueTime(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []int64{100, 0, 150, 90} {
		if err := s.UpsertJob(Job{TargetID: 1, JobID: "j", Slot: -1, QueuedAt: q}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _, _ := s.JobsPage(context.Background(), JobFilter{}, 10, 0)
	if len(jobs) != 1 || jobs[0].QueuedAt != 90 {
		t.Fatalf("queued_at %+v, want the earliest known (90)", jobs)
	}
}
