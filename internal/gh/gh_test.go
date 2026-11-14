package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
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
