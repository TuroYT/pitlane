package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

// ------------------------------------------------------------------- roles

// principal is the signed-in user of a request.
type principal struct {
	Login string
	Role  string
}

func (p principal) can(role string) bool { return store.RoleRank(p.Role) >= store.RoleRank(role) }

type principalKey struct{}

// currentUser returns the signed-in user of a request and their role, read
// again on every request: a removed user, or one whose rule was removed, is
// signed out at once, a demoted one loses the rights right away.
func (s *Server) currentUser(r *http.Request) (principal, bool) {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return p, true
	}
	if s.cfg.InsecureNoAuth {
		return principal{Login: "admin", Role: store.RoleAdmin}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return principal{}, false
	}
	sess, ok := s.store.Session(c.Value)
	if !ok {
		return principal{}, false
	}
	role := s.roleOf(sess.Login, sess.Memberships)
	if role == "" {
		return principal{}, false
	}
	return principal{Login: sess.Login, Role: role}, true
}

// roleOf is the role of a login: the one it was given as a user, else the
// highest role of the rules it matched at sign-in that still exist.
func (s *Server) roleOf(login string, memberships []string) string {
	if login == "" {
		return ""
	}
	if u, ok := s.store.User(login); ok {
		return u.Role
	}
	if len(memberships) == 0 {
		return ""
	}
	rules, err := s.store.AccessRules()
	if err != nil {
		s.log.Error("load access rules", "error", err)
		return ""
	}
	best := ""
	for _, rule := range rules {
		if containsFold(memberships, rule.Subject()) && store.RoleRank(rule.Role) > store.RoleRank(best) {
			best = rule.Role
		}
	}
	return best
}

// require serves h to signed-in users with at least role: 401 for anyone
// else, 403 for a lower role.
func (s *Server) require(role string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.currentUser(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if !p.can(role) {
			writeErr(w, http.StatusForbidden, fmt.Sprintf("this needs the %s role (you are %s)", role, p.Role))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}

// isMember checks a membership on GitHub with the user's access token
// (team "" for the whole organization).
func isMember(ctx context.Context, userToken, login, org, team string) (bool, error) {
	if team == "" {
		return gh.OrgMember(ctx, userToken, org, login)
	}
	return gh.TeamMember(ctx, userToken, org, team, login)
}

// signInRole decides the role of a user signing in: their user entry, else
// the highest role among the organization and team rules they match, asked
// to GitHub with their access token. A membership GitHub cannot confirm
// (missing permission, GitHub unreachable) grants nothing. It also returns
// the rules matched, kept with the session.
func (s *Server) signInRole(ctx context.Context, login, userToken string) (string, []string) {
	if u, ok := s.store.User(login); ok {
		return u.Role, nil
	}
	rules, err := s.store.AccessRules()
	if err != nil {
		s.log.Error("load access rules", "error", err)
		return "", nil
	}
	if len(rules) == 0 || userToken == "" {
		return "", nil
	}
	check := isMember
	if s.member != nil {
		check = s.member
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	best, matched, seen := "", []string{}, map[string]bool{}
	for _, rule := range rules {
		subject := rule.Subject()
		ok, done := seen[subject]
		if !done {
			ok, err = check(ctx, userToken, login, rule.Org, rule.Team)
			if err != nil {
				s.log.Warn("cannot check a membership at sign-in: the rule is ignored", "login", login, "rule", subject, "error", err)
			}
			seen[subject] = ok
			if ok {
				matched = append(matched, subject)
			}
		}
		if ok && store.RoleRank(rule.Role) > store.RoleRank(best) {
			best = rule.Role
		}
	}
	return best, matched
}

// ------------------------------------------------------------------- audit

// audit records an admin action in the audit log (and Pitlane's log).
func (s *Server) audit(r *http.Request, action, subject, detail string) {
	p, _ := s.currentUser(r)
	actor := p.Login
	if actor == "" {
		actor = "setup"
	}
	s.log.Info("admin action", "actor", actor, "action", action, "subject", subject, "detail", detail)
	if err := s.store.AddAudit(store.AuditEntry{Actor: actor, Action: action, Subject: subject, Detail: detail}); err != nil {
		s.log.Error("write audit log", "action", action, "error", err)
	}
}

// changes describes what differs between two sets of values, by key, in
// the order of keys: "key: old → new; …". Keys missing from after did not
// change.
func changes(keys []string, before, after map[string]string) string {
	var out []string
	for _, k := range keys {
		a, ok := after[k]
		if b := before[k]; ok && a != b {
			out = append(out, fmt.Sprintf("%s: %s → %s", k, orNone(b), orNone(a)))
		}
	}
	return strings.Join(out, "; ")
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}

// Audit log page sizes (?limit=).
const (
	defaultAuditPage = 50
	maxAuditPage     = 500
)

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > maxAuditPage {
		limit = defaultAuditPage
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	entries, total, err := s.store.AuditPage(r.Context(), limit, max(0, offset))
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "audit_list", "cannot read the audit log", err)
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, entries)
}

// ------------------------------------------------------------------- users

// maxAccessBody bounds the bodies of the user and access rule requests.
const maxAccessBody = 4096

// validLogin accepts a GitHub login (or organization name): letters,
// digits and hyphens, at most 39 characters.
func validLogin(s string) bool {
	if s == "" || len(s) > 39 {
		return false
	}
	for _, c := range s {
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// validTeam accepts a team slug.
func validTeam(s string) bool { return validName(s) }

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.Users()
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "users_list", "cannot list the users", err)
		return
	}
	rules, err := s.store.AccessRules()
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "rules_list", "cannot list the access rules", err)
		return
	}
	writeJSON(w, map[string]any{"users": users, "rules": rules})
}

// storeErr answers a failed user or access rule change: the store's own
// refusals are shown as they are, anything else is an internal error.
func (s *Server) storeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		writeErr(w, http.StatusConflict, store.ErrLastAdmin.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, store.ErrNotFound.Error())
	default:
		s.internalErr(w, r, http.StatusInternalServerError, "access_save", "cannot save the change", err)
	}
}

func (s *Server) handlePutUser(w http.ResponseWriter, r *http.Request) {
	login := strings.TrimPrefix(strings.TrimSpace(r.PathValue("login")), "@")
	var body struct {
		Role string `json:"role"`
	}
	if !readJSON(w, http.MaxBytesReader(w, r.Body, maxAccessBody), &body) {
		return
	}
	if !validLogin(login) {
		writeErr(w, http.StatusBadRequest, "invalid GitHub login")
		return
	}
	if !store.ValidRole(body.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin, operator or viewer")
		return
	}
	p, _ := s.currentUser(r)
	prev, err := s.store.PutUser(store.User{Login: login, Role: body.Role, CreatedBy: p.Login})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	switch prev {
	case "":
		s.audit(r, "user.add", login, "role "+body.Role)
	case body.Role:
	default:
		s.audit(r, "user.role", login, prev+" → "+body.Role)
	}
	s.handleUsers(w, r)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.DeleteUser(r.PathValue("login"))
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	s.audit(r, "user.remove", u.Login, "was "+u.Role)
	s.handleUsers(w, r)
}

func (s *Server) handlePutRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Org  string `json:"org"`
		Team string `json:"team"`
		Role string `json:"role"`
	}
	if !readJSON(w, http.MaxBytesReader(w, r.Body, maxAccessBody), &body) {
		return
	}
	org, team := strings.TrimSpace(body.Org), strings.TrimSpace(body.Team)
	if o, t, ok := strings.Cut(org, "/"); ok && team == "" {
		org, team = o, t // "org/team"
	}
	if !validLogin(org) || (team != "" && !validTeam(team)) {
		writeErr(w, http.StatusBadRequest, "invalid organization or team")
		return
	}
	if !store.ValidRole(body.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin, operator or viewer")
		return
	}
	p, _ := s.currentUser(r)
	rule, err := s.store.PutAccessRule(store.AccessRule{Org: org, Team: team, Role: body.Role, CreatedBy: p.Login})
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	s.audit(r, "rule.put", rule.Subject(), "role "+rule.Role)
	s.handleUsers(w, r)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rule, err := s.store.DeleteAccessRule(id)
	if err != nil {
		s.storeErr(w, r, err)
		return
	}
	s.audit(r, "rule.remove", rule.Subject(), "was "+rule.Role)
	s.handleUsers(w, r)
}

// containsFold reports whether list holds v, ignoring case and the spaces
// around list's items.
func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), v) {
			return true
		}
	}
	return false
}
