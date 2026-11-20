package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// doFrom is do for a client at remote.
func doFrom(h http.Handler, remote, method, target string, header http.Header, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
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

// loginFrom signs in from remote and returns the responses of the login
// redirect and of the callback.
func loginFrom(t *testing.T, h http.Handler, remote string, header http.Header, extra ...*http.Cookie) (loginResp, callback *httptest.ResponseRecorder) {
	t.Helper()
	loginResp = doFrom(h, remote, "GET", "/auth/login", header)
	st := cookie(loginResp, stateCookie)
	if st == nil {
		t.Fatalf("login set no state cookie: %d", loginResp.Code)
	}
	callback = doFrom(h, remote, "GET", "/auth/callback?code=c&state="+url.QueryEscape(st.Value), header, append(extra, st)...)
	return loginResp, callback
}

// Cookies are Secure whenever the browser reached Pitlane over HTTPS, as
// far as the server can know: the base URL, else a trusted proxy's word.
func TestCookieFlags(t *testing.T) {
	proxies := []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}
	xfpHTTPS := http.Header{"X-Forwarded-Proto": {"https"}}
	for _, tc := range []struct {
		name       string
		baseURL    string
		remote     string
		header     http.Header
		wantSecure bool
	}{
		{"http base URL", "http://pitlane.test", "192.0.2.7:1", nil, false},
		{"https base URL", "https://pitlane.test", "192.0.2.7:1", nil, true},
		{"https base URL, proxy header ignored", "https://pitlane.test", "192.0.2.7:1", http.Header{"X-Forwarded-Proto": {"http"}}, true},
		{"no base URL, loopback proxy says https", "", "127.0.0.1:1", xfpHTTPS, true},
		{"no base URL, configured proxy says https", "", "10.1.2.3:1", xfpHTTPS, true},
		{"no base URL, proxy says http", "", "10.1.2.3:1", nil, false},
		{"no base URL, stranger claims https", "", "192.0.2.7:1", xfpHTTPS, false},
		{"no base URL, other network claims https", "", "10.2.0.1:1", xfpHTTPS, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st := authServer(t, "alice")
			addUser(t, st, "alice", store.RoleAdmin)
			s.cfg.BaseURL, s.cfg.Listen, s.cfg.TrustedProxies = tc.baseURL, "192.0.2.50:8080", proxies
			if tc.header != nil {
				tc.header = tc.header.Clone()
				tc.header.Set("Host", "pitlane.test")
			}
			lw, cb := loginFrom(t, s.Handler(), tc.remote, tc.header)
			sess, state := cookie(cb, sessionCookie), cookie(lw, stateCookie)
			if sess == nil || state == nil {
				t.Fatalf("no cookie: callback %q", cb.Header().Get("Location"))
			}
			for _, c := range []*http.Cookie{sess, state} {
				if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure != tc.wantSecure {
					t.Errorf("%s: HttpOnly=%v SameSite=%v Path=%q Secure=%v, want HttpOnly, Lax, /, Secure=%v", c.Name, c.HttpOnly, c.SameSite, c.Path, c.Secure, tc.wantSecure)
				}
			}
			if sess.MaxAge != int(sessionTTL/time.Second) {
				t.Errorf("session cookie lasts %ds, want %d", sess.MaxAge, int(sessionTTL/time.Second))
			}
			if state.MaxAge != 900 {
				t.Errorf("state cookie lasts %ds, want 900", state.MaxAge)
			}
			// The state is single use: the callback clears its cookie.
			var cleared *http.Cookie
			for _, c := range cb.Result().Cookies() {
				if c.Name == stateCookie {
					cleared = c
				}
			}
			if cleared == nil || cleared.MaxAge >= 0 || cleared.Value != "" || cleared.Secure != tc.wantSecure {
				t.Errorf("state cookie not cleared by the callback: %+v", cleared)
			}
		})
	}
}

func TestSetupUnlockCookieFlags(t *testing.T) {
	for _, secure := range []bool{false, true} {
		s, _ := newSetupServer(t)
		s.cfg.BaseURL = map[bool]string{false: "http://p.test", true: "https://p.test"}[secure]
		r := httptest.NewRequest("POST", "/api/setup/unlock", strings.NewReader(`{"token":"tok"}`))
		r.Header.Set("X-Pitlane", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		c := cookie(w, setupCookie)
		if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure != secure || c.MaxAge != 3600 || c.Path != "/" {
			t.Errorf("secure=%v: %+v", secure, c)
		}
	}
}

func TestSessionLifetimes(t *testing.T) {
	s, st := authServer(t, "dave")
	addUser(t, st, "alice", store.RoleAdmin)
	if _, err := st.PutAccessRule(store.AccessRule{Org: "acme", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	s.member = (&fakeMembers{members: map[string][]string{"acme": {"dave"}}}).check
	h := s.Handler()

	// A user entry: a long session, no memberships to recheck.
	s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
		return "alice", "user-token", nil
	}
	c := cookie(login(t, h), sessionCookie)
	if c == nil || c.MaxAge != int(sessionTTL/time.Second) {
		t.Fatalf("user session cookie %+v", c)
	}
	if sess, _ := st.Session(c.Value); len(sess.Memberships) != 0 {
		t.Fatalf("memberships kept for a user entry: %+v", sess)
	}
	if ruleSessionTTL >= sessionTTL {
		t.Fatal("a session granted by a rule must be shorter than a user's")
	}
	// A rule: a short one, with the rules matched.
	s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
		return "dave", "user-token", nil
	}
	c = cookie(login(t, h), sessionCookie)
	if c == nil || c.MaxAge != int(ruleSessionTTL/time.Second) {
		t.Fatalf("rule session cookie %+v", c)
	}
	if sess, _ := st.Session(c.Value); len(sess.Memberships) != 1 || sess.Memberships[0] != "acme" {
		t.Fatalf("memberships %+v", sess)
	}
}

func TestLogoutClearsTheSession(t *testing.T) {
	s, st := authServer(t, "alice")
	addUser(t, st, "alice", store.RoleAdmin)
	h := s.Handler()
	c := cookie(login(t, h), sessionCookie)
	if c == nil {
		t.Fatal("no session")
	}
	w := do(h, "POST", "/api/logout", uiHeader, c)
	out := cookie(w, sessionCookie)
	if w.Code != http.StatusOK || out == nil || out.Value != "" || out.MaxAge >= 0 || !out.HttpOnly {
		t.Fatalf("%d %+v", w.Code, out)
	}
	if w := do(h, "GET", "/api/jobs", nil, c); w.Code != http.StatusUnauthorized {
		t.Fatalf("session still works after sign-out: %d", w.Code)
	}
	// No session at all: still fine, and the cookie is cleared anyway.
	if w := do(h, "POST", "/api/logout", uiHeader); w.Code != http.StatusOK || cookie(w, sessionCookie) == nil {
		t.Fatalf("anonymous sign-out: %d", w.Code)
	}
	// A sign-out of another browser's session is not possible without its token.
	other := cookie(login(t, h), sessionCookie)
	do(h, "POST", "/api/logout", uiHeader, &http.Cookie{Name: sessionCookie, Value: "guess"})
	if w := do(h, "GET", "/api/jobs", nil, other); w.Code != http.StatusOK {
		t.Fatalf("someone else's session ended: %d", w.Code)
	}
}

func TestLoginRedirect(t *testing.T) {
	s, _ := authServer(t, "alice")
	h := s.Handler()
	w := do(h, "GET", "/auth/login", nil)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || w.Code != http.StatusFound || loc.Host != "github.com" || loc.Path != "/login/oauth/authorize" {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("client_id") != "Iv1.test" || q.Get("redirect_uri") != "http://pitlane.test/auth/callback" || q.Get("state") != cookie(w, stateCookie).Value {
		t.Fatalf("query %v", q)
	}
	// Two sign-ins never share a state.
	if other := do(h, "GET", "/auth/login", nil).Header().Get("Location"); other == w.Header().Get("Location") {
		t.Fatal("same state twice")
	}
	// Before the App exists, there is nothing to sign in with.
	_ = s.store.Set("app_client_id", "")
	if w := do(h, "GET", "/auth/login", nil); w.Header().Get("Location") != "/setup" || cookie(w, stateCookie) != nil {
		t.Fatalf("no App: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestOAuthCallbackFailures(t *testing.T) {
	t.Run("the exchange fails", func(t *testing.T) {
		s, st := authServer(t, "alice")
		addUser(t, st, "alice", store.RoleAdmin)
		s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
			return "", "", errors.New("bad_verification_code")
		}
		_, cb := loginFrom(t, s.Handler(), "192.0.2.7:1", nil)
		if cb.Header().Get("Location") != "/login?error=oauth" || cookie(cb, sessionCookie) != nil {
			t.Fatalf("%d %q", cb.Code, cb.Header().Get("Location"))
		}
	})
	t.Run("the exchange gets the App's credentials and the code", func(t *testing.T) {
		s, st := authServer(t, "alice")
		addUser(t, st, "alice", store.RoleAdmin)
		if err := st.SetSecret("app_client_secret", "shh"); err != nil {
			t.Fatal(err)
		}
		var got [4]string
		s.exchangeCode = func(_ context.Context, id, secret, code, redirect string) (string, string, error) {
			got = [4]string{id, secret, code, redirect}
			return "alice", "tok", nil
		}
		loginFrom(t, s.Handler(), "192.0.2.7:1", nil)
		if want := [4]string{"Iv1.test", "shh", "c", "http://pitlane.test/auth/callback"}; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("an old state", func(t *testing.T) {
		s, st := authServer(t, "alice")
		addUser(t, st, "alice", store.RoleAdmin)
		state := s.newState("oauth")
		s.mu.Lock()
		s.states[state] = time.Now().Add(-16 * time.Minute)
		s.mu.Unlock()
		w := do(s.Handler(), "GET", "/auth/callback?code=c&state="+state, nil, &http.Cookie{Name: stateCookie, Value: state})
		if w.Header().Get("Location") != "/login?error=state" || cookie(w, sessionCookie) != nil {
			t.Fatalf("expired state accepted: %q", w.Header().Get("Location"))
		}
	})
	t.Run("expired states are forgotten", func(t *testing.T) {
		s, _ := authServer(t, "alice")
		old := s.newState("oauth")
		s.mu.Lock()
		s.states[old] = time.Now().Add(-time.Hour)
		s.mu.Unlock()
		s.newState("oauth")
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, kept := s.states[old]; kept || len(s.states) != 1 {
			t.Fatalf("states %v", s.states)
		}
	})
	t.Run("the first admin cannot be saved", func(t *testing.T) {
		s, _, dir, logs := serverAt(t)
		s.cfg.InsecureNoAuth = false
		_ = s.store.Set("app_client_id", "Iv1.test")
		s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
			return "alice", "t", nil
		}
		tok := s.EnsureSetupToken().Value
		execSQL(t, dir, `CREATE TRIGGER no_users BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		_, cb := loginFrom(t, s.Handler(), "192.0.2.7:1", nil, &http.Cookie{Name: setupCookie, Value: tok})
		checkInternalErr(t, cb, logs, http.StatusInternalServerError, "first_admin", "disk full")
		if cookie(cb, sessionCookie) != nil || s.getSetupToken() != tok {
			t.Fatal("a failed registration opened a session or burnt the setup token")
		}
	})
	t.Run("the session cannot be created", func(t *testing.T) {
		s, st, dir, logs := serverAt(t)
		s.cfg.InsecureNoAuth = false
		_ = st.Set("app_client_id", "Iv1.test")
		addUser(t, st, "alice", store.RoleAdmin)
		s.exchangeCode = func(context.Context, string, string, string, string) (string, string, error) {
			return "alice", "t", nil
		}
		dropTable(t, dir, "sessions")
		_, cb := loginFrom(t, s.Handler(), "192.0.2.7:1", nil)
		checkInternalErr(t, cb, logs, http.StatusInternalServerError, "session_create", "no such table")
		if cookie(cb, sessionCookie) != nil {
			t.Fatal("session cookie set without a session")
		}
	})
}

func TestStatusEndpoint(t *testing.T) {
	type status struct {
		Version       string `json:"version"`
		Mode          string `json:"mode"`
		NeedsSetup    bool   `json:"needsSetup"`
		SetupUnlocked bool   `json:"setupUnlocked"`
		SignedIn      bool   `json:"signedIn"`
		User          string `json:"user"`
		Role          string `json:"role"`
		BaseURL       string `json:"baseUrl"`
		App           map[string]string
	}
	read := func(t *testing.T, s *Server, cs ...*http.Cookie) status {
		t.Helper()
		w := do(s.Handler(), "GET", "/api/status", nil, cs...)
		var out status
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%d %s: %v", w.Code, w.Body, err)
		}
		return out
	}
	t.Run("fresh install", func(t *testing.T) {
		s, st := authServer(t, "alice")
		s.cfg.Version = "v1.2.3"
		_ = st.Set("app_client_id", "")
		s.setSetupToken("tok")
		got := read(t, s)
		if got.Version != "v1.2.3" || got.Mode != "none" || !got.NeedsSetup || got.SetupUnlocked || got.SignedIn || got.User != "" {
			t.Fatalf("%+v", got)
		}
		if got := read(t, s, &http.Cookie{Name: setupCookie, Value: "tok"}); !got.SetupUnlocked {
			t.Fatal("setup not reported unlocked")
		}
		if got := read(t, s, &http.Cookie{Name: setupCookie, Value: "guess"}); got.SetupUnlocked {
			t.Fatal("a wrong setup cookie unlocks the setup")
		}
	})
	t.Run("GitHub App", func(t *testing.T) {
		s, st := authServer(t, "alice")
		addUser(t, st, "alice", store.RoleAdmin)
		for k, v := range map[string]string{"app_slug": "pitlane-x", "app_name": "Pitlane X", "app_html_url": "https://github.com/apps/pitlane-x", "app_owner": "acme"} {
			_ = st.Set(k, v)
		}
		_ = st.CreateSession("tok", "alice", time.Hour)
		got := read(t, s, session("tok"))
		if got.Mode != "app" || got.NeedsSetup || !got.SignedIn || got.User != "alice" || got.Role != store.RoleAdmin || got.BaseURL != "http://pitlane.test" ||
			got.App["slug"] != "pitlane-x" || got.App["owner"] != "acme" || got.App["htmlUrl"] != "https://github.com/apps/pitlane-x" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("personal access token", func(t *testing.T) {
		s, st := authServer(t, "alice")
		addUser(t, st, "alice", store.RoleAdmin)
		s.cfg.PAT = "ghp_x"
		if got := read(t, s); got.Mode != "pat" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("no authentication", func(t *testing.T) {
		s, _ := authServer(t, "alice")
		s.cfg.InsecureNoAuth = true
		if got := read(t, s); got.NeedsSetup || !got.SignedIn || got.Role != store.RoleAdmin {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a removed user is not signed in", func(t *testing.T) {
		s, st := authServer(t, "alice")
		addUser(t, st, "alice", store.RoleAdmin)
		addUser(t, st, "bob", store.RoleAdmin)
		_ = st.CreateSession("tok", "alice", time.Hour)
		_, _ = st.DeleteUser("alice")
		if got := read(t, s, session("tok")); got.SignedIn || got.User != "" || got.Role != "" {
			t.Fatalf("%+v", got)
		}
	})
}

type manifestReply struct {
	Action   string `json:"action"`
	Manifest string `json:"manifest"`
}

func TestSetupManifestFlow(t *testing.T) {
	post := func(s *Server, body string, cs ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/setup/manifest", strings.NewReader(body))
		r.Header.Set("X-Pitlane", "1")
		for _, c := range cs {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	t.Run("locked", func(t *testing.T) {
		s, _ := newSetupServer(t)
		for name, cs := range map[string][]*http.Cookie{"no cookie": nil, "wrong cookie": {{Name: setupCookie, Value: "guess"}}} {
			if w := post(s, `{"baseUrl":"https://p.test"}`, cs...); w.Code != http.StatusForbidden || cookie(w, stateCookie) != nil || s.store.Get("base_url", "") != "" {
				t.Errorf("%s: %d", name, w.Code)
			}
		}
	})
	t.Run("creates the manifest and binds a state to the browser", func(t *testing.T) {
		s, setup := newSetupServer(t)
		w := post(s, `{"baseUrl":" https://p.test/ ","name":"My Pitlane"}`, setup)
		var out manifestReply
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		state := cookie(w, stateCookie)
		if state == nil || !strings.HasPrefix(state.Value, "manifest.") || !state.HttpOnly || state.MaxAge != 900 {
			t.Fatalf("state cookie %+v", state)
		}
		if out.Action != "https://github.com/settings/apps/new?state="+state.Value {
			t.Fatalf("action %q", out.Action)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(out.Manifest), &m); err != nil || m["name"] != "My Pitlane" || !strings.Contains(out.Manifest, "https://p.test/") || strings.Contains(out.Manifest, "p.test//") {
			t.Fatalf("manifest %s: %v", out.Manifest, err)
		}
		if got := s.store.Get("base_url", ""); got != "https://p.test" {
			t.Fatalf("base URL saved as %q", got)
		}
		// The state is accepted once, by the manifest flow only.
		if s.consumeState("oauth", state.Value) || !s.consumeState("manifest", state.Value) || s.consumeState("manifest", state.Value) {
			t.Fatal("state not single use or not bound to its purpose")
		}
	})
	t.Run("defaults", func(t *testing.T) {
		s, setup := newSetupServer(t)
		s.cfg.BaseURL = "http://configured.test"
		for _, body := range []string{``, `{}`} { // no body: the base URL is the server's own, the name random
			w := post(s, body, setup)
			var out manifestReply
			var m struct{ Name string }
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || json.Unmarshal([]byte(out.Manifest), &m) != nil || !strings.HasPrefix(m.Name, "Pitlane ") {
				t.Fatalf("%q: %d %s", body, w.Code, w.Body)
			}
			if !strings.Contains(out.Manifest, "http://configured.test/") || !strings.HasPrefix(out.Action, "https://github.com/settings/apps/new?state=") {
				t.Fatalf("%q: %s", body, w.Body)
			}
		}
	})
	t.Run("refused requests", func(t *testing.T) {
		s, setup := newSetupServer(t)
		for name, body := range map[string]string{"not JSON": `{`, "ftp base URL": `{"baseUrl":"ftp://p.test"}`, "no scheme": `{"baseUrl":"p.test"}`} {
			if w := post(s, body, setup); w.Code != http.StatusBadRequest || cookie(w, stateCookie) != nil {
				t.Errorf("%s: %d %s", name, w.Code, w.Body)
			}
		}
	})
	t.Run("the base URL cannot be saved", func(t *testing.T) {
		s, _, dir, logs := serverAt(t)
		s.cfg.InsecureNoAuth = false
		s.setSetupToken("tok")
		dropTable(t, dir, "settings")
		w := post(s, `{"baseUrl":"https://p.test"}`, &http.Cookie{Name: setupCookie, Value: "tok"})
		checkInternalErr(t, w, logs, http.StatusInternalServerError, "base_url_save", "no such table")
	})
	t.Run("an admin may create the App again, a viewer may not", func(t *testing.T) {
		s, _, _, h := apiServer(t)
		_ = s
		for who, want := range map[string]int{"alice": http.StatusOK, "oscar": http.StatusForbidden, "vera": http.StatusForbidden, "": http.StatusForbidden} {
			if w := send(h, "POST", "/api/setup/manifest", `{"baseUrl":"https://p.test"}`, cookiesFor(who)...); w.Code != want {
				t.Errorf("%q: %d, want %d", who, w.Code, want)
			}
		}
	})
}

func TestSetupCallbackConversion(t *testing.T) {
	s, setup := newSetupServer(t)
	h := s.Handler()
	state := s.newState("manifest")
	stateCk := &http.Cookie{Name: stateCookie, Value: state}
	// A request whose context is already cancelled: the conversion fails at
	// once, without reaching GitHub.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "GET", "/setup/callback?code=abc&state="+state, nil)
	r.AddCookie(setup)
	r.AddCookie(stateCk)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Header().Get("Location") != "/setup?error=conversion" || s.appConfigured() {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Location"))
	}
	// The state was used up by that attempt: no retry with it.
	w = do(h, "GET", "/setup/callback?code=abc&state="+state, nil, setup, stateCk)
	if w.Header().Get("Location") != "/setup?error=state" {
		t.Fatalf("replayed state: %q", w.Header().Get("Location"))
	}
}
