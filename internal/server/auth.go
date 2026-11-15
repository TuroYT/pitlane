package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

// EnsureSetupToken creates the one-time token protecting the first setup.
func (s *Server) EnsureSetupToken() string {
	file := filepath.Join(s.cfg.DataDir, "setup-token")
	if s.store.HasUsers() {
		_ = os.Remove(file)
		return ""
	}
	if b, err := os.ReadFile(file); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		tok := strings.TrimSpace(string(b))
		s.setSetupToken(tok)
		return tok
	}
	tok := randomHex(12)
	s.setSetupToken(tok)
	if err := os.WriteFile(file, []byte(tok+"\n"), 0o600); err != nil {
		s.log.Warn("cannot write the setup token file (the token is in this log)", "file", file, "error", err)
	}
	return tok
}

// The setup token is read by request handlers and cleared by the first
// sign-in: guarded by s.mu.
func (s *Server) getSetupToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setupToken
}

func (s *Server) setSetupToken(tok string) {
	s.mu.Lock()
	s.setupToken = tok
	s.mu.Unlock()
}

// ------------------------------------------------------------------- auth

const (
	// sessionTTL is how long a sign-in lasts.
	sessionTTL = 30 * 24 * time.Hour
	// ruleSessionTTL is how long a sign-in granted by an organization or
	// team rule lasts: its membership is only checked at sign-in, so someone
	// who left the organization keeps access at most this long.
	ruleSessionTTL = 12 * time.Hour
)

const (
	sessionCookie = "pitlane_session"
	setupCookie   = "pitlane_setup"
	stateCookie   = "pitlane_state"
)

func (s *Server) hasSetupCookie(r *http.Request) bool {
	tok := s.getSetupToken()
	if tok == "" {
		return false
	}
	c, err := r.Cookie(setupCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(tok)) == 1
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, name, value string, maxAge int) {
	// Secure whenever the UI is served over HTTPS; plain HTTP stays possible
	// on a LAN.
	c := &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, //nolint:gosec // G124: Secure depends on the scheme, HttpOnly and SameSite are set
		Secure: strings.HasPrefix(s.baseURL(r), "https://"), SameSite: http.SameSiteLaxMode}
	http.SetCookie(w, c)
}

// newState returns a random state bound to a purpose ("manifest" or "oauth")
// so one flow's state can never be replayed in the other.
func (s *Server) newState(purpose string) string {
	st := purpose + "." + randomHex(16)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.states {
		if time.Since(t) > 15*time.Minute {
			delete(s.states, k)
		}
	}
	s.states[st] = time.Now()
	return st
}

func (s *Server) consumeState(purpose, st string) bool {
	if !strings.HasPrefix(st, purpose+".") {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.states[st]
	delete(s.states, st)
	return ok && time.Since(t) < 15*time.Minute
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	p, signedIn := s.currentUser(r)
	mode := "none"
	switch {
	case s.cfg.PAT != "":
		mode = "pat"
	case s.appConfigured():
		mode = "app"
	}
	writeJSON(w, map[string]any{
		"version":       s.cfg.Version,
		"mode":          mode,
		"needsSetup":    !s.store.HasUsers() && !s.cfg.InsecureNoAuth,
		"setupUnlocked": s.hasSetupCookie(r),
		"signedIn":      signedIn,
		"user":          p.Login,
		"role":          p.Role,
		"baseUrl":       s.knownBaseURL(r), // "" before setup: the UI proposes its own address
		"app": map[string]string{
			"slug":    s.store.Get("app_slug", ""),
			"name":    s.store.Get("app_name", ""),
			"htmlUrl": s.store.Get("app_html_url", ""),
			"owner":   s.store.Get("app_owner", ""),
		},
	})
}

// maxSetupBody bounds the bodies of the setup endpoints, which are reachable
// without signing in.
const maxSetupBody = 64 << 10

func (s *Server) handleSetupUnlock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	tok := s.getSetupToken()
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSetupBody)).Decode(&body); err != nil || tok == "" ||
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Token)), []byte(tok)) != 1 {
		writeErr(w, http.StatusForbidden, "invalid setup token")
		return
	}
	s.setCookie(w, r, setupCookie, tok, 3600)
	writeJSON(w, map[string]bool{"ok": true})
}

// handleSetupManifest returns what the browser must POST to github.com to
// create the GitHub App.
func (s *Server) handleSetupManifest(w http.ResponseWriter, r *http.Request) {
	if !s.canManageApp(r) {
		writeErr(w, http.StatusForbidden, "setup is locked")
		return
	}
	var body struct {
		BaseURL string `json:"baseUrl"`
		Org     string `json:"org"`
		Name    string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSetupBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	org := strings.TrimSpace(body.Org)
	if org != "" && !validName(org) {
		writeErr(w, http.StatusBadRequest, "invalid organization name")
		return
	}
	base := strings.TrimRight(strings.TrimSpace(body.BaseURL), "/")
	if base == "" {
		base = s.baseURL(r)
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		writeErr(w, http.StatusBadRequest, "base URL must start with http:// or https://")
		return
	}
	if err := s.store.Set("base_url", base); err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "base_url_save", "cannot save the base URL", err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "Pitlane " + randomHex(3)
	}
	manifest, _ := json.Marshal(gh.Manifest(name, base))
	state := s.newState("manifest")
	s.setCookie(w, r, stateCookie, state, 900)
	action := "https://github.com/settings/apps/new?state=" + state
	if org != "" {
		action = "https://github.com/organizations/" + url.PathEscape(org) + "/settings/apps/new?state=" + state
	}
	writeJSON(w, map[string]string{"action": action, "manifest": string(manifest)})
}

// canManageApp reports whether the request may create the GitHub App: during
// first setup it needs the setup token cookie, afterwards a signed-in admin.
func (s *Server) canManageApp(r *http.Request) bool {
	if !s.appConfigured() && !s.store.HasUsers() {
		return s.hasSetupCookie(r)
	}
	p, signedIn := s.currentUser(r)
	return signedIn && p.Role == store.RoleAdmin && !s.cfg.InsecureNoAuth || (s.cfg.InsecureNoAuth && !s.appConfigured())
}

func (s *Server) handleSetupCallback(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query().Get("state")
	c, err := r.Cookie(stateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(st)) != 1 || !s.canManageApp(r) || !s.consumeState("manifest", st) {
		http.Redirect(w, r, "/setup?error=state", http.StatusFound)
		return
	}
	app, err := gh.ConvertManifest(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		s.log.Error("github app conversion", "error", err)
		http.Redirect(w, r, "/setup?error=conversion", http.StatusFound)
		return
	}
	if err := saveApp(s.store, app); err != nil {
		// The manifest code is single use: the App exists on GitHub but its
		// credentials are lost. Setup must start over.
		s.log.Error("cannot save the GitHub App: delete it on GitHub and run the setup again", "app", app.Slug, "url", app.HTMLURL, "error", err)
		http.Redirect(w, r, "/setup?error=store", http.StatusFound)
		return
	}
	s.setCookie(w, r, stateCookie, "", -1)
	s.log.Info("GitHub App created", "app", app.Slug)
	s.audit(r, "github-app.create", app.Slug, app.HTMLURL)
	if s.mgr != nil {
		s.mgr.Restart()
	}
	http.Redirect(w, r, "/setup?step=install", http.StatusFound)
}

// settingsStore is the part of the store saveApp needs.
type settingsStore interface {
	Set(key, value string) error
	SetSecret(key, value string) error
}

// saveApp stores a newly created GitHub App. Secrets go first and the client
// ID, which marks the App as configured, last: a failure midway leaves the
// App unconfigured rather than half configured.
func saveApp(st settingsStore, app *gh.App) error {
	for _, kv := range [][2]string{{"app_pem", app.PEM}, {"app_client_secret", app.ClientSecret}, {"app_webhook_secret", app.WebhookSecret}} {
		if err := st.SetSecret(kv[0], kv[1]); err != nil {
			return fmt.Errorf("save %s: %w", kv[0], err)
		}
	}
	for _, kv := range [][2]string{
		{"app_id", strconv.FormatInt(app.ID, 10)}, {"app_slug", app.Slug}, {"app_name", app.Name},
		{"app_html_url", app.HTMLURL}, {"app_owner", app.Owner.Login}, {"app_client_id", app.ClientID},
	} {
		if err := st.Set(kv[0], kv[1]); err != nil {
			return fmt.Errorf("save %s: %w", kv[0], err)
		}
	}
	return nil
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	clientID := s.store.Get("app_client_id", "")
	if clientID == "" {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	st := s.newState("oauth")
	s.setCookie(w, r, stateCookie, st, 900)
	http.Redirect(w, r, gh.AuthorizeURL(clientID, s.baseURL(r)+"/auth/callback", st), http.StatusFound)
}

func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	st := r.URL.Query().Get("state")
	c, err := r.Cookie(stateCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(st)) != 1 || !s.consumeState("oauth", st) {
		http.Redirect(w, r, "/login?error=state", http.StatusFound)
		return
	}
	secret, _ := s.store.GetSecret("app_client_secret")
	exchange := gh.ExchangeCode
	if s.exchangeCode != nil {
		exchange = s.exchangeCode
	}
	login, userToken, err := exchange(r.Context(), s.store.Get("app_client_id", ""), secret, r.URL.Query().Get("code"), s.baseURL(r)+"/auth/callback")
	if err != nil {
		s.log.Error("oauth", "error", err)
		http.Redirect(w, r, "/login?error=oauth", http.StatusFound)
		return
	}
	// The first person to sign in with the setup token becomes the admin.
	if !s.store.HasUsers() && s.hasSetupCookie(r) {
		if _, err := s.store.PutUser(store.User{Login: login, Role: store.RoleAdmin, CreatedBy: "setup"}); err != nil {
			s.internalErr(w, r, http.StatusInternalServerError, "first_admin", "cannot register the first admin "+login, err)
			return
		}
		s.EnsureSetupToken() // deletes the token file
		s.setSetupToken("")
		s.log.Info("first admin registered", "login", login)
		if err := s.store.AddAudit(store.AuditEntry{Actor: login, Action: "user.add", Subject: login, Detail: "role admin (first admin, setup token)"}); err != nil {
			s.log.Error("write audit log", "error", err)
		}
	}
	role, memberships := s.signInRole(r.Context(), login, userToken)
	if role == "" {
		s.log.Warn("sign-in refused", "login", login)
		http.Redirect(w, r, "/login?error=forbidden&login="+url.QueryEscape(login), http.StatusFound) //nolint:gosec // G710: fixed local path, the login is query-escaped
		return
	}
	token := randomHex(32)
	ttl := sessionTTL
	if len(memberships) > 0 {
		ttl = ruleSessionTTL
	}
	s.log.Info("signed in", "login", login, "role", role, "rules", strings.Join(memberships, ","))
	if err := s.store.CreateSession(token, login, ttl, memberships...); err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "session_create", "cannot open the session", err)
		return
	}
	s.setCookie(w, r, sessionCookie, token, int(ttl/time.Second))
	s.setCookie(w, r, stateCookie, "", -1)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.store.DeleteSession(c.Value)
	}
	s.setCookie(w, r, sessionCookie, "", -1)
	writeJSON(w, map[string]bool{"ok": true})
}
