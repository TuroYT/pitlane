package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

func addUser(t *testing.T, st *store.Store, login, role string) {
	t.Helper()
	if _, err := st.PutUser(store.User{Login: login, Role: role}); err != nil {
		t.Fatal(err)
	}
}

// onlyAdmin reports whether login is the one and only user, an admin.
func onlyAdmin(st *store.Store, login string) bool {
	users, err := st.Users()
	return err == nil && len(users) == 1 && users[0].Login == login && users[0].Role == store.RoleAdmin
}

// authServer is a server with a configured GitHub App whose OAuth exchange
// returns login (no network).
func authServer(t *testing.T, login string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{DataDir: t.TempDir(), BaseURL: "http://pitlane.test"}, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), fstest.MapFS{})
	s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
		return login, "user-token", nil
	}
	if err := st.Set("app_client_id", "Iv1.test"); err != nil {
		t.Fatal(err)
	}
	return s, st
}

func do(h http.Handler, method, target string, header http.Header, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		r.Header[k] = v
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var uiHeader = http.Header{"X-Pitlane": {"1"}}

func TestAPIRequiresSession(t *testing.T) {
	s, st := authServer(t, "alice")
	addUser(t, st, "alice", store.RoleAdmin)
	h := s.Handler()
	if w := do(h, "GET", "/api/jobs", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no session: %d", w.Code)
	}
	if w := do(h, "GET", "/api/jobs", nil, &http.Cookie{Name: sessionCookie, Value: "forged"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown session: %d", w.Code)
	}
	_ = st.CreateSession("tok", "alice", time.Hour)
	sess := &http.Cookie{Name: sessionCookie, Value: "tok"}
	if w := do(h, "GET", "/api/jobs", nil, sess); w.Code != http.StatusOK {
		t.Fatalf("valid session: %d %s", w.Code, w.Body)
	}
	// The database keeps the token's hash: the hash itself is no session.
	sum := sha256.Sum256([]byte("tok"))
	if w := do(h, "GET", "/api/jobs", nil, &http.Cookie{Name: sessionCookie, Value: hex.EncodeToString(sum[:])}); w.Code != http.StatusUnauthorized {
		t.Fatalf("stored hash accepted as a session: %d", w.Code)
	}
	// A session of a user removed no longer works.
	addUser(t, st, "bob", store.RoleAdmin)
	if _, err := st.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if w := do(h, "GET", "/api/jobs", nil, sess); w.Code != http.StatusUnauthorized {
		t.Fatalf("removed user: %d", w.Code)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, st := authServer(t, "alice")
	addUser(t, st, "alice", store.RoleAdmin)
	_ = st.CreateSession("old", "alice", -time.Second)
	if w := do(s.Handler(), "GET", "/api/jobs", nil, &http.Cookie{Name: sessionCookie, Value: "old"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: %d", w.Code)
	}
}

func TestLogoutDeletesSession(t *testing.T) {
	s, st := authServer(t, "alice")
	addUser(t, st, "alice", store.RoleAdmin)
	_ = st.CreateSession("tok", "alice", time.Hour)
	sess := &http.Cookie{Name: sessionCookie, Value: "tok"}
	h := s.Handler()
	if w := do(h, "POST", "/api/logout", nil, sess); w.Code != http.StatusForbidden {
		t.Fatalf("logout without X-Pitlane: %d", w.Code)
	}
	if w := do(h, "POST", "/api/logout", uiHeader, sess); w.Code != http.StatusOK {
		t.Fatalf("logout: %d", w.Code)
	}
	if _, ok := st.SessionLogin("tok"); ok {
		t.Fatal("session still valid after logout")
	}
}

func TestCSRFHeaderOnAuthenticatedRoutes(t *testing.T) {
	s, st := authServer(t, "alice")
	addUser(t, st, "alice", store.RoleAdmin)
	_ = st.CreateSession("tok", "alice", time.Hour)
	sess := &http.Cookie{Name: sessionCookie, Value: "tok"}
	h := s.Handler()
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/targets"}, {"PATCH", "/api/targets/1"}, {"DELETE", "/api/targets/1"},
		{"PUT", "/api/settings"}, {"POST", "/api/setup/unlock"}, {"POST", "/api/setup/manifest"},
	} {
		if w := do(h, tc.method, tc.path, nil, sess); w.Code != http.StatusForbidden {
			t.Errorf("%s %s without X-Pitlane: %d", tc.method, tc.path, w.Code)
		}
	}
}

// login runs /auth/login then /auth/callback with the state it set.
func login(t *testing.T, h http.Handler, extra ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	w := do(h, "GET", "/auth/login", nil)
	st := cookie(w, stateCookie)
	if w.Code != http.StatusFound || st == nil {
		t.Fatalf("login: %d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Query().Get("state") != st.Value || !strings.HasPrefix(st.Value, "oauth.") {
		t.Fatalf("state %q not bound to the redirect %q", st.Value, loc)
	}
	return do(h, "GET", "/auth/callback?code=c&state="+url.QueryEscape(st.Value), nil, append(extra, st)...)
}

func TestOAuthCallbackState(t *testing.T) {
	s, st := authServer(t, "alice")
	addUser(t, st, "alice", store.RoleAdmin)
	h := s.Handler()

	w := login(t, h)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" || cookie(w, sessionCookie) == nil {
		t.Fatalf("sign-in: %d %q", w.Code, w.Header().Get("Location"))
	}
	if login, ok := st.SessionLogin(cookie(w, sessionCookie).Value); !ok || login != "alice" {
		t.Fatal("session not created")
	}

	refused := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Header().Get("Location") != "/login?error=state" || cookie(w, sessionCookie) != nil {
			t.Errorf("%s: %d %q", name, w.Code, w.Header().Get("Location"))
		}
	}
	// Cookie and query state differ.
	lw := do(h, "GET", "/auth/login", nil)
	good := cookie(lw, stateCookie)
	other := s.newState("oauth")
	refused("mismatch", do(h, "GET", "/auth/callback?code=c&state="+other, nil, good))
	// No state cookie at all.
	refused("no cookie", do(h, "GET", "/auth/callback?code=c&state="+good.Value, nil))
	// A state is single use.
	if w := do(h, "GET", "/auth/callback?code=c&state="+good.Value, nil, good); cookie(w, sessionCookie) == nil {
		t.Fatalf("first use refused: %q", w.Header().Get("Location"))
	}
	refused("replay", do(h, "GET", "/auth/callback?code=c&state="+good.Value, nil, good))
	// A manifest state cannot be replayed in the OAuth flow (and back).
	m := s.newState("manifest")
	refused("manifest state in OAuth", do(h, "GET", "/auth/callback?code=c&state="+m, nil, &http.Cookie{Name: stateCookie, Value: m}))
	o := s.newState("oauth")
	if s.consumeState("manifest", o) {
		t.Error("OAuth state accepted by the manifest flow")
	}
	// Forged state never issued by the server.
	f := "oauth.0123456789abcdef"
	refused("forged", do(h, "GET", "/auth/callback?code=c&state="+f, nil, &http.Cookie{Name: stateCookie, Value: f}))
}

func TestSetupCallbackRejectsBadState(t *testing.T) {
	s, cookieSetup := newSetupServer(t)
	h := s.Handler()
	o := s.newState("oauth")
	for name, cs := range map[string][]*http.Cookie{
		"OAuth state in the manifest flow": {cookieSetup, {Name: stateCookie, Value: o}},
		"no state cookie":                  {cookieSetup},
	} {
		w := do(h, "GET", "/setup/callback?code=c&state="+o, nil, cs...)
		if w.Header().Get("Location") != "/setup?error=state" {
			t.Errorf("%s: %q", name, w.Header().Get("Location"))
		}
	}
	m := s.newState("manifest")
	if w := do(h, "GET", "/setup/callback?code=c&state="+m, nil, &http.Cookie{Name: stateCookie, Value: m}); w.Header().Get("Location") != "/setup?error=state" {
		t.Errorf("manifest callback without the setup cookie: %q", w.Header().Get("Location"))
	}
}

func TestFirstAdminNeedsSetupCookie(t *testing.T) {
	t.Run("without the setup cookie", func(t *testing.T) {
		s, st := authServer(t, "mallory")
		s.EnsureSetupToken()
		w := login(t, s.Handler())
		if !strings.HasPrefix(w.Header().Get("Location"), "/login?error=forbidden") || cookie(w, sessionCookie) != nil {
			t.Fatalf("got %q", w.Header().Get("Location"))
		}
		if st.HasUsers() {
			t.Fatal("first admin registered without the setup token")
		}
	})
	t.Run("with a wrong setup cookie", func(t *testing.T) {
		s, st := authServer(t, "mallory")
		s.EnsureSetupToken()
		login(t, s.Handler(), &http.Cookie{Name: setupCookie, Value: "guess"})
		if st.HasUsers() {
			t.Fatal("first admin registered with a wrong setup token")
		}
	})
	t.Run("with the setup cookie", func(t *testing.T) {
		s, st := authServer(t, "alice")
		tok := s.EnsureSetupToken()
		file := filepath.Join(s.cfg.DataDir, "setup-token")
		if _, err := os.Stat(file); err != nil {
			t.Fatal("setup token file missing")
		}
		w := login(t, s.Handler(), &http.Cookie{Name: setupCookie, Value: tok})
		if cookie(w, sessionCookie) == nil || !onlyAdmin(st, "alice") {
			t.Fatalf("first admin not registered: %q", w.Header().Get("Location"))
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatal("setup token file kept after the first sign-in")
		}
		// The token is single use: the next person is not made admin.
		s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
			return "mallory", "user-token", nil
		}
		login(t, s.Handler(), &http.Cookie{Name: setupCookie, Value: tok})
		if !onlyAdmin(st, "alice") {
			t.Fatal("setup token reused")
		}
	})
}

func TestCanManageApp(t *testing.T) {
	type env struct {
		app, admins, noAuth, setupCookie, session bool
	}
	for _, tc := range []struct {
		name string
		env  env
		want bool
	}{
		{"first setup with the setup cookie", env{setupCookie: true}, true},
		{"first setup without it", env{}, false},
		{"first setup, a session is not enough", env{session: true}, false},
		{"app configured, no admin yet: needs a session", env{app: true, setupCookie: true}, false},
		{"admin signed in", env{app: true, admins: true, session: true}, true},
		{"admin list set, App lost, signed in", env{admins: true, session: true}, true},
		{"admin list set, not signed in", env{app: true, admins: true, setupCookie: true}, false},
		{"no-auth mode before the App exists", env{admins: true, noAuth: true}, true},
		{"no-auth mode cannot replace the App", env{app: true, admins: true, noAuth: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := authServer(t, "alice")
			if !tc.env.app {
				_ = st.Set("app_client_id", "")
			}
			if tc.env.admins {
				addUser(t, st, "alice", store.RoleAdmin)
			}
			s.cfg.InsecureNoAuth = tc.env.noAuth
			s.setSetupToken("tok")
			r := httptest.NewRequest("POST", "/api/setup/manifest", nil)
			if tc.env.setupCookie {
				r.AddCookie(&http.Cookie{Name: setupCookie, Value: "tok"})
			}
			if tc.env.session {
				_ = st.CreateSession("sess", "alice", time.Hour)
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sess"})
			}
			if got := s.canManageApp(r); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestSetupUnlock(t *testing.T) {
	s, _ := newSetupServer(t)
	h := s.Handler()
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/setup/unlock", strings.NewReader(body))
		r.Header.Set("X-Pitlane", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(`{"token":"nope"}`); w.Code != http.StatusForbidden || cookie(w, setupCookie) != nil {
		t.Fatalf("wrong token: %d", w.Code)
	}
	w := post(`{"token":" tok "}`)
	if c := cookie(w, setupCookie); w.Code != http.StatusOK || c == nil || c.Value != "tok" || !c.HttpOnly {
		t.Fatalf("right token: %d %+v", w.Code, c)
	}
	s.setSetupToken("")
	if w := post(`{"token":""}`); w.Code != http.StatusForbidden {
		t.Fatalf("setup done: an empty token must not unlock: %d", w.Code)
	}
}
