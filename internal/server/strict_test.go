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

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// fakeGitHub answers the strict check per target full name; a missing name
// fails like an unreachable GitHub.
type fakeGitHub map[string]string

func (f fakeGitHub) check(_ context.Context, t store.Target) (string, error) {
	r, ok := f[t.FullName()]
	if !ok {
		return "", errors.New("github unreachable")
	}
	return r, nil
}

func newStrictServer(t *testing.T, gh fakeGitHub) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{}, st, &pool.Pool{}, log, nil)
	s.strictCheck = gh.check
	// Not started: Sync only notifies.
	s.SetManager(scaler.New(st, nil, t.TempDir(), func() scaler.Credentials { return scaler.Credentials{} }, func() {}, log))
	return s
}

func call(t *testing.T, h http.HandlerFunc, method, path, id, body string) (int, store.Target, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != "" {
		r.SetPathValue("id", id)
	}
	w := httptest.NewRecorder()
	h(w, r)
	var tg store.Target
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &tg); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code, tg, w.Body.String()
}

func TestCreateTargetStrictPolicy(t *testing.T) {
	s := newStrictServer(t, fakeGitHub{"acme/public": store.StrictPublicRepo, "acme/private": ""})
	for _, tc := range []struct {
		name, body string
		code       int
		strict     bool
	}{
		{"public, nothing asked", `{"kind":"repo","owner":"acme","repo":"public"}`, 200, true},
		{"private, nothing asked", `{"kind":"repo","owner":"acme","repo":"private"}`, 200, false},
		{"github unreachable: fail closed", `{"kind":"repo","owner":"acme","repo":"unknown","strict":false}`, 200, true},
	} {
		code, tg, body := call(t, s.handleCreateTarget, "POST", "/api/targets", "", tc.body)
		if code != tc.code || (code == 200 && tg.Strict != tc.strict) {
			t.Errorf("%s: code %d strict %v (%s)", tc.name, code, tg.Strict, body)
		}
	}
	s2 := newStrictServer(t, fakeGitHub{"acme/public": store.StrictPublicRepo})
	code, _, body := call(t, s2.handleCreateTarget, "POST", "/api/targets", "", `{"kind":"repo","owner":"acme","repo":"public","strict":false}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "strict mode cannot be turned off") {
		t.Fatalf("public repository created non-strict: %d %s", code, body)
	}
}

func TestUpdateTargetCannotTurnStrictOff(t *testing.T) {
	gh := fakeGitHub{"acme/api": store.StrictPublicRepo}
	s := newStrictServer(t, gh)
	_, tg, _ := call(t, s.handleCreateTarget, "POST", "/api/targets", "", `{"kind":"repo","owner":"acme","repo":"api"}`)
	id := "1"
	if code, _, body := call(t, s.handleUpdateTarget, "PATCH", "/api/targets/1", id, `{"strict":false}`); code != http.StatusBadRequest {
		t.Fatalf("public repository turned non-strict: %d %s", code, body)
	}
	delete(gh, "acme/api") // GitHub unreachable: strict stays on
	if code, _, _ := call(t, s.handleUpdateTarget, "PATCH", "/api/targets/1", id, `{"strict":false}`); code != http.StatusBadGateway {
		t.Fatalf("visibility unknown must keep strict on, got %d", code)
	}
	gh["acme/api"] = "" // made private
	code, got, body := call(t, s.handleUpdateTarget, "PATCH", "/api/targets/1", id, `{"strict":false}`)
	if code != 200 || got.Strict {
		t.Fatalf("private repository must be allowed non-strict: %d %s", code, body)
	}
	if stored, _ := s.store.Target(tg.ID); stored.Strict || stored.StrictRequired != "" {
		t.Fatalf("stored %+v", stored)
	}
}

func TestInstallationListingSwitchesPublicReposToStrict(t *testing.T) {
	s := newStrictServer(t, fakeGitHub{"acme/api": ""})
	_, tg, _ := call(t, s.handleCreateTarget, "POST", "/api/targets", "", `{"kind":"repo","owner":"acme","repo":"api"}`)
	if tg.Strict {
		t.Fatal("private repository created strict")
	}
	s.enforcePublicRepos(map[string]bool{"acme/api": false, "acme/other": true})
	got, _ := s.store.Target(tg.ID)
	if !got.Strict || got.StrictRequired != store.StrictPublicRepo || got.StrictForcedAt == 0 {
		t.Fatalf("repository made public not switched to strict: %+v", got)
	}
}

func TestOrgTargetStrictWhenRunnerGroupAllowsPublic(t *testing.T) {
	s := newStrictServer(t, fakeGitHub{"open": store.StrictPublicRunnerGroup, "closed": ""})
	if code, tg, body := call(t, s.handleCreateTarget, "POST", "/api/targets", "", `{"kind":"org","owner":"open"}`); code != 200 || !tg.Strict || tg.StrictRequired == "" {
		t.Fatalf("org open to public repositories: %d %+v %s", code, tg, body)
	}
	if code, tg, _ := call(t, s.handleCreateTarget, "POST", "/api/targets", "", `{"kind":"org","owner":"open2","strict":false}`); code != 200 || !tg.Strict {
		t.Fatal("unknown runner group: created strict (fail closed)")
	}
	if code, tg, _ := call(t, s.handleCreateTarget, "POST", "/api/targets", "", `{"kind":"org","owner":"closed"}`); code != 200 || tg.Strict {
		t.Fatalf("org closed to public repositories must not be forced strict: %+v", tg)
	}
	if code, _, body := call(t, s.handleUpdateTarget, "PATCH", "/api/targets/1", "1", `{"strict":false}`); code != http.StatusBadRequest {
		t.Fatalf("strict turned off on an org open to public repositories: %d %s", code, body)
	}
}

func TestWarmRunnerDefaults(t *testing.T) {
	s := newStrictServer(t, fakeGitHub{"acme/pub": store.StrictPublicRepo, "acme/priv": "", "acme/priv2": "", "acme/pub2": store.StrictPublicRepo})
	for _, tc := range []struct {
		body string
		warm int
	}{
		{`{"kind":"repo","owner":"acme","repo":"pub"}`, 0},
		{`{"kind":"repo","owner":"acme","repo":"priv"}`, store.DefaultMinRunners},
		{`{"kind":"repo","owner":"acme","repo":"priv2","strict":true}`, 0},
		{`{"kind":"repo","owner":"acme","repo":"pub2","minRunners":1}`, 1}, // explicit: kept
		{`{"kind":"repo","owner":"acme","repo":"unknown"}`, 0},             // fail closed: strict
	} {
		code, tg, body := call(t, s.handleCreateTarget, "POST", "/api/targets", "", tc.body)
		if code != 200 || tg.MinRunners != tc.warm {
			t.Errorf("%s: code %d warm %d, want %d (%s)", tc.body, code, tg.MinRunners, tc.warm, body)
		}
	}
}
