package store

import (
	"context"
	"testing"
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
