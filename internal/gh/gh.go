// Package gh is a small GitHub REST client covering what Pitlane needs: the
// GitHub App manifest flow, installations, repositories and OAuth login.
package gh

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var apiBase = "https://api.github.com" // tests point it at a fake server

// oauthBase hosts the OAuth endpoints (authorize and access_token); tests
// point it at a fake server too.
var oauthBase = "https://github.com"

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

// maxAppPages bounds the pages read from a list (100 items each), so that
// at most 2,000 installations or repositories are returned.
const maxAppPages = 20

// Installations lists the accounts the App is installed on.
func (c *AppClient) Installations(ctx context.Context) ([]Installation, error) {
	j, err := c.jwt()
	if err != nil {
		return nil, err
	}
	var all []Installation
	for page := 1; page <= maxAppPages; page++ {
		var out []Installation
		u := fmt.Sprintf("%s/app/installations?per_page=100&page=%d", apiBase, page)
		if err := do(ctx, http.MethodGet, u, "Bearer "+j, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out...)
		if len(out) < 100 {
			break
		}
	}
	return all, nil
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
	for page := 1; page <= maxAppPages; page++ {
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
	return oauthBase + "/login/oauth/authorize?" + q.Encode()
}

// StatusError is a GitHub API answer with an error status.
type StatusError struct {
	Code int
	msg  string
}

func (e *StatusError) Error() string { return e.msg }

// statusCode returns the HTTP status of a GitHub API error, 0 for another
// error.
func statusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// ExchangeCode turns an OAuth code into the login of the signed-in user and
// their user access token (used right away for membership checks, never
// stored).
func ExchangeCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (login, token string, err error) {
	body := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code}, "redirect_uri": {redirectURI}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthBase+"/login/oauth/access_token", strings.NewReader(body.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Code        string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", "", err
	}
	if tok.AccessToken == "" {
		// GitHub answers 200 with an error payload; the description is
		// optional, so fall back on the error code.
		switch {
		case tok.Description != "":
			return "", "", fmt.Errorf("oauth: %s", tok.Description)
		case tok.Code != "":
			return "", "", fmt.Errorf("oauth: %s", tok.Code)
		}
		return "", "", errors.New("oauth: no access token in the answer")
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := do(ctx, http.MethodGet, apiBase+"/user", "Bearer "+tok.AccessToken, nil, &user); err != nil {
		return "", "", err
	}
	return user.Login, tok.AccessToken, nil
}

// OrgMember reports whether login is an active member of org, asked with
// the user's own access token. GitHub answers that for a GitHub App user
// token only when the App has the "Members" organization permission and is
// installed on org; otherwise the public membership is checked, which needs
// no permission (a member who keeps their membership private is then not
// seen).
func OrgMember(ctx context.Context, userToken, org, login string) (bool, error) {
	var m struct {
		State string `json:"state"`
		Role  string `json:"role"`
	}
	err := do(ctx, http.MethodGet, apiBase+"/user/memberships/orgs/"+url.PathEscape(org), "Bearer "+userToken, nil, &m)
	if err == nil {
		// billing_manager (and any role GitHub may add) is not membership.
		return m.State == "active" && (m.Role == "member" || m.Role == "admin"), nil
	}
	if c := statusCode(err); c != http.StatusForbidden && c != http.StatusNotFound && c != http.StatusUnauthorized {
		return false, err
	}
	err = do(ctx, http.MethodGet, apiBase+"/orgs/"+url.PathEscape(org)+"/public_members/"+url.PathEscape(login), "", nil, nil)
	if statusCode(err) == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// TeamMember reports whether login is an active member of the team (by
// slug) of org, asked with the user's own access token. It needs the
// "Members" organization permission on a GitHub App installed on org.
// GitHub answers 404 both to "not a member" and to a team the token cannot
// see: either way the user is not a member as far as Pitlane can tell.
// Other refusals (403: missing permission) are errors.
func TeamMember(ctx context.Context, userToken, org, team, login string) (bool, error) {
	var m struct {
		State string `json:"state"`
	}
	err := do(ctx, http.MethodGet, fmt.Sprintf("%s/orgs/%s/teams/%s/memberships/%s", apiBase, url.PathEscape(org), url.PathEscape(team), url.PathEscape(login)),
		"Bearer "+userToken, nil, &m)
	if statusCode(err) == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return m.State == "active", nil
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

// JobRun is one completed job of a workflow run.
type JobRun struct {
	Workflow   string
	Name       string
	RunnerName string
	Seconds    float64
}

// RecentJobs returns the successful jobs of the last completed workflow runs.
func RecentJobs(ctx context.Context, token, owner, repo string, maxRuns int) ([]JobRun, error) {
	var runs struct {
		Runs []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"workflow_runs"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs?status=completed&per_page=%d", apiBase, url.PathEscape(owner), url.PathEscape(repo), min(maxRuns, 100))
	if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &runs); err != nil {
		return nil, err
	}
	var out []JobRun
	for _, r := range runs.Runs {
		var jobs struct {
			Jobs []struct {
				Name        string    `json:"name"`
				RunnerName  string    `json:"runner_name"`
				Conclusion  string    `json:"conclusion"`
				StartedAt   time.Time `json:"started_at"`
				CompletedAt time.Time `json:"completed_at"`
			} `json:"jobs"`
		}
		u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?per_page=100", apiBase, url.PathEscape(owner), url.PathEscape(repo), r.ID)
		if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &jobs); err != nil {
			continue
		}
		for _, j := range jobs.Jobs {
			if j.Conclusion != "success" || j.StartedAt.IsZero() || j.CompletedAt.Before(j.StartedAt) {
				continue
			}
			out = append(out, JobRun{Workflow: r.Name, Name: j.Name, RunnerName: j.RunnerName, Seconds: j.CompletedAt.Sub(j.StartedAt).Seconds()})
		}
	}
	return out, nil
}

// RunJobID returns the ID of the job a runner ran in a workflow run (the ID
// used in job page URLs and the jobs API).
func RunJobID(ctx context.Context, token, owner, repo string, runID int64, runnerName string) (int64, error) {
	var jobs struct {
		Jobs []struct {
			ID         int64  `json:"id"`
			RunnerName string `json:"runner_name"`
		} `json:"jobs"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?filter=all&per_page=100", apiBase, url.PathEscape(owner), url.PathEscape(repo), runID)
	if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &jobs); err != nil {
		return 0, err
	}
	var jobID int64
	for _, j := range jobs.Jobs {
		if j.RunnerName == runnerName {
			jobID = j.ID
		}
	}
	if jobID == 0 {
		return 0, fmt.Errorf("job of runner %s not found in run %d", runnerName, runID)
	}
	return jobID, nil
}

// jobsPerPage is the page size of the jobs listing, the most GitHub allows.
const jobsPerPage = 100

// maxJobPages bounds how many pages JobCreatedAt reads (1000 jobs), so a
// huge run cannot turn one lookup into dozens of requests.
const maxJobPages = 10

// JobCreatedAt returns when GitHub queued the job a runner ran in a workflow
// run (the job's created_at, the start of its wait for a runner). It reads
// the run's jobs page by page until it finds the runner's job.
func JobCreatedAt(ctx context.Context, token, owner, repo string, runID int64, runnerName string) (time.Time, error) {
	for page := 1; page <= maxJobPages; page++ {
		var jobs struct {
			Jobs []struct {
				RunnerName string    `json:"runner_name"`
				CreatedAt  time.Time `json:"created_at"`
			} `json:"jobs"`
		}
		u := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs?filter=all&per_page=%d&page=%d", apiBase, url.PathEscape(owner), url.PathEscape(repo), runID, jobsPerPage, page)
		if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &jobs); err != nil {
			return time.Time{}, err
		}
		for _, j := range jobs.Jobs {
			if j.RunnerName == runnerName && !j.CreatedAt.IsZero() {
				return j.CreatedAt, nil
			}
		}
		if len(jobs.Jobs) < jobsPerPage {
			break
		}
	}
	return time.Time{}, fmt.Errorf("job of runner %s not found in run %d", runnerName, runID)
}

// JobLog downloads the log of the job a given runner ran in a workflow run.
// token is an installation token or a personal access token.
func JobLog(ctx context.Context, token, owner, repo string, runID int64, runnerName string) (io.ReadCloser, error) {
	jobID, err := RunJobID(ctx, token, owner, repo, runID, runnerName)
	if err != nil {
		return nil, err
	}
	req, err := newRequest(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs", apiBase, url.PathEscape(owner), url.PathEscape(repo), jobID), "Bearer "+token, nil)
	if err != nil {
		return nil, err
	}
	// The API redirects to a signed blob URL; Go drops the Authorization
	// header on the cross-host redirect.
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, withoutQuery(err)
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("download job log: %s", resp.Status)
	}
	return resp.Body, nil
}

// withoutQuery drops the query of the URL in a request error: the signed
// blob URL a log download is redirected to carries its signature there.
func withoutQuery(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	u, perr := url.Parse(ue.URL)
	if perr != nil {
		return &url.Error{Op: ue.Op, URL: "(unparsable URL)", Err: ue.Err}
	}
	u.RawQuery, u.Fragment = "", ""
	return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
}

// newRequest builds a GitHub API request with the headers every call sends.
// auth is the Authorization header value ("" for none); body, when not nil,
// is sent as JSON.
func newRequest(ctx context.Context, method, u, auth string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
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
	return req, nil
}

func do(ctx context.Context, method, u, auth string, body any, out any) error {
	req, err := newRequest(ctx, method, u, auth, body)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &StatusError{Code: resp.StatusCode, msg: fmt.Sprintf("github %s %s: %d %s", method, u, resp.StatusCode, strings.TrimSpace(string(msg)))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Runner is a self-hosted runner as listed by the REST API.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "online" or "offline"
	Busy   bool   `json:"busy"`
}

// ListRunners returns the self-hosted runners of a repository (repo != "")
// or an organization. token is an installation token or a personal access
// token. At most 1,000 runners are returned.
func ListRunners(ctx context.Context, token, owner, repo string) ([]Runner, error) {
	base := fmt.Sprintf("%s/orgs/%s/actions/runners", apiBase, url.PathEscape(owner))
	if repo != "" {
		base = fmt.Sprintf("%s/repos/%s/%s/actions/runners", apiBase, url.PathEscape(owner), url.PathEscape(repo))
	}
	var out []Runner
	for page := 1; page <= 10; page++ {
		var res struct {
			Total   int      `json:"total_count"`
			Runners []Runner `json:"runners"`
		}
		if err := do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", base, page), "Bearer "+token, nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Runners...)
		if len(res.Runners) < 100 || len(out) >= res.Total {
			break
		}
	}
	return out, nil
}

// RepoPrivate reports whether a repository is private (or internal). token
// is an installation token or a personal access token.
func RepoPrivate(ctx context.Context, token, owner, repo string) (bool, error) {
	var r struct {
		Private *bool `json:"private"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s", apiBase, url.PathEscape(owner), url.PathEscape(repo))
	if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &r); err != nil {
		return false, err
	}
	if r.Private == nil {
		return false, fmt.Errorf("visibility of %s/%s not returned", owner, repo)
	}
	return *r.Private, nil
}

// RunnerGroupAllowsPublic reports whether an organisation runner group lets
// public repositories use its runners.
func RunnerGroupAllowsPublic(ctx context.Context, token, org string, groupID int) (bool, error) {
	var g struct {
		AllowsPublic *bool `json:"allows_public_repositories"`
	}
	u := fmt.Sprintf("%s/orgs/%s/actions/runner-groups/%d", apiBase, url.PathEscape(org), groupID)
	if err := do(ctx, http.MethodGet, u, "Bearer "+token, nil, &g); err != nil {
		return false, err
	}
	if g.AllowsPublic == nil {
		return false, fmt.Errorf("runner group %d of %s: allows_public_repositories not returned", groupID, org)
	}
	return *g.AllowsPublic, nil
}
