package server

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestSetSecurityHeaders(t *testing.T) {
	for _, https := range []bool{false, true} {
		h := http.Header{}
		setSecurityHeaders(h, https)
		for k, want := range map[string]string{
			"X-Frame-Options":         "DENY",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "same-origin",
			"Permissions-Policy":      permissionsPolicy,
			"Content-Security-Policy": contentSecurityPolicy,
		} {
			if got := h.Get(k); got != want {
				t.Errorf("https=%v %s = %q, want %q", https, k, got, want)
			}
		}
		if got, want := h.Get("Strict-Transport-Security"), map[bool]string{false: "", true: hstsPolicy}[https]; got != want {
			t.Errorf("https=%v Strict-Transport-Security = %q, want %q", https, got, want)
		}
	}
}

func TestContentSecurityPolicy(t *testing.T) {
	directives := map[string]string{}
	for _, d := range strings.Split(contentSecurityPolicy, "; ") {
		name, value, _ := strings.Cut(d, " ")
		directives[name] = value
	}
	for name, want := range map[string]string{
		"default-src":     "'none'",
		"script-src":      "'self'",
		"style-src":       "'self'",
		"connect-src":     "'self'",
		"frame-ancestors": "'none'",
		"base-uri":        "'none'",
		// The setup posts the GitHub App manifest to github.com.
		"form-action": "'self' https://github.com",
	} {
		if got := directives[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, unsafe := range []string{"'unsafe-inline'", "'unsafe-eval'", "*", "data:"} {
		if strings.Contains(contentSecurityPolicy, unsafe) {
			t.Errorf("policy allows %s: %s", unsafe, contentSecurityPolicy)
		}
	}
}

func TestServedOverHTTPS(t *testing.T) {
	s := &Server{cfg: Config{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")}}}
	for _, tc := range []struct {
		name, remote, proto string
		tls                 bool
		want                bool
	}{
		{"plain HTTP", "192.0.2.7:4000", "", false, false},
		{"TLS ends here", "192.0.2.7:4000", "", true, true},
		{"loopback proxy says https", "127.0.0.1:4000", "https", false, true},
		{"configured proxy says https", "10.1.2.3:4000", "https", false, true},
		{"proxy says http", "127.0.0.1:4000", "http", false, false},
		{"proxy says nothing", "127.0.0.1:4000", "", false, false},
		{"stranger claims https", "192.0.2.7:4000", "https", false, false},
		{"proxy of another network claims https", "10.2.0.1:4000", "https", false, false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := s.servedOverHTTPS(r); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The headers are on every kind of response: API, static files, the SPA
// fallback, the event stream and the CSRF rejection. HSTS follows the scheme.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	web := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>Pitlane</title>")},
		"assets/app.js": {Data: []byte("export {}")},
	}
	s := New(Config{DataDir: t.TempDir(), InsecureNoAuth: true}, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), web)
	h := s.Handler()

	for _, tc := range []struct {
		name, method, path string
		wantCode           int
	}{
		{"API", "GET", "/api/status", 200},
		{"health", "GET", "/healthz", 200},
		{"index", "GET", "/", 200},
		{"asset", "GET", "/assets/app.js", 200},
		{"SPA route", "GET", "/targets", 200},
		{"unknown API route", "GET", "/api/nope", 404},
		{"CSRF rejection", "POST", "/api/targets", 403},
	} {
		for _, https := range []bool{false, true} {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.RemoteAddr = "127.0.0.1:4000"
			if https {
				r.Header.Set("X-Forwarded-Proto", "https")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Errorf("%s: status %d, want %d", tc.name, w.Code, tc.wantCode)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Errorf("%s: Content-Security-Policy = %q", tc.name, got)
			}
			if w.Header().Get("X-Frame-Options") != "DENY" || w.Header().Get("Permissions-Policy") == "" {
				t.Errorf("%s: missing X-Frame-Options or Permissions-Policy: %v", tc.name, w.Header())
			}
			if got := w.Header().Get("Strict-Transport-Security") != ""; got != https {
				t.Errorf("%s: HSTS = %v with https=%v", tc.name, got, https)
			}
		}
	}
}

// The middleware must not wrap the ResponseWriter: the event stream needs its
// http.Flusher.
func TestSecurityHeadersKeepStreaming(t *testing.T) {
	s := &Server{}
	h := s.csrf(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("the response writer lost http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
		fl.Flush()
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/events", nil))
	if !w.Flushed || w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("flushed=%v headers=%v", w.Flushed, w.Header())
	}
}
