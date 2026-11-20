package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstream is a fake internal/update handler: it records what it receives
// and answers with the status the test sets.
type upstream struct {
	status int
	got    []string // "METHOD path body"
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.got = append(u.got, r.Method+" "+r.URL.Path+" "+string(b))
	w.WriteHeader(u.status)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func updateAudit(t *testing.T, s *Server) (actions []string, details []string) {
	t.Helper()
	entries, _, err := s.store.AuditPage(t.Context(), 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		actions = append(actions, entries[i].Action+" by "+entries[i].Actor)
		details = append(details, entries[i].Detail)
	}
	return actions, details
}

func TestUpdatesNotAvailable(t *testing.T) {
	_, _, _, h := apiServer(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/updates"}, {"POST", "/api/updates/check"}, {"POST", "/api/updates/apply"},
	} {
		w := send(h, tc.method, tc.path, "{}", session("alice"))
		if w.Code != http.StatusNotFound || !bodyHas(w, "updates are not available") {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
}

func TestUpdatesAreProxiedAndAudited(t *testing.T) {
	s, _, _, h := apiServer(t)
	up := &upstream{status: http.StatusOK}
	s.SetUpdates(up)

	// Reading is not an event.
	if w := get(h, "/api/updates", "oscar"); w.Code != http.StatusOK || !bodyHas(w, `"ok":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if actions, _ := updateAudit(t, s); len(actions) != 0 {
		t.Fatalf("a read was audited: %v", actions)
	}
	// A mutation reaches the updater with its body, then is audited with it.
	if w := send(h, "POST", "/api/updates/check", `{"channel":"stable"}`, session("oscar")); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	if w := send(h, "PUT", "/api/updates/settings", `{"auto":true}`, session("alice")); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	// No body, a trivial body or text: audited without detail.
	send(h, "POST", "/api/updates/pull-runner", "", session("oscar"))
	send(h, "POST", "/api/updates/apply", "{}", session("alice"))
	send(h, "POST", "/api/updates/apply", "not json", session("alice"))
	wantGot := []string{
		"GET /api/updates ",
		`POST /api/updates/check {"channel":"stable"}`,
		`PUT /api/updates/settings {"auto":true}`,
		"POST /api/updates/pull-runner ",
		"POST /api/updates/apply {}",
		"POST /api/updates/apply not json",
	}
	if strings.Join(up.got, "\n") != strings.Join(wantGot, "\n") {
		t.Fatalf("the updater received %q, want %q", up.got, wantGot)
	}
	actions, details := updateAudit(t, s)
	wantActions := []string{"updates.check by oscar", "updates.settings by alice", "updates.pull-runner by oscar", "updates.apply by alice", "updates.apply by alice"}
	wantDetails := []string{`{"channel":"stable"}`, `{"auto":true}`, "", "", ""}
	if strings.Join(actions, "|") != strings.Join(wantActions, "|") || strings.Join(details, "|") != strings.Join(wantDetails, "|") {
		t.Fatalf("audit %q %q", actions, details)
	}
}

func TestUpdatesFailuresAreNotAudited(t *testing.T) {
	s, _, _, h := apiServer(t)
	up := &upstream{status: http.StatusConflict}
	s.SetUpdates(up)
	if w := send(h, "POST", "/api/updates/apply", `{"x":1}`, session("alice")); w.Code != http.StatusConflict {
		t.Fatalf("the updater's status is not passed on: %d", w.Code)
	}
	if actions, _ := updateAudit(t, s); len(actions) != 0 {
		t.Fatalf("a refused update was audited: %v", actions)
	}
}

func TestUpdatesRoles(t *testing.T) {
	s, _, _, h := apiServer(t)
	s.SetUpdates(&upstream{status: http.StatusOK})
	for _, tc := range []struct {
		method, path string
		who          map[string]int
	}{
		{"GET", "/api/updates", map[string]int{"": 401, "vera": 403, "oscar": 200, "alice": 200}},
		{"POST", "/api/updates/check", map[string]int{"": 401, "vera": 403, "oscar": 200, "alice": 200}},
		{"POST", "/api/updates/pull-runner", map[string]int{"": 401, "vera": 403, "oscar": 200, "alice": 200}},
		{"POST", "/api/updates/apply", map[string]int{"": 401, "vera": 403, "oscar": 403, "alice": 200}},
		{"PUT", "/api/updates/settings", map[string]int{"": 401, "vera": 403, "oscar": 403, "alice": 200}},
	} {
		for who, want := range tc.who {
			if w := send(h, tc.method, tc.path, "{}", cookiesFor(who)...); w.Code != want {
				t.Errorf("%s %s as %q: %d, want %d", tc.method, tc.path, who, w.Code, want)
			}
		}
	}
}

// A body too big for the audit reaches the updater whole, and the audit does
// not record the part of it that was read as if it were the request.
func TestUpdatesLargeBody(t *testing.T) {
	t.Parallel()
	s, _, _, _ := apiServer(t)
	up := &upstream{status: http.StatusOK}
	s.SetUpdates(up)
	big := `{"note":"` + strings.Repeat("a", 3*maxUpdatesAudit) + `"}`
	r := httptest.NewRequest("POST", "/api/updates/check", strings.NewReader(big))
	w := httptest.NewRecorder()
	s.handleUpdates(w, r)
	if w.Code != http.StatusOK || len(up.got) != 1 {
		t.Fatalf("%d, %d calls", w.Code, len(up.got))
	}
	if want := "POST /api/updates/check " + big; up.got[0] != want {
		t.Fatalf("the updater received %d bytes, want %d", len(up.got[0]), len(want))
	}
	if _, details := updateAudit(t, s); len(details) != 1 || details[0] != "" {
		t.Fatalf("a truncated body was audited as if it were the request: %q", details)
	}
}

func TestStatusWriterKeepsTheCode(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, code: http.StatusOK}
	sw.WriteHeader(http.StatusTeapot)
	if sw.code != http.StatusTeapot || rec.Code != http.StatusTeapot {
		t.Fatalf("%d %d", sw.code, rec.Code)
	}
}

// A body longer than the audit cap reaches the update API whole.
func TestUpdatesForwardsWholeBody(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{InsecureNoAuth: true}}
	var got string
	s.upd = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.WriteHeader(http.StatusBadRequest) // no audit entry: no store needed
	})
	body := `{"x":"` + strings.Repeat("a", 2*maxUpdatesAudit) + `"}`
	s.handleUpdates(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/updates/preferences", strings.NewReader(body)))
	if got != body {
		t.Fatalf("forwarded %d bytes, want %d", len(got), len(body))
	}
}
