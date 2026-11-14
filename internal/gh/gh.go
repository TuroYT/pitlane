// Package gh is a small GitHub REST client covering what Pitlane needs: the
// GitHub App manifest flow, installations, repositories and OAuth login.
package gh

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const apiBase = "https://api.github.com"

var httpClient = &http.Client{Timeout: 30 * time.Second}

// App holds the credentials of the GitHub App Pitlane created for itself.
type App struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	HTMLURL       string `json:"html_url"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Manifest builds the GitHub App manifest submitted to github.com.
// baseURL is the public URL of the Pitlane UI (e.g. http://pitlane.lan:8080).
func Manifest(name, baseURL string) map[string]any {
	baseURL = strings.TrimRight(baseURL, "/")
	return map[string]any{
		"name":          name,
		"url":           "https://github.com/TuroYT/pitlane",
		"description":   "Self-hosted, autoscaled GitHub Actions runners managed by Pitlane.",
		"public":        false,
		"redirect_url":  baseURL + "/setup/callback",
		"callback_urls": []string{baseURL + "/auth/callback"},
		"setup_url":     baseURL + "/setup",
		"hook_attributes": map[string]any{
			"url":    baseURL + "/webhook",
			"active": false,
		},
		"request_oauth_on_install": false,
		"default_permissions": map[string]string{
			"administration":                   "write", // repository-level runners
			"organization_self_hosted_runners": "write", // organisation-level runners
			"actions":                          "read",
			"metadata":                         "read",
		},
		"default_events": []string{},
	}
}

// ConvertManifest exchanges the temporary code returned by the manifest flow
// for the App credentials.
func ConvertManifest(ctx context.Context, code string) (*App, error) {
	var app App
	err := do(ctx, http.MethodPost, apiBase+"/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &app)
	return &app, err
}

// Installation of the App on a user or organisation account.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"` // "User" or "Organization"
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
}

// Repo is a repository reachable through an installation.
type Repo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// AppClient calls the GitHub API as the App or one of its installations.
type AppClient struct {
	appID string
	key   *rsa.PrivateKey
}

// NewAppClient parses the App private key.
func NewAppClient(appID int64, pem string) (*AppClient, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pem))
	if err != nil {
		return nil, fmt.Errorf("parse app private key: %w", err)
	}
	return &AppClient{appID: strconv.FormatInt(appID, 10), key: key}, nil
}

func (c *AppClient) jwt() (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    c.appID,
	})
	return tok.SignedString(c.key)
}

// Installations lists the accounts the App is installed on.
func (c *AppClient) Installations(ctx context.Context) ([]Installation, error) {
	j, err := c.jwt()
	if err != nil {
		return nil, err
	}
	var out []Installation
	err = do(ctx, http.MethodGet, apiBase+"/app/installations?per_page=100", "Bearer "+j, nil, &out)
	return out, err
}

// InstallationToken returns a short-lived token for an installation.
func (c *AppClient) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	j, err := c.jwt()
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	err = do(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, installationID), "Bearer "+j, nil, &out)
	return out.Token, err
}

// Repositories lists the repositories an installation can access.
func (c *AppClient) Repositories(ctx context.Context, installationID int64) ([]Repo, error) {
	tok, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var all []Repo
	for page := 1; page <= 20; page++ {
		var out struct {
			Repositories []Repo `json:"repositories"`
		}
		u := fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", apiBase, page)
		if err := do(ctx, http.MethodGet, u, "token "+tok, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(out.Repositories) < 100 {
			break
		}
	}
	return all, nil
}

// ------------------------------------------------------------------ OAuth

// AuthorizeURL is where users are sent to sign in to the UI.
func AuthorizeURL(clientID, redirectURI, state string) string {
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state}}
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

// ExchangeCode turns an OAuth code into the login of the signed-in user.
func ExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (string, error) {
	body := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}, "redirect_uri": {redirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(body.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("oauth: %s", tok.Error)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := do(ctx, http.MethodGet, apiBase+"/user", "Bearer "+tok.AccessToken, nil, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

// ---------------------------------------------------------------- helpers

// LatestRunnerVersion returns the latest actions/runner release tag (e.g. "2.330.0").
func LatestRunnerVersion(ctx context.Context) (string, error) {
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := do(ctx, http.MethodGet, apiBase+"/repos/actions/runner/releases/latest", "", nil, &rel); err != nil {
		return "", err
	}
	return strings.TrimPrefix(rel.TagName, "v"), nil
}

// JobLog downloads the log of the job a given runner ran in a workflow run.
// token is an installation token or a personal access token.
func JobLog(ctx context.Context, token, owner, repo string, runID int64, runnerName string) (io.ReadCloser, error) {
	var jobs struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			RunnerName string `json:"runner_name"`
		} `json:"jobs"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?filter=all&per_page=100", apiBase, url.PathEscape(owner), url.PathEscape(repo), runID)
	if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &jobs); err != nil {
		return nil, err
	}
	var jobID int64
	for _, j := range jobs.Jobs {
		if j.RunnerName == runnerName {
			jobID = j.ID
		}
	}
	if jobID == 0 {
		return nil, fmt.Errorf("job of runner %s not found in run %d", runnerName, runID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs", apiBase, url.PathEscape(owner), url.PathEscape(repo), jobID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pitlane")
	// The API redirects to a signed blob URL; Go drops the Authorization
	// header on the cross-host redirect.
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("download job log: %s", resp.Status)
	}
	return resp.Body, nil
}

func do(ctx context.Context, method, u, auth string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pitlane")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("github %s %s: %d %s", method, u, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
