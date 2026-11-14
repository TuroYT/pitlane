package store

import (
	"context"
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
