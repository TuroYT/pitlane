package gh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestNewAppClient(t *testing.T) {
	t.Parallel()
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey())
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		pem     string
		wantErr bool
	}{
		{"PKCS#1 key as GitHub gives it", pkcs1PEM(testKey()), false},
		{"PKCS#8 key", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})), false},
		{"empty", "", true},
		{"not PEM", "hello", true},
		{"garbage in a PEM block", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("nope")})), true},
		{"not an RSA key", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER})), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewAppClient(42, tc.pem)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), "parse app private key") {
					t.Errorf("error %q does not say what failed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.appID != "42" {
				t.Errorf("appID = %q, want 42", c.appID)
			}
		})
	}
}

// verifyAppJWT checks that raw is an RS256 token signed by testKey for app
// 12345 and returns its claims.
func verifyAppJWT(t *testing.T, raw string) *jwt.RegisteredClaims {
	t.Helper()
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return &testKey().PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		t.Fatalf("token does not verify: %v", err)
	}
	if claims.Issuer != "12345" {
		t.Errorf("iss = %q, want the app ID 12345", claims.Issuer)
	}
	return claims
}

func TestAppJWT(t *testing.T) {
	t.Parallel()
	c := newTestClient(t)
	before := time.Now()
	raw, err := c.jwt()
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	claims := verifyAppJWT(t, raw)

	// GitHub rejects a token issued in its future (clock drift) and one that
	// lives longer than 10 minutes.
	if iat := claims.IssuedAt.Time; !iat.Before(before) || before.Sub(iat) > 2*time.Minute {
		t.Errorf("iat = %v, want slightly before %v", iat, before)
	}
	exp := claims.ExpiresAt.Time
	if !exp.After(after) {
		t.Errorf("exp = %v is not in the future", exp)
	}
	if exp.Sub(claims.IssuedAt.Time) > 10*time.Minute {
		t.Errorf("lifetime %v exceeds GitHub's 10 minute limit", exp.Sub(claims.IssuedAt.Time))
	}
	if exp.After(after.Add(10 * time.Minute)) {
		t.Errorf("exp = %v is more than 10 minutes from now", exp)
	}
}

// A token signed with another key must not verify: the signature is what
// proves to GitHub that the caller owns the App.
func TestAppJWTSignature(t *testing.T) {
	t.Parallel()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewAppClient(12345, pkcs1PEM(otherKey))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := other.jwt()
	if err != nil {
		t.Fatal(err)
	}
	_, err = jwt.Parse(raw, func(*jwt.Token) (any, error) { return &testKey().PublicKey, nil })
	if err == nil {
		t.Error("a token signed by another key verified with testKey")
	}
}

// requireGitHubHeaders fails the test when a request lacks the headers GitHub
// expects from every API call.
func requireGitHubHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	for k, want := range map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
		"User-Agent":           "pitlane",
	} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("%s %s: header %s = %q, want %q", r.Method, r.URL.Path, k, got, want)
		}
	}
}

// requireAppJWT fails the test unless the request carries a valid App JWT.
func requireAppJWT(t *testing.T, r *http.Request) {
	t.Helper()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		t.Errorf("%s %s: Authorization = %q, want a Bearer JWT", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		return
	}
	verifyAppJWT(t, raw)
}

func TestInstallations(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireGitHubHeaders(t, r)
		requireAppJWT(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/app/installations" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q, want 100", got)
		}
		_, _ = w.Write([]byte(`[
			{"id":1,"account":{"login":"acme","type":"Organization"},"repository_selection":"all"},
			{"id":2,"account":{"login":"alice","type":"User"},"repository_selection":"selected"}]`))
	}))
	got, err := newTestClient(t).Installations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d installations, want 2", len(got))
	}
	if got[0].ID != 1 || got[0].Account.Login != "acme" || got[0].Account.Type != "Organization" || got[0].RepositorySelection != "all" {
		t.Errorf("first installation = %+v", got[0])
	}
	if got[1].ID != 2 || got[1].Account.Login != "alice" || got[1].Account.Type != "User" || got[1].RepositorySelection != "selected" {
		t.Errorf("second installation = %+v", got[1])
	}
}

func TestAppEndpointErrors(t *testing.T) {
	calls := map[string]func(*AppClient) error{
		"Installations": func(c *AppClient) error {
			_, err := c.Installations(context.Background())
			return err
		},
		"InstallationToken": func(c *AppClient) error {
			_, err := c.InstallationToken(context.Background(), 7)
			return err
		},
		"Repositories": func(c *AppClient) error {
			_, err := c.Repositories(context.Background(), 7)
			return err
		},
	}
	replies := []struct {
		name   string
		status int
		body   string
		// code is the status the StatusError must carry, 0 when the failure
		// is not an API status (a body that cannot be decoded).
		code int
	}{
		{"bad credentials", http.StatusUnauthorized, `{"message":"Bad credentials"}`, http.StatusUnauthorized},
		{"installation gone", http.StatusNotFound, `{"message":"Not Found"}`, http.StatusNotFound},
		{"rate limited", http.StatusForbidden, `{"message":"API rate limit exceeded"}`, http.StatusForbidden},
		{"secondary rate limit", http.StatusTooManyRequests, `{"message":"slow down"}`, http.StatusTooManyRequests},
		{"server error", http.StatusInternalServerError, ``, http.StatusInternalServerError},
		{"bad gateway", http.StatusBadGateway, `<html>502</html>`, http.StatusBadGateway},
		{"malformed JSON", http.StatusOK, `{"token": `, 0},
	}
	for name, call := range calls {
		for _, rep := range replies {
			t.Run(name+"/"+rep.name, func(t *testing.T) {
				serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(rep.status)
					_, _ = w.Write([]byte(rep.body))
				}))
				err := call(newTestClient(t))
				if err == nil {
					t.Fatal("expected an error")
				}
				if got := statusCode(err); got != rep.code {
					t.Errorf("status = %d, want %d (err: %v)", got, rep.code, err)
				}
			})
		}
	}
}

func TestInstallationToken(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireGitHubHeaders(t, r)
		requireAppJWT(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/987/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_abc","expires_at":"2026-01-02T10:00:00Z","permissions":{"actions":"read"}}`))
	}))
	tok, err := newTestClient(t).InstallationToken(context.Background(), 987)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_abc" {
		t.Errorf("token = %q, want ghs_abc", tok)
	}
}

func TestInstallationTokenCanceled(t *testing.T) {
	c := newTestClient(t)
	// Canceled while the request is in flight: the handler cancels, then
	// waits for the client to hang up.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	if _, err := c.InstallationToken(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}

	// Canceled before the call: no request must reach the server.
	hit := false
	serveAPI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := c.InstallationToken(done, 1); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if hit {
		t.Error("a canceled context still sent a request")
	}
}

func TestInstallationTokenDeadline(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	serveAPI(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if _, err := c.InstallationToken(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// fakeRepos serves /app/installations/{id}/access_tokens and a paginated
// /installation/repositories of total repositories, counting page requests.
type fakeRepos struct {
	t     *testing.T
	total int
	pages []int
}

func (f *fakeRepos) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requireGitHubHeaders(f.t, r)
	switch r.URL.Path {
	case "/app/installations/7/access_tokens":
		requireAppJWT(f.t, r)
		_, _ = w.Write([]byte(`{"token":"inst-tok"}`))
	case "/installation/repositories":
		if got := r.Header.Get("Authorization"); got != "token inst-tok" {
			f.t.Errorf("Authorization = %q, want the installation token", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			f.t.Errorf("per_page = %q, want 100", got)
		}
		var page int
		if _, err := fmt.Sscan(r.URL.Query().Get("page"), &page); err != nil {
			f.t.Errorf("page = %q: %v", r.URL.Query().Get("page"), err)
		}
		f.pages = append(f.pages, page)
		type repo struct {
			Name     string `json:"name"`
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		}
		var repos []repo
		for i := (page-1)*100 + 1; i <= min(page*100, f.total); i++ {
			rp := repo{Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("acme/r%d", i), Private: i%2 == 0}
			rp.Owner.Login = "acme"
			repos = append(repos, rp)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": f.total, "repositories": repos})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestRepositoriesPagination(t *testing.T) {
	tests := []struct {
		name      string
		total     int
		wantPages []int
	}{
		{"none", 0, []int{1}},
		{"one short page", 3, []int{1}},
		{"exactly one full page needs a second request to know it is over", 100, []int{1, 2}},
		{"two pages", 137, []int{1, 2}},
		{"three pages", 250, []int{1, 2, 3}},
		{"capped at 20 pages", 2500, func() []int {
			var p []int
			for i := 1; i <= 20; i++ {
				p = append(p, i)
			}
			return p
		}()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRepos{t: t, total: tc.total}
			serveAPI(t, f)
			got, err := newTestClient(t).Repositories(context.Background(), 7)
			if err != nil {
				t.Fatal(err)
			}
			want := min(tc.total, 2000)
			if len(got) != want {
				t.Fatalf("got %d repositories, want %d", len(got), want)
			}
			if fmt.Sprint(f.pages) != fmt.Sprint(tc.wantPages) {
				t.Errorf("requested pages %v, want %v", f.pages, tc.wantPages)
			}
			if want > 0 {
				first := got[0]
				if first.Name != "r1" || first.FullName != "acme/r1" || first.Owner.Login != "acme" || first.Private {
					t.Errorf("first repo = %+v", first)
				}
				if last := got[len(got)-1]; last.Name != fmt.Sprintf("r%d", want) || last.Private != (want%2 == 0) {
					t.Errorf("last repo = %+v", last)
				}
			}
		})
	}
}

func TestRepositoriesFailingPage(t *testing.T) {
	var pages []string
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/7/access_tokens" {
			_, _ = w.Write([]byte(`{"token":"inst-tok"}`))
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		if page == "2" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		var b strings.Builder
		b.WriteString(`{"repositories":[`)
		for i := 0; i < 100; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"name":"r"}`)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	}))
	got, err := newTestClient(t).Repositories(context.Background(), 7)
	if statusCode(err) != http.StatusBadGateway {
		t.Fatalf("err = %v, want a 502", err)
	}
	if got != nil {
		t.Errorf("a failed listing returned %d partial repositories", len(got))
	}
	if fmt.Sprint(pages) != "[1 2]" {
		t.Errorf("requested pages %v, want to stop at the failing page 2", pages)
	}
}

func TestRepositoriesTokenFailure(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/7/access_tokens" {
			t.Errorf("listed repositories without a token: %s", r.URL)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, err := newTestClient(t).Repositories(context.Background(), 7); statusCode(err) != http.StatusUnauthorized {
		t.Errorf("err = %v, want a 401", err)
	}
}

func TestManifest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, baseURL string
	}{
		{"plain", "http://pitlane.lan:8080"},
		{"one trailing slash", "http://pitlane.lan:8080/"},
		{"several trailing slashes", "http://pitlane.lan:8080///"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := Manifest("my-pitlane", tc.baseURL)
			base := "http://pitlane.lan:8080"

			strs := map[string]string{
				"name":         "my-pitlane",
				"url":          "https://github.com/TuroYT/pitlane",
				"redirect_url": base + "/setup/callback",
				"setup_url":    base + "/setup",
			}
			for k, want := range strs {
				if got := m[k]; got != want {
					t.Errorf("%s = %v, want %q", k, got, want)
				}
			}
			if got, _ := m["callback_urls"].([]string); len(got) != 1 || got[0] != base+"/auth/callback" {
				t.Errorf("callback_urls = %v, want [%s/auth/callback]", m["callback_urls"], base)
			}
			hook, _ := m["hook_attributes"].(map[string]any)
			if hook["url"] != base+"/webhook" || hook["active"] != false {
				t.Errorf("hook_attributes = %v, want an inactive webhook at %s/webhook", hook, base)
			}
			if m["public"] != false {
				t.Errorf("public = %v, want a private App", m["public"])
			}
			if m["request_oauth_on_install"] != false {
				t.Errorf("request_oauth_on_install = %v, want false", m["request_oauth_on_install"])
			}
			if ev, _ := m["default_events"].([]string); ev == nil || len(ev) != 0 {
				t.Errorf("default_events = %#v, want an empty list (not null)", m["default_events"])
			}
			wantPerms := map[string]string{
				"administration":                   "write",
				"organization_self_hosted_runners": "write",
				"actions":                          "read",
				"metadata":                         "read",
			}
			perms, _ := m["default_permissions"].(map[string]string)
			if fmt.Sprint(perms) != fmt.Sprint(wantPerms) {
				t.Errorf("default_permissions = %v, want %v", perms, wantPerms)
			}
		})
	}
}

// GitHub receives the manifest as JSON in a form field: it must encode, with
// an empty event list as [] and not null.
func TestManifestJSON(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Manifest("x", "http://h"))
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if ev, ok := back["default_events"].([]any); !ok || len(ev) != 0 {
		t.Errorf("default_events = %#v, want []", back["default_events"])
	}
	if cb, ok := back["callback_urls"].([]any); !ok || len(cb) != 1 || cb[0] != "http://h/auth/callback" {
		t.Errorf("callback_urls = %#v", back["callback_urls"])
	}
}

func TestConvertManifest(t *testing.T) {
	var gotPath, gotAuth string
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireGitHubHeaders(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":99,"slug":"my-pitlane","name":"my pitlane","client_id":"Iv1.abc","client_secret":"s3cret",
			"pem":"-----BEGIN RSA PRIVATE KEY-----","webhook_secret":"wh","html_url":"https://github.com/apps/my-pitlane",
			"owner":{"login":"acme"}}`))
	}))
	app, err := ConvertManifest(context.Background(), "a/b c")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/app-manifests/a%2Fb%20c/conversions" {
		t.Errorf("path = %q, want the code escaped as one segment", gotPath)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, the temporary code is the only credential", gotAuth)
	}
	want := App{ID: 99, Slug: "my-pitlane", Name: "my pitlane", ClientID: "Iv1.abc", ClientSecret: "s3cret",
		PEM: "-----BEGIN RSA PRIVATE KEY-----", WebhookSecret: "wh", HTMLURL: "https://github.com/apps/my-pitlane"}
	want.Owner.Login = "acme"
	if *app != want {
		t.Errorf("app = %+v, want %+v", *app, want)
	}
}

func TestConvertManifestErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		code   int
	}{
		{"expired code", http.StatusNotFound, `{"message":"Not Found"}`, http.StatusNotFound},
		{"rejected", http.StatusUnprocessableEntity, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity},
		{"server error", http.StatusInternalServerError, ``, http.StatusInternalServerError},
		{"malformed JSON", http.StatusCreated, `{"id": `, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			_, err := ConvertManifest(context.Background(), "code")
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := statusCode(err); got != tc.code {
				t.Errorf("status = %d, want %d (err: %v)", got, tc.code, err)
			}
		})
	}
}
