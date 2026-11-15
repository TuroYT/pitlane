package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeAPI(t *testing.T, routes map[string]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })
}

func TestRepoPrivate(t *testing.T) {
	fakeAPI(t, map[string]string{
		"/repos/acme/pub":  `{"private":false,"visibility":"public"}`,
		"/repos/acme/priv": `{"private":true}`,
		"/repos/acme/odd":  `{}`,
	})
	ctx := context.Background()
	if p, err := RepoPrivate(ctx, "tok", "acme", "pub"); err != nil || p {
		t.Errorf("pub: %v %v", p, err)
	}
	if p, err := RepoPrivate(ctx, "tok", "acme", "priv"); err != nil || !p {
		t.Errorf("priv: %v %v", p, err)
	}
	for _, repo := range []string{"odd", "missing"} {
		if _, err := RepoPrivate(ctx, "tok", "acme", repo); err == nil {
			t.Errorf("%s: an unknown visibility must be an error", repo)
		}
	}
}

func TestRunnerGroupAllowsPublic(t *testing.T) {
	fakeAPI(t, map[string]string{
		"/orgs/open/actions/runner-groups/1":   `{"id":1,"name":"Default","allows_public_repositories":true}`,
		"/orgs/closed/actions/runner-groups/1": `{"id":1,"name":"Default","allows_public_repositories":false}`,
		"/orgs/odd/actions/runner-groups/1":    `{"id":1}`,
	})
	ctx := context.Background()
	if p, err := RunnerGroupAllowsPublic(ctx, "tok", "open", 1); err != nil || !p {
		t.Errorf("open: %v %v", p, err)
	}
	if p, err := RunnerGroupAllowsPublic(ctx, "tok", "closed", 1); err != nil || p {
		t.Errorf("closed: %v %v", p, err)
	}
	for _, org := range []string{"odd", "missing"} {
		if _, err := RunnerGroupAllowsPublic(ctx, "tok", org, 1); err == nil {
			t.Errorf("%s: an unknown setting must be an error", org)
		}
	}
}

// fakeMembers is a GitHub API answering membership requests: active org
// members (private membership, needs the Members permission), public org
// members and team members, "org/team".
type fakeMembers struct {
	private, public, teams map[string]bool
	noPermission           bool // the App lacks the Members permission
}

func (f fakeMembers) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		authed := r.Header.Get("Authorization") == "Bearer user-tok"
		switch {
		case len(p) == 4 && p[0] == "user" && p[1] == "memberships" && p[2] == "orgs":
			if !authed {
				w.WriteHeader(http.StatusUnauthorized)
			} else if f.noPermission {
				w.WriteHeader(http.StatusForbidden)
			} else if f.private[p[3]] {
				_, _ = w.Write([]byte(`{"state":"active"}`))
			} else if p[3] == "invited" {
				_, _ = w.Write([]byte(`{"state":"pending"}`))
			} else { // not a member, or the App is not installed there
				w.WriteHeader(http.StatusNotFound)
			}
		case len(p) == 4 && p[0] == "orgs" && p[2] == "public_members":
			if f.public[p[1]] && p[3] == "alice" {
				w.WriteHeader(http.StatusNoContent)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		case len(p) == 6 && p[0] == "orgs" && p[2] == "teams" && p[4] == "memberships":
			switch {
			case !authed:
				w.WriteHeader(http.StatusUnauthorized)
			case f.noPermission:
				w.WriteHeader(http.StatusForbidden)
			case f.teams[p[1]+"/"+p[3]] && p[5] == "alice":
				_, _ = w.Write([]byte(`{"state":"active","role":"member"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old })
}

func TestOrgAndTeamMembership(t *testing.T) {
	ctx := context.Background()
	f := fakeMembers{private: map[string]bool{"acme": true}, public: map[string]bool{"oss": true}, teams: map[string]bool{"acme/ops": true}}
	f.serve(t)
	for _, tc := range []struct {
		org  string
		want bool
	}{{"acme", true}, {"oss", true}, {"invited", false}, {"other", false}} {
		if got, err := OrgMember(ctx, "user-tok", tc.org, "alice"); err != nil || got != tc.want {
			t.Errorf("org %s: %v %v", tc.org, got, err)
		}
	}
	if got, err := TeamMember(ctx, "user-tok", "acme", "ops", "alice"); err != nil || !got {
		t.Errorf("team member: %v %v", got, err)
	}
	if got, err := TeamMember(ctx, "user-tok", "acme", "dev", "alice"); err != nil || got {
		t.Errorf("other team: %v %v", got, err)
	}

	// Without the Members permission: public membership still works, a
	// private one is not seen, teams cannot be checked.
	f.noPermission = true
	f.serve(t)
	if got, err := OrgMember(ctx, "user-tok", "oss", "alice"); err != nil || !got {
		t.Errorf("public member without permission: %v %v", got, err)
	}
	if got, err := OrgMember(ctx, "user-tok", "acme", "alice"); err != nil || got {
		t.Errorf("private member without permission: %v %v", got, err)
	}
	if got, err := TeamMember(ctx, "user-tok", "acme", "ops", "alice"); err == nil || got {
		t.Errorf("team without permission: %v %v", got, err)
	}
}
