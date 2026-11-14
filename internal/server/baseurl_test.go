package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/TuroYT/pitlane/internal/store"
)

func TestBaseURLTrustsHeadersOnlyFromProxies(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(Config{Listen: "10.0.0.5:8080", TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}},
		st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	req := func(remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.RemoteAddr = remote
		r.Host = "evil.example"
		r.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		s.handleStatus(w, r)
		return w
	}
	base := func(remote string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r.Host = "evil.example"
		r.Header.Set("X-Forwarded-Proto", "https")
		return s.baseURL(r)
	}
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.7:4000", "http://10.0.0.5:8080"}, // anyone: the listen address
		{"127.0.0.1:4000", "https://evil.example"}, // proxy on the same host
		{"[::1]:4000", "https://evil.example"},
		{"[::ffff:127.0.0.1]:4000", "https://evil.example"},
		{"172.20.0.3:4000", "https://evil.example"}, // configured proxy
		{"garbage", "http://10.0.0.5:8080"},
	} {
		if got := base(tc.remote); got != tc.want {
			t.Errorf("from %s: got %q want %q", tc.remote, got, tc.want)
		}
	}
	// The status endpoint does not report a guessed URL (the setup page then
	// proposes the browser's own address).
	var status struct {
		BaseURL string `json:"baseUrl"`
	}
	_ = json.Unmarshal(req("192.0.2.7:4000").Body.Bytes(), &status)
	if status.BaseURL != "" {
		t.Errorf("status reports %q to an untrusted client", status.BaseURL)
	}
	_ = st.Set("base_url", "https://pitlane.example")
	if got := base("192.0.2.7:4000"); got != "https://pitlane.example" {
		t.Errorf("stored base URL: %q", got)
	}
	s.cfg.BaseURL = "https://configured.example/"
	if got := base("127.0.0.1:4000"); got != "https://configured.example" {
		t.Errorf("configured base URL: %q", got)
	}
}

func TestListenURL(t *testing.T) {
	for listen, want := range map[string]string{
		"10.0.0.5:9000":  "http://10.0.0.5:9000",
		"[fd00::1]:8080": "http://[fd00::1]:8080",
		"pitlane:8080":   "http://pitlane:8080",
	} {
		s := &Server{cfg: Config{Listen: listen}}
		if got := s.listenURL(); got != want {
			t.Errorf("%s: got %q want %q", listen, got, want)
		}
	}
	s := &Server{cfg: Config{Listen: ":8080"}}
	if got := s.listenURL(); got == "http://:8080" {
		t.Errorf("every interface: %q, want the host name", got)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies("10.0.0.1, 172.16.0.0/12,fd00::/8 ::ffff:192.168.1.2")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.1/32", "172.16.0.0/12", "fd00::/8", "192.168.1.2/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, p := range got {
		if p.String() != want[i] {
			t.Errorf("%d: got %s want %s", i, p, want[i])
		}
	}
	if p, _ := ParseTrustedProxies(""); len(p) != 0 {
		t.Fatal("empty list")
	}
	if _, err := ParseTrustedProxies("10.0.0.1,proxy.lan"); err == nil {
		t.Fatal("host name accepted")
	}
}
