package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWebHandler(t *testing.T) {
	t.Parallel()
	s, _, _, _ := apiServer(t)
	s.web = fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html>index")},
		"assets/app.js":   {Data: []byte("export {}")},
		"favicon.svg":     {Data: []byte("<svg/>")},
		"secret/.env":     {Data: []byte("x")},
		"assets/dir/file": {Data: []byte("nested")},
	}
	h := s.Handler()
	for _, tc := range []struct {
		name, path  string
		code        int
		body, cache string
	}{
		{"index", "/", 200, "<!doctype html>index", "no-cache"},
		{"hashed asset", "/assets/app.js", 200, "export {}", "public, max-age=31536000, immutable"},
		{"nested asset", "/assets/dir/file", 200, "nested", "public, max-age=31536000, immutable"},
		{"other file", "/favicon.svg", 200, "<svg/>", "no-cache"},
		{"UI route", "/targets/12/edit", 200, "<!doctype html>index", "no-cache"},
		{"missing asset", "/assets/gone.js", 200, "<!doctype html>index", "no-cache"}, // not cached as if it were one
		{"unknown API route", "/api/nope", 404, `{"error":"not found"}`, ""},
		{"unknown API route, deep", "/api/a/b/c", 404, `{"error":"not found"}`, ""},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.RemoteAddr = "192.0.2.9:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code || strings.TrimSpace(w.Body.String()) != tc.body || w.Header().Get("Cache-Control") != tc.cache {
			t.Errorf("%s: %d %q cache %q; want %d %q cache %q", tc.name, w.Code, w.Body, w.Header().Get("Cache-Control"), tc.code, tc.body, tc.cache)
		}
	}
}

// The mux cleans a path with dot segments before the UI sees it.
func TestWebPathsAreCleaned(t *testing.T) {
	t.Parallel()
	s, _, _, _ := apiServer(t)
	s.web = fstest.MapFS{"index.html": {Data: []byte("index")}}
	for path, want := range map[string]string{"/../../etc/passwd": "/etc/passwd", "/ui/../api/nope": "/api/nope"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != want {
			t.Errorf("%s: %d %q, want a redirect to %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
}

func TestWebNotBuilt(t *testing.T) {
	t.Parallel()
	s, _, _, _ := apiServer(t)
	s.web = nil
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "UI not built") {
		t.Fatalf("%d %q", w.Code, w.Body)
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("no security headers on the not-built page")
	}
}

// Every state-changing API route refuses a request without the X-Pitlane
// header, even from an admin.
func TestCSRFOnEveryRoute(t *testing.T) {
	t.Parallel()
	s, _, _, _ := apiServer(t)
	s.SetUpdates(&upstream{status: http.StatusOK})
	h := s.Handler()
	for _, rt := range s.routes() {
		method, path, ok := strings.Cut(rt.pattern, " ")
		if !ok { // any method: the updates routes
			method, path = http.MethodPost, rt.pattern
		}
		path = strings.NewReplacer("{id}", "1", "{login}", "bob", "{name}", "pitlane-a").Replace(path)
		if strings.HasSuffix(path, "/") && path != "/" {
			path += "x"
		}
		// Reads need no header (and /api/events never ends).
		if !strings.HasPrefix(path, "/api/") || method == http.MethodGet || method == http.MethodHead {
			continue
		}
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.AddCookie(session("alice"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "X-Pitlane") {
			t.Errorf("%s %s without the header: %d %s", method, path, w.Code, w.Body)
		}
	}
	// Other values of the header do not count, and non-API paths need none.
	for _, v := range []string{"0", "true", " 1", ""} {
		r := httptest.NewRequest("DELETE", "/api/targets/1", nil)
		r.AddCookie(session("alice"))
		r.Header.Set("X-Pitlane", v)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("X-Pitlane=%q accepted: %d", v, w.Code)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/auth/callback", nil))
	if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "X-Pitlane") {
		t.Error("the OAuth callback needs the CSRF header")
	}
}

func TestRequestIDInErrorResponses(t *testing.T) {
	t.Parallel()
	s, _, dir, logs := serverAt(t)
	dropTable(t, dir, "jobs")
	h := s.Handler()
	call := func(id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/jobs", nil)
		if id != "" {
			r.Header.Set("X-Request-Id", id)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// A reverse proxy's request ID is kept, in the response and the log.
	w := call("proxy-id.42")
	if w.Header().Get("X-Request-Id") != "proxy-id.42" || !strings.Contains(w.Body.String(), `"requestId":"proxy-id.42"`) || !strings.Contains(logs.String(), "request_id=proxy-id.42") {
		t.Fatalf("%v %s", w.Header(), w.Body)
	}
	// One that could carry something else is replaced, never echoed.
	w = call("evil\"><script>")
	if got := w.Header().Get("X-Request-Id"); len(got) != 16 || strings.Contains(w.Body.String(), "script") {
		t.Fatalf("%q %s", got, w.Body)
	}
	// Two failures never share a generated ID.
	if a, b := call("").Header().Get("X-Request-Id"), call("").Header().Get("X-Request-Id"); a == b || a == "" {
		t.Fatalf("%q %q", a, b)
	}
}
