package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// Credentials returns the GitHub credentials currently configured.
func (s *Server) Credentials() scaler.Credentials {
	if s.cfg.PAT != "" {
		return scaler.Credentials{PAT: s.cfg.PAT}
	}
	pem, _ := s.store.GetSecret("app_pem")
	return scaler.Credentials{AppClientID: s.store.Get("app_client_id", ""), AppPEM: pem}
}

// appConfigured reports whether the setup saved a GitHub App.
func (s *Server) appConfigured() bool { return s.store.Get("app_client_id", "") != "" }

// errAppKey is a GitHub App private key that cannot be read back.
var errAppKey = errors.New("cannot decrypt app_pem, the GitHub App private key (was master.key replaced or lost?)")

// appClient returns a client authenticated as the GitHub App saved by the
// setup. A key that cannot be decrypted (master.key replaced or lost) is
// reported as such, not as an unparsable key.
func (s *Server) appClient() (*gh.AppClient, error) {
	pem, err := s.store.GetSecret("app_pem")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errAppKey, err)
	}
	appID, err := strconv.ParseInt(s.store.Get("app_id", "0"), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("stored GitHub App ID: %w", err)
	}
	return gh.NewAppClient(appID, pem)
}

// token returns a GitHub token able to read Actions data of a target.
func (s *Server) token(ctx context.Context, t store.Target) (string, error) {
	if s.cfg.PAT != "" {
		return s.cfg.PAT, nil
	}
	client, err := s.appClient()
	if err != nil {
		return "", err
	}
	return client.InstallationToken(ctx, t.InstallationID)
}

// GitHubRunners lists the self-hosted runners of a target through the REST
// API (used by the scaler to find orphan runners).
func (s *Server) GitHubRunners(ctx context.Context, t store.Target) ([]gh.Runner, error) {
	tok, err := s.token(ctx, t)
	if err != nil {
		return nil, err
	}
	repo := ""
	if t.Kind == "repo" {
		repo = t.Repo
	}
	return gh.ListRunners(ctx, tok, t.Owner, repo)
}

// JobQueuedAt asks GitHub when a job was queued (the scaler uses it when the
// job messages carry no queue time).
func (s *Server) JobQueuedAt(ctx context.Context, t store.Target, owner, repo string, runID int64, runner string) (time.Time, error) {
	tok, err := s.token(ctx, t)
	if err != nil {
		return time.Time{}, err
	}
	return gh.JobCreatedAt(ctx, tok, owner, repo, runID, runner)
}
