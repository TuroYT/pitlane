package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/store"
)

type fakeSettings struct {
	set     map[string]string
	failKey string
}

func (f *fakeSettings) Set(k, v string) error {
	if k == f.failKey {
		return errors.New("disk full")
	}
	f.set[k] = v
	return nil
}

func (f *fakeSettings) SetSecret(k, v string) error { return f.Set(k, v) }

func TestSaveAppSurfacesErrors(t *testing.T) {
	app := &gh.App{ID: 1, Slug: "pitlane", ClientID: "Iv1.x", PEM: "pem", ClientSecret: "s"}
	ok := &fakeSettings{set: map[string]string{}}
	if err := saveApp(ok, app); err != nil || ok.set["app_client_id"] != "Iv1.x" || ok.set["app_pem"] != "pem" {
		t.Fatalf("err=%v set=%v", err, ok.set)
	}
	for _, key := range []string{"app_pem", "app_client_secret", "app_slug"} {
		f := &fakeSettings{set: map[string]string{}, failKey: key}
		if err := saveApp(f, app); err == nil {
			t.Fatalf("failure on %s not reported", key)
		}
		if _, ok := f.set["app_client_id"]; ok {
			t.Fatalf("failure on %s left the App marked as configured", key)
		}
	}
}

// Run with -race: handlers read the setup token while the first sign-in
// clears it.
func TestSetupTokenConcurrentAccess(t *testing.T) {
	s := &Server{}
	s.setSetupToken("tok")
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: setupCookie, Value: "tok"})
	if !s.hasSetupCookie(r) {
		t.Fatal("valid setup cookie refused")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				s.hasSetupCookie(r)
			}
		}()
	}
	s.setSetupToken("")
	wg.Wait()
	if s.hasSetupCookie(r) {
		t.Fatal("cleared setup token still accepted")
	}
}

func newSetupServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(Config{DataDir: t.TempDir()}, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), fstest.MapFS{})
	s.setSetupToken("tok")
	return s, &http.Cookie{Name: setupCookie, Value: "tok"}
}

func TestSetupManifestValidatesOrg(t *testing.T) {
	s, cookie := newSetupServer(t)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/setup/manifest", strings.NewReader(body))
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		s.handleSetupManifest(w, r)
		return w
	}
	for _, org := range []string{"../../evil", "a/b", "x?y", "a b"} {
		if w := post(`{"baseUrl":"https://p.example","org":"` + org + `"}`); w.Code != http.StatusBadRequest {
			t.Errorf("org %q accepted: %d", org, w.Code)
		}
	}
	w := post(`{"baseUrl":"https://p.example","org":"acme-corp"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "https://github.com/organizations/acme-corp/settings/apps/new?state=") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestSetupBodiesAreBounded(t *testing.T) {
	s, cookie := newSetupServer(t)
	huge := `{"token":"` + strings.Repeat("a", 1<<20) + `"}`
	for path, h := range map[string]http.HandlerFunc{"/api/setup/unlock": s.handleSetupUnlock, "/api/setup/manifest": s.handleSetupManifest} {
		r := httptest.NewRequest("POST", path, strings.NewReader(huge))
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code == http.StatusOK {
			t.Errorf("%s accepted a 1 MB body", path)
		}
	}
}
