package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// tlsGitHub runs h behind a TLS server and returns a client that trusts it
// and may only talk to it.
func tlsGitHub(t *testing.T, h http.HandlerFunc) (*GitHub, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &GitHub{HTTP: srv.Client(), APIBase: srv.URL, Hosts: []string{srv.Listener.Addr().String()}}, srv
}

func TestReleasesFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
		is      error
	}{
		{"not found", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "me/app", ErrNoRelease},
		{"rate limited by 403", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }, "rate limited or forbidden (HTTP 403)", nil},
		{"rate limited by 429", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }, "rate limited or forbidden (HTTP 429)", nil},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }, "HTTP 502", nil},
		{"invalid JSON", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"tag_name":`)) }, "GitHub releases of me/app", nil},
		{"not a list", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"message":"hi"}`)) }, "GitHub releases of me/app", nil},
		{"truncated JSON", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "500")
			_, _ = w.Write([]byte(`[{"tag_name":"v1.0.0"},`))
		}, "GitHub releases of me/app", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, _ := tlsGitHub(t, tc.handler)
			rels, err := g.Releases(context.Background(), "me/app", 5)
			if err == nil {
				t.Fatalf("no error, got %v", rels)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error %v is not %v", err, tc.is)
			}
		})
	}
}

func TestReleasesRequest(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got *http.Request
	g, _ := tlsGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r
		mu.Unlock()
		_ = json.NewEncoder(w).Encode([]Release{
			{Tag: "v1.1.0", Draft: true}, {Tag: "v1.0.0", Assets: []Asset{{Name: "a", URL: "u", Size: 3}}}, {Tag: "v0.9.0", Prerelease: true},
		})
	})
	rels, err := g.Releases(context.Background(), "me/app", 7)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.URL.Path != "/repos/me/app/releases" || got.URL.Query().Get("per_page") != "7" {
		t.Errorf("requested %s", got.URL)
	}
	if got.Header.Get("User-Agent") != "pitlane-updater" || !strings.Contains(got.Header.Get("Accept"), "github") {
		t.Errorf("headers: %v", got.Header)
	}
	if got.Header.Get("Authorization") != "" {
		t.Error("the update client must be unauthenticated")
	}
	if len(rels) != 2 || rels[0].Tag != "v1.0.0" || rels[1].Tag != "v0.9.0" || !rels[1].Prerelease {
		t.Fatalf("releases (drafts dropped, order kept): %+v", rels)
	}
	if a, ok := rels[0].Asset("a"); !ok || a.Size != 3 || a.URL != "u" {
		t.Errorf("asset: %+v %v", a, ok)
	}
	if _, ok := rels[0].Asset("missing"); ok {
		t.Error("unknown asset found")
	}
}

func TestReleasesTransportErrors(t *testing.T) {
	t.Parallel()
	g, srv := tlsGitHub(t, func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode([]Release{}) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Releases(ctx, "me/app", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
	// The default client verifies certificates: the test server's is unknown to it.
	plain := &GitHub{APIBase: srv.URL, Hosts: g.Hosts}
	if _, err := plain.Releases(context.Background(), "me/app", 1); err == nil {
		t.Error("untrusted certificate accepted")
	}
	// APIBase and the host list are checked like any URL.
	if _, err := (&GitHub{APIBase: "http://api.github.com"}).Releases(context.Background(), "me/app", 1); err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Errorf("plain HTTP API: %v", err)
	}
	if _, err := (&GitHub{APIBase: "https://evil.example"}).Releases(context.Background(), "me/app", 1); err == nil || !strings.Contains(err.Error(), "unexpected host") {
		t.Errorf("foreign API host: %v", err)
	}
	if _, err := (&GitHub{APIBase: "https://bad host"}).Releases(context.Background(), "me/app", 1); err == nil {
		t.Error("unparsable API URL accepted")
	}
}

func TestDownload(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("x", 100)
	tests := []struct {
		name    string
		handler http.HandlerFunc
		limit   int64
		want    int64
		wantErr string
	}{
		{"complete", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }, 1000, 100, ""},
		{"exactly the limit", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }, 100, 100, ""},
		{"not found", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, 1000, 0, "HTTP 404"},
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, 1000, 0, "HTTP 500"},
		{"announced size over the limit", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }, 99, 0, "over the 99 byte limit"},
		{"unannounced size over the limit", func(w http.ResponseWriter, r *http.Request) {
			w.(http.Flusher).Flush() // chunked: no Content-Length
			_, _ = w.Write([]byte(body))
		}, 99, 100, "over the 99 byte limit"},
		{"connection cut mid-download", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(body[:10]))
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}, 1000, 10, "unexpected EOF"},
		{"empty body is not an error", func(w http.ResponseWriter, r *http.Request) {}, 1000, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, srv := tlsGitHub(t, tc.handler)
			var sink strings.Builder
			n, err := g.Download(context.Background(), srv.URL+"/file", tc.limit, &sink)
			if tc.wantErr == "" {
				if err != nil || n != tc.want || int64(sink.Len()) != tc.want {
					t.Fatalf("n=%d written=%d err=%v, want %d", n, sink.Len(), err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.limit < 100 && int64(sink.Len()) > tc.limit+1 {
				t.Errorf("wrote %d bytes, far over the %d limit", sink.Len(), tc.limit)
			}
		})
	}
}

func TestDownloadRedirects(t *testing.T) {
	t.Parallel()
	var srvURL string
	g, srv := tlsGitHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			http.Redirect(w, r, "/b", http.StatusFound)
		case "/b":
			_, _ = w.Write([]byte("final"))
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/plain":
			http.Redirect(w, r, "http://"+r.Host+"/b", http.StatusFound)
		}
	})
	srvURL = srv.URL
	for _, tc := range []struct{ path, want, wantErr string }{
		{"/a", "final", ""},
		{"/loop", "", "too many redirects"},
		{"/plain", "", "non-HTTPS"},
	} {
		var sink strings.Builder
		_, err := g.Download(context.Background(), srvURL+tc.path, 100, &sink)
		switch {
		case tc.wantErr == "" && (err != nil || sink.String() != tc.want):
			t.Errorf("%s: %q %v", tc.path, sink.String(), err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: %v, want %q", tc.path, err, tc.wantErr)
		}
	}
}

func TestDownloadBadURL(t *testing.T) {
	t.Parallel()
	g := &GitHub{}
	var sink strings.Builder
	for _, u := range []string{"://nope", "https://bad host/x", "/relative", "ftp://github.com/x", "https://github.com.evil.example/x"} {
		if _, err := g.Download(context.Background(), u, 10, &sink); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	// Every default host is allowed, over HTTPS only.
	for _, h := range DefaultHosts {
		if err := g.checkURL(mustParse(t, "https://"+h+"/x")); err != nil {
			t.Errorf("%s: %v", h, err)
		}
		if err := g.checkURL(mustParse(t, "http://"+h+"/x")); err == nil {
			t.Errorf("%s over HTTP accepted", h)
		}
	}
}

func TestDownloadCancelled(t *testing.T) {
	t.Parallel()
	g, srv := tlsGitHub(t, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "x") })
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	var sink strings.Builder
	if _, err := g.Download(ctx, srv.URL, 10, &sink); err == nil {
		t.Fatal("download with an expired context succeeded")
	}
}

func TestLatestStable(t *testing.T) {
	t.Parallel()
	rels := []Release{
		{Tag: "v2.0.0", Draft: true},
		{Tag: "v1.9.0-rc.1"},
		{Tag: "v1.8.0", Prerelease: true},
		{Tag: "nightly"},
		{Tag: "v1.2.0"},
		{Tag: "v1.10.0"},
		{Tag: "v1.3.0"},
	}
	r, v, ok := LatestStable(rels)
	if !ok || r.Tag != "v1.10.0" || v.Minor != 10 {
		t.Fatalf("got %v %v %v (1.10 sorts after 1.3)", r.Tag, v, ok)
	}
	if _, _, ok := LatestStable(nil); ok {
		t.Error("stable release in an empty list")
	}
	if _, _, ok := LatestStable([]Release{{Tag: "v1.0.0-rc.1"}, {Tag: "v1.0.0", Prerelease: true}, {Tag: "junk"}}); ok {
		t.Error("only pre-releases and junk, yet a stable one was found")
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
