package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyDB creates, in dir, a database as an older Pitlane left it: only the
// first k migrations applied, with rows in every table that existed then.
func legacyDB(t *testing.T, dir string, k int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "pitlane.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("schema %d: %s: %v", k, q, err)
		}
	}
	exec(`CREATE TABLE schema_version (v INTEGER NOT NULL)`)
	for i := range k {
		exec(migrations[i])
		exec(`INSERT INTO schema_version (v) VALUES (?)`, i+1)
	}
	has := func(table string) bool {
		var n int
		_ = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
		return n == 1
	}
	if has("settings") {
		exec(`INSERT INTO settings (key, value) VALUES ('history_days', '12')`)
		exec(`INSERT INTO settings (key, value) VALUES ('allowed_users', 'alice, @Bob,,ALICE ,carol')`)
	}
	if has("targets") {
		exec(`INSERT INTO targets (kind, owner, repo, max_runners, scale_set_name, created_at) VALUES ('repo', 'acme', 'api', 3, 'pitlane-api', 1700000000)`)
	}
	if has("jobs") {
		exec(`INSERT INTO jobs (target_id, job_id, run_id, owner, repo, workflow_ref, job_name, runner_name, slot, started_at, finished_at, result)
			VALUES (1, 'j1', 7, 'acme', 'api', 'ci.yml', 'build', 'pitlane-a', 0, 1700000100, 1700000200, 'succeeded')`)
	}
	if has("sessions") {
		exec(`INSERT INTO sessions (token, login, expires_at) VALUES ('plain', 'alice', ?)`, time.Now().Add(time.Hour).Unix())
	}
	if has("runners") {
		exec(`INSERT INTO runners (runner_id, target_id, name, created_at) VALUES (55, 1, 'pitlane-a', 1700000000)`)
	}
}

// usersMigration is the number of migrations before the allow list is
// turned into users.
var usersMigration = func() int {
	for i, m := range migrations {
		if strings.Contains(m, "INSERT OR IGNORE INTO users") {
			return i
		}
	}
	panic("no users migration")
}()

// Every schema an earlier version may have left migrates to the latest one
// and keeps its data.
func TestMigrateFromEveryEarlierSchema(t *testing.T) {
	for k := 0; k <= len(migrations); k++ {
		dir := t.TempDir()
		legacyDB(t, dir, k)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("schema %d: %v", k, err)
		}
		var v int
		if err := s.db.QueryRow(`SELECT MAX(v) FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
			t.Fatalf("schema %d migrated to %d (%v), want %d", k, v, err, len(migrations))
		}
		if k >= 1 && s.GetInt("history_days", 30) != 12 {
			t.Errorf("schema %d: setting lost", k)
		}
		if k >= 2 {
			ts, err := s.Targets()
			if err != nil || len(ts) != 1 {
				t.Fatalf("schema %d: targets %v %v", k, ts, err)
			}
			tg := ts[0]
			if tg.Owner != "acme" || tg.Repo != "api" || tg.MaxRunners != 3 || tg.ScaleSetName != "pitlane-api" {
				t.Errorf("schema %d: target altered: %+v", k, tg)
			}
			if k < 7 && tg.MaxJobMinutes != DefaultMaxJobMinutes {
				t.Errorf("schema %d: new column default: max job minutes %d", k, tg.MaxJobMinutes)
			}
			if k < 10 && (tg.StrictRequired != "" || tg.StrictForcedAt != 0) {
				t.Errorf("schema %d: strict requirement defaults: %+v", k, tg)
			}
		}
		if k >= 3 {
			jobs, err := s.Jobs(context.Background(), 10)
			if err != nil || len(jobs) != 1 || jobs[0].JobID != "j1" || jobs[0].Result != "succeeded" || jobs[0].WorkflowRef != "ci.yml" {
				t.Fatalf("schema %d: jobs %+v %v", k, jobs, err)
			}
		}
		// Every login of the former allow list becomes an admin (unless the
		// users table already existed: the allow list was then a mirror).
		if k >= 1 && k <= usersMigration {
			users, err := s.Users()
			if err != nil || len(users) != 3 || users[0].Login != "alice" || users[1].Login != "Bob" || users[2].Login != "carol" {
				t.Fatalf("schema %d: users %+v %v", k, users, err)
			}
			for _, u := range users {
				if u.Role != RoleAdmin || u.CreatedBy != "migration" {
					t.Errorf("schema %d: migrated %+v", k, u)
				}
			}
		}
		if k >= 8 && s.RunnerIDByName("pitlane-a") != 55 {
			t.Errorf("schema %d: runner record lost", k)
		}
		// Sessions stored before tokens were hashed are dropped (sign in
		// again); a plaintext token must never be accepted as is.
		if _, ok := s.SessionLogin("plain"); ok {
			t.Errorf("schema %d: plaintext session still valid", k)
		}
		// The migrated database is fully usable.
		if err := s.CreateSession("tok", "bob", time.Hour); err != nil {
			t.Fatalf("schema %d: %v", k, err)
		}
		if _, err := s.CreateTarget(Target{Kind: "org", Owner: "other"}); err != nil {
			t.Fatalf("schema %d: %v", k, err)
		}
		_ = s.Close()

		// Opening again is a no-op.
		s, err = Open(dir)
		if err != nil {
			t.Fatalf("schema %d: reopen: %v", k, err)
		}
		var rows int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM schema_version`).Scan(&rows)
		if rows != len(migrations) {
			t.Errorf("schema %d: %d version rows after reopening, want %d", k, rows, len(migrations))
		}
		if login, ok := s.SessionLogin("tok"); !ok || login != "bob" {
			t.Errorf("schema %d: session lost on reopen", k)
		}
		_ = s.Close()
	}
}

// A database from a newer Pitlane (more migrations than this binary knows)
// opens without running anything.
func TestOpenNewerSchema(t *testing.T) {
	dir := t.TempDir()
	legacyDB(t, dir, len(migrations))
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "pitlane.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version (v) VALUES (?)`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if ts, err := s.Targets(); err != nil || len(ts) != 1 {
		t.Fatalf("targets %v %v", ts, err)
	}
}
