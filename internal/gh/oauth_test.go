package gh

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// serveOAuth starts a fake GitHub answering both the OAuth endpoints and the
// API, and points the client at it until the test ends.
func serveOAuth(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := serveAPI(t, h)
	old := oauthBase
	oauthBase = srv.URL
	t.Cleanup(func() { oauthBase = old })
	return srv
}

func TestAuthorizeURL(t *testing.T) {
	t.Parallel()
	got := AuthorizeURL("Iv1.abc", "http://pitlane.lan:8080/auth/callback?next=/a b&x=1", "st&te=1")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "github.com" || u.Path != "/login/oauth/authorize" {
		t.Errorf("URL = %s, want https://github.com/login/oauth/authorize", got)
	}
	want := map[string]string{
		"client_id":    "Iv1.abc",
		"redirect_uri": "http://pitlane.lan:8080/auth/callback?next=/a b&x=1",
		"state":        "st&te=1",
	}
	q := u.Query()
	if len(q) != len(want) {
		t.Errorf("query = %v, want only %v", q, want)
	}
	for k, v := range want {
		// Values with & and spaces must survive a round trip through the query.
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
}

func TestExchangeCode(t *testing.T) {
	serveOAuth(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			if r.Method != http.MethodPost {
				t.Errorf("token request method = %s, want POST", r.Method)
			}
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q", ct)
			}
			if a := r.Header.Get("Accept"); a != "application/json" {
				t.Errorf("Accept = %q: GitHub answers with a query string unless asked for JSON", a)
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			for k, want := range map[string]string{
				"client_id":     "cid",
				"client_secret": "secret",
				"code":          "the-code",
				"redirect_uri":  "http://h/auth/callback?a=1&b=2",
			} {
				if got := r.PostForm.Get(k); got != want {
					t.Errorf("form %s = %q, want %q", k, got, want)
				}
			}
			_, _ = w.Write([]byte(`{"access_token":"ghu_user","token_type":"bearer","scope":""}`))
		case "/user":
			if got := r.Header.Get("Authorization"); got != "Bearer ghu_user" {
				t.Errorf("Authorization = %q, want the user token", got)
			}
			requireGitHubHeaders(t, r)
			_, _ = w.Write([]byte(`{"login":"alice","id":1}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	login, tok, err := ExchangeCode(context.Background(), "cid", "secret", "the-code", "http://h/auth/callback?a=1&b=2")
	if err != nil {
		t.Fatal(err)
	}
	if login != "alice" || tok != "ghu_user" {
		t.Errorf("got login %q token %q, want alice ghu_user", login, tok)
	}
}

func TestExchangeCodeFailures(t *testing.T) {
	tests := []struct {
		name string
		// token and user are the replies of the two endpoints.
		tokenStatus int
		tokenBody   string
		userStatus  int
		userBody    string
		wantInErr   string
		wantStatus  int // StatusError code, 0 when none
		userCalled  bool
	}{
		{
			name: "error payload with a 200", tokenStatus: 200,
			tokenBody: `{"error":"bad_verification_code","error_description":"The code passed is incorrect or expired."}`,
			wantInErr: "The code passed is incorrect or expired.",
		},
		{
			name: "error code without description", tokenStatus: 200,
			tokenBody: `{"error":"access_denied"}`,
			wantInErr: "access_denied",
		},
		{
			name: "no token and no error", tokenStatus: 200, tokenBody: `{}`,
			wantInErr: "no access token",
		},
		{
			name: "token endpoint down", tokenStatus: 502, tokenBody: `<html>Bad Gateway</html>`,
			wantInErr: "invalid character",
		},
		{
			name: "malformed JSON", tokenStatus: 200, tokenBody: `{"access_token": `,
			wantInErr: "unexpected EOF",
		},
		{
			name: "user lookup refused", tokenStatus: 200, tokenBody: `{"access_token":"ghu_user"}`,
			userStatus: 401, userBody: `{"message":"Bad credentials"}`,
			wantInErr: "401", wantStatus: 401, userCalled: true,
		},
		{
			name: "user lookup malformed", tokenStatus: 200, tokenBody: `{"access_token":"ghu_user"}`,
			userStatus: 200, userBody: `[`,
			wantInErr: "EOF", userCalled: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			userCalled := false
			serveOAuth(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/user" {
					userCalled = true
					w.WriteHeader(tc.userStatus)
					_, _ = w.Write([]byte(tc.userBody))
					return
				}
				w.WriteHeader(tc.tokenStatus)
				_, _ = w.Write([]byte(tc.tokenBody))
			}))
			login, tok, err := ExchangeCode(context.Background(), "cid", "secret", "code", "http://h/cb")
			if err == nil {
				t.Fatal("expected an error")
			}
			if login != "" || tok != "" {
				t.Errorf("a failed exchange returned login %q token %q", login, tok)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not contain %q", err, tc.wantInErr)
			}
			if got := statusCode(err); got != tc.wantStatus {
				t.Errorf("status = %d, want %d", got, tc.wantStatus)
			}
			if userCalled != tc.userCalled {
				t.Errorf("user endpoint called = %v, want %v", userCalled, tc.userCalled)
			}
		})
	}
}

func TestExchangeCodeContext(t *testing.T) {
	t.Run("canceled during the token request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		serveOAuth(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The server notices a hung-up client only once the body is read.
			_ = r.ParseForm()
			cancel()
			<-r.Context().Done()
		}))
		_, _, err := ExchangeCode(ctx, "cid", "secret", "code", "http://h/cb")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("canceled during the user request", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		serveOAuth(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/user" {
				cancel()
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"ghu_user"}`))
		}))
		_, _, err := ExchangeCode(ctx, "cid", "secret", "code", "http://h/cb")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("server gone", func(t *testing.T) {
		srv := serveOAuth(t, http.NotFoundHandler())
		srv.Close()
		if _, _, err := ExchangeCode(context.Background(), "cid", "secret", "code", "http://h/cb"); err == nil {
			t.Error("expected a connection error")
		}
	})
}

func TestStatusError(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("  API rate limit exceeded\n"))
	}))
	_, err := RepoPrivate(context.Background(), "tok", "acme", "app")
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *StatusError", err)
	}
	if se.Code != http.StatusForbidden {
		t.Errorf("Code = %d, want 403", se.Code)
	}
	want := "github GET " + apiBase + "/repos/acme/app: 403 API rate limit exceeded"
	if se.Error() != want {
		t.Errorf("Error() = %q, want %q", se.Error(), want)
	}
	if statusCode(err) != 403 {
		t.Errorf("statusCode = %d", statusCode(err))
	}
	if statusCode(errors.New("boom")) != 0 || statusCode(nil) != 0 {
		t.Error("statusCode of a non-API error must be 0")
	}
}

// The body of an error answer is quoted in the error: a huge one is cut.
func TestStatusErrorBodyIsCut(t *testing.T) {
	serveAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 10000)))
	}))
	_, err := RepoPrivate(context.Background(), "tok", "acme", "app")
	if err == nil {
		t.Fatal("expected an error")
	}
	if n := len(err.Error()); n > 2048+len(apiBase)+100 {
		t.Errorf("error is %d bytes long, the body must be cut at 2 KiB", n)
	}
}
