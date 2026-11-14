package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"pitlane", "my-repo", "a.b_c", "TuroYT"} {
		if !validName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "x y", "a;rm", string(make([]byte, 101))} {
		if validName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestNormalizeUsers(t *testing.T) {
	if got := normalizeUsers(" @alice, bob\nAlice  carol,"); got != "alice,bob,carol" {
		t.Fatalf("got %q", got)
	}
}

func TestMedian(t *testing.T) {
	if m := median([]float64{5, 1, 3}); m != 3 {
		t.Fatal(m)
	}
	if m := median(nil); m != 0 {
		t.Fatal(m)
	}
}

func TestCSRFRequiresHeader(t *testing.T) {
	s := &Server{}
	h := s.csrf(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		method, path, header string
		want                 int
	}{
		{"POST", "/api/targets", "", http.StatusForbidden},
		{"DELETE", "/api/targets/1", "", http.StatusForbidden},
		{"POST", "/api/targets", "1", 204},
		{"GET", "/api/state", "", 204},
		{"GET", "/setup/callback", "", 204},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.header != "" {
			r.Header.Set("X-Pitlane", tc.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s header=%q: got %d want %d", tc.method, tc.path, tc.header, w.Code, tc.want)
		}
	}
}
