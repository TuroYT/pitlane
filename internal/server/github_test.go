package server

import (
	"context"
	"testing"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestCredentials(t *testing.T) {
	s, st := authServer(t, "alice")
	if err := st.SetSecret("app_pem", "PEM"); err != nil {
		t.Fatal(err)
	}
	if c := s.Credentials(); c.AppClientID != "Iv1.test" || c.AppPEM != "PEM" || c.PAT != "" {
		t.Fatalf("App credentials %+v", c)
	}
	s.cfg.PAT = "ghp_x"
	if c := s.Credentials(); c.PAT != "ghp_x" || c.AppPEM != "" || c.AppClientID != "" {
		t.Fatalf("a personal access token takes over: %+v", c)
	}
}

func TestTokenSources(t *testing.T) {
	s, _ := authServer(t, "alice")
	tg := store.Target{Kind: "repo", Owner: "acme", Repo: "api", InstallationID: 7}
	s.cfg.PAT = "ghp_x"
	if tok, err := s.token(context.Background(), tg); err != nil || tok != "ghp_x" {
		t.Fatalf("PAT: %q %v", tok, err)
	}
	// Without a PAT the App is needed: with no key saved, no token (and no
	// call to GitHub is made).
	s.cfg.PAT = ""
	if _, err := s.token(context.Background(), tg); err == nil {
		t.Fatal("a token without any credential")
	}
	if _, err := s.GitHubRunners(context.Background(), tg); err == nil {
		t.Fatal("runners listed without a token")
	}
	if _, err := s.JobQueuedAt(context.Background(), tg, "acme", "api", 1, "pitlane-a"); err == nil {
		t.Fatal("queue time read without a token")
	}
	if _, err := s.StrictReason(context.Background(), tg); err == nil {
		t.Fatal("strict check without a token")
	}
}
