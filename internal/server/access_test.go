package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// rolesServer is a server with an admin (alice), an operator (oscar), a
// viewer (vera) and a viewer through an organization rule (olga, member of
// acme), each signed in with a session named after them.
func rolesServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{DataDir: t.TempDir(), BaseURL: "http://pitlane.test"}, st, &pool.Pool{}, log, nil)
	s.strictCheck = func(context.Context, store.Target) (string, error) { return "", nil }
	s.SetManager(scaler.New(st, nil, t.TempDir(), func() scaler.Credentials { return scaler.Credentials{} }, func() {}, log))
	_ = st.Set("app_client_id", "Iv1.test")
	addUser(t, st, "alice", store.RoleAdmin)
	addUser(t, st, "oscar", store.RoleOperator)
	addUser(t, st, "vera", store.RoleViewer)
	if _, err := st.PutAccessRule(store.AccessRule{Org: "acme", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"alice", "oscar", "vera"} {
		_ = st.CreateSession(l, l, time.Hour)
	}
	_ = st.CreateSession("olga", "olga", time.Hour, "acme")
	return s, st
}

func session(name string) *http.Cookie { return &http.Cookie{Name: sessionCookie, Value: name} }

// send runs a request with a JSON body as the UI does.
func send(h http.Handler, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-Pitlane", "1")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Every route, for every role: who gets through (200 from a stub handler),
// who is refused (403) and who must sign in (401).
func TestAuthorizationMatrix(t *testing.T) {
	s, _ := rolesServer(t)
	hit := map[string]bool{}
	h := s.handler(func(rt route) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hit[rt.pattern] = true
			w.Header().Set("X-Route", rt.pattern)
			w.WriteHeader(http.StatusOK)
		}
	})
	const (
		pub = iota // anyone
		vw         // viewer and up
		op         // operator and up
		ad         // admin only
	)
	cases := []struct {
		method, path string
		min          int
	}{
		{"GET", "/healthz", pub},
		{"GET", "/readyz", pub},
		{"GET", "/metrics", pub}, // checks the token or session itself (metrics_test.go)
		{"GET", "/api/status", pub},
		{"POST", "/api/setup/unlock", pub},
		{"POST", "/api/setup/manifest", pub}, // checks the setup token or an admin itself (TestCanManageApp)
		{"GET", "/setup/callback", pub},
		{"GET", "/auth/login", pub},
		{"GET", "/auth/callback", pub},
		{"POST", "/api/logout", pub},
		{"GET", "/", pub},
		{"GET", "/jobs", pub}, // the UI itself

		{"GET", "/api/state", vw},
		{"GET", "/api/events", vw},
		{"GET", "/api/jobs", vw},
		{"GET", "/api/jobs/repos", vw},
		{"GET", "/api/jobs/memory", vw},
		{"GET", "/api/runners/pitlane-a/logs", vw},
		{"GET", "/api/runners/pitlane-a/job-log", vw},
		{"GET", "/api/runners/pitlane-a/github", vw},
		{"GET", "/api/compare", vw},

		{"PATCH", "/api/targets/1", op}, // operators: enabled only (TestOperatorTargetChanges)
		{"GET", "/api/updates", op},
		{"POST", "/api/updates/check", op},
		{"POST", "/api/updates/pull-runner", op},

		{"GET", "/api/installations", ad},
		{"POST", "/api/targets", ad},
		{"DELETE", "/api/targets/1", ad},
		{"GET", "/api/settings", ad},
		{"PUT", "/api/settings", ad},
		{"POST", "/api/updates/apply", ad},
		{"PUT", "/api/updates/settings", ad},
		{"POST", "/api/updates", ad},
		{"GET", "/api/users", ad},
		{"PUT", "/api/users/bob", ad},
		{"DELETE", "/api/users/bob", ad},
		{"POST", "/api/access-rules", ad},
		{"DELETE", "/api/access-rules/1", ad},
		{"GET", "/api/audit", ad},
	}
	who := []struct {
		name string
		rank int
		c    *http.Cookie
	}{
		{"anonymous", 0, nil},
		{"forged session", 0, session("nobody")},
		{"viewer", vw, session("vera")},
		{"viewer through an org rule", vw, session("olga")},
		{"operator", op, session("oscar")},
		{"admin", ad, session("alice")},
	}
	for _, tc := range cases {
		for _, u := range who {
			var cs []*http.Cookie
			if u.c != nil {
				cs = append(cs, u.c)
			}
			w := send(h, tc.method, tc.path, "{}", cs...)
			want := http.StatusOK
			switch {
			case tc.min == pub:
			case u.rank == 0:
				want = http.StatusUnauthorized
			case u.rank < tc.min:
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Errorf("%s %s as %s: %d, want %d (%s)", tc.method, tc.path, u.name, w.Code, want, strings.TrimSpace(w.Body.String()))
			}
		}
	}
	// A new route must be added to this matrix.
	for _, rt := range s.routes() {
		if !hit[rt.pattern] {
			t.Errorf("route %q is not in the authorization matrix", rt.pattern)
		}
	}
}

// Roles are read again on every request.
func TestRoleChangesApplyAtOnce(t *testing.T) {
	s, st := rolesServer(t)
	h := s.Handler()
	if w := send(h, "GET", "/api/users", "", session("alice")); w.Code != http.StatusOK {
		t.Fatalf("admin: %d", w.Code)
	}
	addUser(t, st, "oscar", store.RoleAdmin)
	if _, err := st.PutUser(store.User{Login: "alice", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if w := send(h, "GET", "/api/users", "", session("alice")); w.Code != http.StatusForbidden {
		t.Fatalf("demoted admin: %d", w.Code)
	}
	if w := send(h, "GET", "/api/jobs", "", session("alice")); w.Code != http.StatusOK {
		t.Fatalf("demoted admin, read: %d", w.Code)
	}
	if _, err := st.DeleteUser("vera"); err != nil {
		t.Fatal(err)
	}
	if w := send(h, "GET", "/api/jobs", "", session("vera")); w.Code != http.StatusUnauthorized {
		t.Fatalf("removed user: %d", w.Code)
	}
	// A user given access again must sign in again: their sessions went
	// with them.
	addUser(t, st, "vera", store.RoleViewer)
	if w := send(h, "GET", "/api/jobs", "", session("vera")); w.Code != http.StatusUnauthorized {
		t.Fatalf("re-added user, old session: %d", w.Code)
	}
	// Removing the rule signs its members out.
	rules, _ := st.AccessRules()
	if _, err := st.DeleteAccessRule(rules[0].ID); err != nil {
		t.Fatal(err)
	}
	if w := send(h, "GET", "/api/jobs", "", session("olga")); w.Code != http.StatusUnauthorized {
		t.Fatalf("rule removed: %d", w.Code)
	}
}

func TestStatusReportsRole(t *testing.T) {
	s, _ := rolesServer(t)
	w := send(s.Handler(), "GET", "/api/status", "", session("oscar"))
	var st struct {
		User, Role string
		SignedIn   bool
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || !st.SignedIn || st.User != "oscar" || st.Role != store.RoleOperator {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestOperatorTargetChanges(t *testing.T) {
	s, st := rolesServer(t)
	tg, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 2})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/targets/" + jsonNumber(tg.ID)
	if w := send(h, "PATCH", path, `{"enabled":false}`, session("oscar")); w.Code != http.StatusOK {
		t.Fatalf("operator pauses: %d %s", w.Code, w.Body)
	}
	for _, body := range []string{`{"maxRunners":5}`, `{"enabled":true,"strict":true}`, `{"scaleSetName":"x"}`, `{"minRunners":0}`, `{"maxJobMinutes":5}`} {
		if w := send(h, "PATCH", path, body, session("oscar")); w.Code != http.StatusForbidden {
			t.Errorf("operator %s: %d", body, w.Code)
		}
	}
	if w := send(h, "PATCH", path, `{"enabled":true}`, session("vera")); w.Code != http.StatusForbidden {
		t.Fatalf("viewer: %d", w.Code)
	}
	if w := send(h, "PATCH", path, `{"maxRunners":5}`, session("alice")); w.Code != http.StatusOK {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
	got, _ := st.Target(tg.ID)
	if got.Enabled || got.MaxRunners != 5 {
		t.Fatalf("target %+v", got)
	}
	entries, _, _ := st.AuditPage(context.Background(), 10, 0)
	if len(entries) != 2 || entries[0].Actor != "alice" || entries[0].Detail != "max parallel: 2 → 5" ||
		entries[1].Actor != "oscar" || entries[1].Action != "target.update" || entries[1].Subject != "acme/api" || entries[1].Detail != "enabled: true → false" {
		t.Fatalf("audit %+v", entries)
	}
}

func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestUsersAPI(t *testing.T) {
	s, _ := rolesServer(t)
	h := s.Handler()
	admin := session("alice")
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{
		{"PUT", "/api/users/@Bob", `{"role":"viewer"}`, 200},
		{"PUT", "/api/users/bob", `{"role":"viewer"}`, 200}, // unchanged: no audit entry
		{"PUT", "/api/users/bob", `{"role":"operator"}`, 200},
		{"PUT", "/api/users/bad%20login", `{"role":"viewer"}`, 400},
		{"PUT", "/api/users/carol", `{"role":"root"}`, 400},
		// The only admin stays one.
		{"PUT", "/api/users/alice", `{"role":"viewer"}`, 409},
		{"DELETE", "/api/users/alice", ``, 409},
		{"DELETE", "/api/users/nobody", ``, 404},
		{"DELETE", "/api/users/bob", ``, 200},
		{"POST", "/api/access-rules", `{"org":"acme/ops","role":"admin"}`, 200},
		{"POST", "/api/access-rules", `{"org":"acme","team":"dev team","role":"admin"}`, 400},
		{"POST", "/api/access-rules", `{"org":"acme","role":"boss"}`, 400},
		{"DELETE", "/api/access-rules/999", ``, 404},
	} {
		if w := send(h, tc.method, tc.path, tc.body, admin); w.Code != tc.code {
			t.Errorf("%s %s %s: %d %s", tc.method, tc.path, tc.body, w.Code, w.Body)
		}
	}
	w := send(h, "GET", "/api/users", "", admin)
	var out struct {
		Users []store.User
		Rules []store.AccessRule
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Users) != 3 || len(out.Rules) != 2 {
		t.Fatalf("%s %v", w.Body, err)
	}
	if out.Rules[1].Subject() != "acme/ops" || out.Rules[1].Role != store.RoleAdmin || out.Rules[1].CreatedBy != "alice" {
		t.Fatalf("rule %+v", out.Rules[1])
	}
	if w := send(h, "DELETE", "/api/access-rules/"+jsonNumber(out.Rules[1].ID), "", admin); w.Code != 200 {
		t.Fatalf("delete rule: %d", w.Code)
	}
	// Audit log, latest first, paginated.
	w = send(h, "GET", "/api/audit?limit=3", "", admin)
	var entries []store.AuditEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || w.Header().Get("X-Total-Count") != "5" || len(entries) != 3 {
		t.Fatalf("%s %q %v", w.Body, w.Header().Get("X-Total-Count"), err)
	}
	want := []string{"rule.remove acme/ops was admin", "rule.put acme/ops role admin", "user.remove Bob was operator"}
	for i, e := range entries {
		if got := e.Action + " " + e.Subject + " " + e.Detail; got != want[i] || e.Actor != "alice" {
			t.Errorf("entry %d: %q by %s, want %q", i, got, e.Actor, want[i])
		}
	}
	w = send(h, "GET", "/api/audit?limit=3&offset=3", "", admin)
	_ = json.Unmarshal(w.Body.Bytes(), &entries)
	if len(entries) != 2 || entries[0].Action != "user.role" || entries[0].Detail != "viewer → operator" || entries[1].Action != "user.add" || entries[1].Subject != "Bob" {
		t.Fatalf("second page %+v", entries)
	}
}

// fakeMembers answers membership checks: "org" or "org/team" -> logins.
type fakeMembers struct {
	members map[string][]string
	fail    map[string]bool // checks that fail (no permission)
	calls   int
}

func (f *fakeMembers) check(_ context.Context, token, login, org, team string) (bool, error) {
	f.calls++
	if token != "user-token" {
		return false, errors.New("bad token")
	}
	subject := org
	if team != "" {
		subject += "/" + team
	}
	if f.fail[subject] {
		return false, errors.New("403 Resource not accessible by integration")
	}
	return containsFold(f.members[subject], login), nil
}

func TestSignInWithOrgAndTeamRules(t *testing.T) {
	s, st := authServer(t, "dave")
	addUser(t, st, "alice", store.RoleAdmin)
	f := &fakeMembers{
		members: map[string][]string{"acme": {"dave", "erin", "bob"}, "acme/ops": {"dave", "bob"}, "acme/sre": {"erin"}},
		fail:    map[string]bool{"acme/sre": true},
	}
	s.member = f.check
	for _, r := range []store.AccessRule{{Org: "acme", Role: store.RoleViewer}, {Org: "acme", Team: "ops", Role: store.RoleAdmin}, {Org: "acme", Team: "sre", Role: store.RoleAdmin}} {
		if _, err := st.PutAccessRule(r); err != nil {
			t.Fatal(err)
		}
	}
	h := s.Handler()
	signIn := func(who string) *http.Cookie {
		t.Helper()
		s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
			return who, "user-token", nil
		}
		return cookie(login(t, h), sessionCookie)
	}

	// dave: viewer through acme, admin through acme/ops.
	c := signIn("dave")
	if c == nil {
		t.Fatal("dave refused")
	}
	if sess, _ := st.Session(c.Value); len(sess.Memberships) != 2 {
		t.Fatalf("memberships %+v", sess)
	}
	// Memberships are only checked at sign-in: such a session is short.
	if c.MaxAge != int(ruleSessionTTL/time.Second) {
		t.Fatalf("rule session cookie max age %d", c.MaxAge)
	}
	if w := send(h, "GET", "/api/users", "", c); w.Code != http.StatusOK {
		t.Fatalf("dave admin: %d", w.Code)
	}

	// erin: the sre check fails, so only the acme rule counts.
	ce := signIn("erin")
	if w := send(h, "GET", "/api/users", "", ce); w.Code != http.StatusForbidden {
		t.Fatalf("erin through a failed check: %d", w.Code)
	}
	if w := send(h, "GET", "/api/jobs", "", ce); w.Code != http.StatusOK {
		t.Fatalf("erin viewer: %d", w.Code)
	}

	// A user entry takes precedence over the rules (no check is made).
	addUser(t, st, "bob", store.RoleViewer)
	before := f.calls
	cb := signIn("bob")
	if f.calls != before {
		t.Fatal("rules checked for a user with an entry")
	}
	if w := send(h, "GET", "/api/users", "", cb); w.Code != http.StatusForbidden {
		t.Fatalf("bob: %d", w.Code)
	}

	// No membership: refused.
	w := func() *httptest.ResponseRecorder {
		s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
			return "mallory", "user-token", nil
		}
		return login(t, h)
	}()
	if !strings.HasPrefix(w.Header().Get("Location"), "/login?error=forbidden") || cookie(w, sessionCookie) != nil {
		t.Fatalf("mallory: %q", w.Header().Get("Location"))
	}

	// Demoting the team rule demotes its members at once.
	if _, err := st.PutAccessRule(store.AccessRule{Org: "acme", Team: "ops", Role: store.RoleOperator}); err != nil {
		t.Fatal(err)
	}
	if w := send(h, "GET", "/api/users", "", c); w.Code != http.StatusForbidden {
		t.Fatalf("dave after demotion: %d", w.Code)
	}
	if w := send(h, "POST", "/api/updates/check", "", c); w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("dave operator: %d", w.Code)
	}
}

func TestChanges(t *testing.T) {
	before := settingValues(Settings{Slots: 2, CacheScope: "owner", HistoryDays: 30, BaseURL: "http://a"})
	after := settingValues(Settings{Slots: 4, CacheScope: "bogus", HistoryDays: 30, RunnerImage: "img:2"})
	if got := changes(settingKeys, before, after); got != "slots: 2 → 4; runner_image: (none) → img:2" {
		t.Fatalf("got %q", got)
	}
	if got := changes(targetKeys, nil, targetValues(store.Target{Enabled: true, MaxRunners: 1})); !strings.HasPrefix(got, "enabled: (none) → true; label: (none) → pitlane") {
		t.Fatalf("got %q", got)
	}
}
