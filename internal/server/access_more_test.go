package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestUserAndRuleValidation(t *testing.T) {
	t.Parallel()
	_, st, _, h := apiServer(t)
	admin := session("alice")
	long := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, method, path, body string
		code                     int
		msg                      string
	}{
		{"user: not JSON", "PUT", "/api/users/bob", `nope`, 400, "invalid JSON body"},
		{"user: body too big", "PUT", "/api/users/bob", `{"role":"viewer","pad":"` + strings.Repeat("x", maxAccessBody) + `"}`, 400, "invalid JSON body"},
		{"user: login too long", "PUT", "/api/users/" + long, `{"role":"viewer"}`, 400, "invalid GitHub login"},
		{"user: login with a dot", "PUT", "/api/users/a.b", `{"role":"viewer"}`, 400, "invalid GitHub login"},
		{"user: only an @", "PUT", "/api/users/@", `{"role":"viewer"}`, 400, "invalid GitHub login"},
		{"user: no role", "PUT", "/api/users/bob", `{}`, 400, "role must be"},
		{"user: role in capitals", "PUT", "/api/users/bob", `{"role":"Admin"}`, 400, "role must be"},
		{"rule: not JSON", "POST", "/api/access-rules", `[`, 400, "invalid JSON body"},
		{"rule: no organization", "POST", "/api/access-rules", `{"role":"viewer"}`, 400, "invalid organization"},
		{"rule: organization with a dot", "POST", "/api/access-rules", `{"org":"a.b","role":"viewer"}`, 400, "invalid organization"},
		{"rule: team with a slash", "POST", "/api/access-rules", `{"org":"acme","team":"a/b","role":"viewer"}`, 400, "invalid organization or team"},
		{"rule: no role", "POST", "/api/access-rules", `{"org":"acme"}`, 400, "role must be"},
		{"rule: id is not a number", "DELETE", "/api/access-rules/abc", ``, 404, "not found"},
	} {
		if w := send(h, tc.method, tc.path, tc.body, admin); w.Code != tc.code || !bodyHas(w, tc.msg) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, w.Code, w.Body, tc.code, tc.msg)
		}
	}
	if users, _ := st.Users(); len(users) != 3 {
		t.Errorf("refused requests changed the users: %+v", users)
	}
	if rules, _ := st.AccessRules(); len(rules) != 1 {
		t.Errorf("refused requests changed the rules: %+v", rules)
	}
	if _, total, _ := st.AuditPage(t.Context(), 1, 0); total != 0 {
		t.Errorf("refused requests were audited (%d entries)", total)
	}
}

func TestRuleForms(t *testing.T) {
	t.Parallel()
	_, st, _, h := apiServer(t)
	admin := session("alice")
	// "org/team" in the org field, an org with a team field, and a plain
	// org all make a rule; the same subject twice updates its role.
	for _, body := range []string{`{"org":" acme/ops ","role":"operator"}`, `{"org":"acme","team":" dev ","role":"viewer"}`, `{"org":"other","role":"admin"}`, `{"org":"acme/ops","role":"admin"}`} {
		if w := send(h, "POST", "/api/access-rules", body, admin); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	rules, _ := st.AccessRules()
	got := map[string]string{}
	for _, r := range rules {
		got[r.Subject()] = r.Role
	}
	want := map[string]string{"acme": "viewer", "acme/ops": "admin", "acme/dev": "viewer", "other": "admin"}
	if len(got) != len(want) {
		t.Fatalf("rules %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("rule %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestAuditPaging(t *testing.T) {
	t.Parallel()
	_, st, _, h := apiServer(t)
	for i := range 60 {
		if err := st.AddAudit(store.AuditEntry{Actor: "alice", Action: "x", Subject: strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		query     string
		wantLen   int
		wantTop   string // subject of the first entry
		wantTotal string
	}{
		{"", 50, "59", "60"},
		{"?limit=10", 10, "59", "60"},
		{"?limit=10&offset=55", 5, "4", "60"},
		{"?limit=0", 50, "59", "60"},   // not a page size: the default
		{"?limit=501", 50, "59", "60"}, // over the maximum: the default
		{"?limit=500", 60, "59", "60"}, // the maximum itself is fine
		{"?limit=abc&offset=zzz", 50, "59", "60"},
		{"?limit=5&offset=-3", 5, "59", "60"}, // a negative offset is the start
	} {
		w := get(h, "/api/audit"+tc.query, "alice")
		var entries []store.AuditEntry
		if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || w.Code != http.StatusOK || len(entries) != tc.wantLen ||
			w.Header().Get("X-Total-Count") != tc.wantTotal || (len(entries) > 0 && entries[0].Subject != tc.wantTop) {
			t.Errorf("%q: %d, %d entries, total %q (%v)", tc.query, w.Code, len(entries), w.Header().Get("X-Total-Count"), err)
		}
	}
	for _, who := range []string{"", "vera", "oscar"} {
		if w := get(h, "/api/audit", who); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%q read the audit log: %d", who, w.Code)
		}
	}
}

func TestAccessStoreFailures(t *testing.T) {
	t.Parallel()
	t.Run("the rules cannot be listed", func(t *testing.T) {
		s, _, dir, logs := serverAt(t)
		dropTable(t, dir, "access_rules")
		checkInternalErr(t, get(s.Handler(), "/api/users", ""), logs, http.StatusInternalServerError, "rules_list", "no such table")
	})
	t.Run("a user cannot be saved", func(t *testing.T) {
		s, st, dir, logs := serverAt(t)
		execSQL(t, dir, `CREATE TRIGGER no_users BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		w := send(s.Handler(), "PUT", "/api/users/bob", `{"role":"viewer"}`)
		checkInternalErr(t, w, logs, http.StatusInternalServerError, "access_save", "disk full")
		if st.HasUsers() {
			t.Error("user saved")
		}
	})
	t.Run("an access rule cannot be saved", func(t *testing.T) {
		s, _, dir, logs := serverAt(t)
		execSQL(t, dir, `CREATE TRIGGER no_rules BEFORE INSERT ON access_rules BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		w := send(s.Handler(), "POST", "/api/access-rules", `{"org":"acme","role":"viewer"}`)
		checkInternalErr(t, w, logs, http.StatusInternalServerError, "access_save", "disk full")
	})
	t.Run("the audit log cannot be written", func(t *testing.T) {
		s, st, dir, logs := serverAt(t)
		dropTable(t, dir, "audit")
		// The change itself is made, and the failure of the log is logged.
		if w := send(s.Handler(), "PUT", "/api/users/bob", `{"role":"viewer"}`); w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		if _, ok := st.User("bob"); !ok || !strings.Contains(logs.String(), "write audit log") {
			t.Errorf("user saved: %v, log:\n%s", ok, logs)
		}
	})
	t.Run("the audit log cannot be read", func(t *testing.T) {
		s, _, dir, logs := serverAt(t)
		dropTable(t, dir, "audit")
		checkInternalErr(t, get(s.Handler(), "/api/audit", ""), logs, http.StatusInternalServerError, "audit_list", "no such table")
	})
}

// A user whose role comes from rules that can no longer be read gets no
// access: failing closed.
func TestRuleSessionFailsClosedWhenRulesAreUnreadable(t *testing.T) {
	t.Parallel()
	s, st, dir, logs := serverAt(t)
	s.cfg.InsecureNoAuth = false
	if err := st.CreateSession("tok", "olga", time.Hour, "acme"); err != nil {
		t.Fatal(err)
	}
	if w := get(s.Handler(), "/api/jobs", "x"); w.Code != http.StatusUnauthorized {
		t.Fatalf("setup: %d", w.Code)
	}
	if _, err := st.PutAccessRule(store.AccessRule{Org: "acme", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if w := send(s.Handler(), "GET", "/api/jobs", "", session("tok")); w.Code != http.StatusOK {
		t.Fatalf("with the rule: %d", w.Code)
	}
	dropTable(t, dir, "access_rules")
	if w := send(s.Handler(), "GET", "/api/jobs", "", session("tok")); w.Code != http.StatusUnauthorized {
		t.Fatalf("rules unreadable: %d", w.Code)
	}
	if !strings.Contains(logs.String(), "load access rules") {
		t.Errorf("not logged:\n%s", logs)
	}
	// The same at sign-in: nobody is let in on a rule that cannot be read.
	s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
		return "dave", "user-token", nil
	}
	if role, matched := s.signInRole(t.Context(), "dave", "user-token"); role != "" || matched != nil {
		t.Fatalf("role %q %v", role, matched)
	}
}

func TestSignInRoleEdgeCases(t *testing.T) {
	t.Parallel()
	s, st := authServer(t, "dave")
	// No rule at all, or no user token: no role, and no GitHub call.
	called := false
	s.member = func(context.Context, string, string, string, string) (bool, error) { called = true; return true, nil }
	if role, _ := s.signInRole(t.Context(), "dave", "tok"); role != "" || called {
		t.Fatalf("no rules: %q (called=%v)", role, called)
	}
	if _, err := st.PutAccessRule(store.AccessRule{Org: "acme", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if role, _ := s.signInRole(t.Context(), "dave", ""); role != "" || called {
		t.Fatalf("no token: %q (called=%v)", role, called)
	}
	// The same organization is asked once however many rules name it (a
	// team and its organization are two subjects).
	for _, r := range []store.AccessRule{{Org: "acme", Team: "a", Role: store.RoleOperator}, {Org: "acme", Team: "b", Role: store.RoleAdmin}} {
		if _, err := st.PutAccessRule(r); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.member = func(_ context.Context, _, _, org, team string) (bool, error) { calls++; return team != "b", nil }
	role, matched := s.signInRole(t.Context(), "dave", "tok")
	if role != store.RoleOperator || len(matched) != 2 || calls != 3 {
		t.Fatalf("role %q matched %v after %d checks", role, matched, calls)
	}
}
