package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// flow is an Updater wired to a fake GitHub (releases and downloads over
// TLS), a temporary "installed binary" and recorders for restarts.
type flow struct {
	t       *testing.T
	u       *Updater
	exe     string
	srv     *httptest.Server
	store   *memStore
	images  *fakeImages
	notify  chan struct{}
	restart chan struct{}

	mu       sync.Mutex
	status   int               // releases status override (0: 200)
	runnerSt int               // same for the runner releases
	releases []Release         // served for Repo
	files    map[string][]byte // served under /dl/
	smoke    string            // output of the new binary's "version"
}

func sums(files map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range files {
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(data), name)
	}
	return []byte(b.String())
}

// newFlow serves release v0.2.1 (current version 0.2.0) with a valid binary.
func newFlow(t *testing.T) *flow {
	t.Helper()
	f := &flow{t: t, notify: make(chan struct{}, 1), restart: make(chan struct{}, 4), store: &memStore{}, images: &fakeImages{id: "sha256:old"}}
	bin := []byte("new binary")
	f.files = map[string][]byte{"pitlane_linux_amd64": bin}
	f.files["checksums.txt"] = sums(f.files)
	f.smoke = "pitlane 0.2.1\n"
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/dl/"):
			b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		case strings.Contains(r.URL.Path, "/repos/"+Repo+"/"):
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			_ = json.NewEncoder(w).Encode(f.releases)
		case strings.Contains(r.URL.Path, "/repos/"+RunnerRepo+"/"):
			if f.runnerSt != 0 {
				w.WriteHeader(f.runnerSt)
				return
			}
			_ = json.NewEncoder(w).Encode([]Release{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.releases = []Release{f.release("v0.2.1")}

	f.exe = filepath.Join(t.TempDir(), "pitlane")
	if err := os.WriteFile(f.exe, []byte("old binary"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	p := fakeProbe()
	p.Executable = func() (string, error) { return f.exe, nil }
	p.Getenv = func(k string) string {
		if k == "INVOCATION_ID" {
			return "x"
		}
		return ""
	}
	f.u = &Updater{
		Version: "0.2.0", Store: f.store, Images: f.images, Probe: p,
		GitHub:      &GitHub{HTTP: f.srv.Client(), APIBase: f.srv.URL, Hosts: []string{f.srv.Listener.Addr().String()}},
		RunnerImage: func() string { return "ghcr.io/x/runner:latest" },
		Now:         func() time.Time { return at(3) },
		Restart:     func() { f.restart <- struct{}{} },
		Notify: func() {
			select {
			case f.notify <- struct{}{}:
			default:
			}
		},
		smoke: func(context.Context, string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.smoke, nil
		},
		sleep: func(time.Duration) {},
	}
	return f
}

// release is a release whose assets are the flow's files.
func (f *flow) release(tag string) Release {
	r := Release{Tag: tag, HTMLURL: "https://github.com/TuroYT/pitlane/releases/tag/" + tag}
	for name := range f.files {
		r.Assets = append(r.Assets, Asset{Name: name, URL: f.srv.URL + "/dl/" + name})
	}
	return r
}

func (f *flow) setFile(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = data
}

func (f *flow) setStatus(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = code
}

// waitFor waits until cond holds, re-checking after each Notify.
func (f *flow) waitFor(what string, cond func(State) bool) State {
	f.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		if s := f.u.State(); cond(s) {
			return s
		}
		select {
		case <-f.notify:
		case <-timeout:
			f.t.Fatalf("timed out waiting for %s; state: %+v", what, f.u.State().Pitlane)
		}
	}
}

func (f *flow) binary() string {
	f.t.Helper()
	b, err := os.ReadFile(f.exe)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *flow) restarted() bool {
	select {
	case <-f.restart:
		return true
	default:
		return false
	}
}

func (f *flow) wantUntouched() {
	f.t.Helper()
	if got := f.binary(); got != "old binary" {
		f.t.Errorf("binary replaced: %q", got)
	}
	if _, err := os.Stat(f.exe + ".prev"); err == nil {
		f.t.Error("backup created on failure")
	}
	if l, _ := filepath.Glob(filepath.Join(filepath.Dir(f.exe), ".pitlane-update-*")); len(l) != 0 {
		f.t.Errorf("temporary files left: %v", l)
	}
	if f.restarted() {
		f.t.Error("restarted although the update failed")
	}
}

func TestApplyHappyPath(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	f.u.Check(context.Background())
	if err := f.u.Apply(false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.restart:
	case <-time.After(10 * time.Second):
		t.Fatalf("no restart; state %+v", f.u.State().Pitlane)
	}
	if got := f.binary(); got != "new binary" {
		t.Errorf("binary = %q", got)
	}
	if b, _ := os.ReadFile(f.exe + ".prev"); string(b) != "old binary" { //nolint:gosec // test file
		t.Errorf("backup = %q", b)
	}
	s := f.u.State().Pitlane
	if s.Phase != "restarting" || s.ApplyError != "" || !strings.HasPrefix(s.LastUpdate, "0.2.0 → 0.2.1 on ") {
		t.Errorf("state %+v", s)
	}
}

// Auto updates go through the same path, and wait while jobs are running.
func TestAutoUpdateWaitsForBusyRunners(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	_ = f.store.Set(keyAutoPull, "false")
	var busy sync.Mutex
	isBusy := true
	f.u.Busy = func() bool { busy.Lock(); defer busy.Unlock(); return isBusy }
	if err := f.u.SaveSettings(Settings{AutoUpdate: true, Window: Window{3, 5}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.u.tick(ctx)
	if s := f.u.State().Pitlane; s.Phase != "" || s.AutoStatus != "a job is running" || s.AutoCandidate != "v0.2.1" {
		t.Fatalf("while busy: %+v", s)
	}
	f.wantUntouched()

	busy.Lock()
	isBusy = false
	busy.Unlock()
	f.u.tick(ctx)
	select {
	case <-f.restart:
	case <-time.After(10 * time.Second):
		t.Fatal("no restart once the runners are idle")
	}
	if got := f.binary(); got != "new binary" {
		t.Errorf("binary = %q", got)
	}
}

func TestAutoUpdateOutsideWindow(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	_ = f.store.Set(keyAutoPull, "false")
	f.u.Now = func() time.Time { return at(12) }
	if err := f.u.SaveSettings(Settings{AutoUpdate: true, Window: Window{3, 5}}); err != nil {
		t.Fatal(err)
	}
	f.u.tick(context.Background())
	if s := f.u.State().Pitlane; s.Phase != "" || !strings.Contains(s.AutoStatus, "outside the update window") {
		t.Fatalf("state %+v", s)
	}
	f.wantUntouched()
}

func TestApplyFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(f *flow)
		want  string
	}{
		"bad checksum": {func(f *flow) { f.setFile("pitlane_linux_amd64", []byte("tampered")) }, "mismatch"},
		"checksums not published": {func(f *flow) {
			f.mu.Lock()
			delete(f.files, "checksums.txt")
			f.mu.Unlock()
		}, "checksums.txt"},
		"binary asset not found": {func(f *flow) {
			f.mu.Lock()
			delete(f.files, "pitlane_linux_amd64") // listed in the release, 404 on download
			f.mu.Unlock()
		}, "HTTP 404"},
		"wrong version inside the binary": {func(f *flow) { f.mu.Lock(); f.smoke = "pitlane 0.2.0\n"; f.mu.Unlock() }, "expected 0.2.1"},
		"verifier refuses": {func(f *flow) {
			f.u.Verify = func(context.Context, Release, string) error { return errors.New("no attestation") }
		}, "no attestation"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFlow(t)
			f.u.Check(context.Background())
			tc.setup(f)
			// Assets are listed from the release fetched before the setup.
			if err := f.u.Apply(false); err != nil {
				t.Fatal(err)
			}
			s := f.waitFor("the failure", func(s State) bool { return s.Pitlane.Phase == "" && s.Pitlane.ApplyError != "" })
			if !strings.Contains(s.Pitlane.ApplyError, tc.want) {
				t.Errorf("error %q does not mention %q", s.Pitlane.ApplyError, tc.want)
			}
			if s.Pitlane.LastUpdate != "" {
				t.Errorf("failed update recorded: %q", s.Pitlane.LastUpdate)
			}
			f.wantUntouched()
			// A new attempt is possible after a failure and clears the error.
			// Apply clears the error before it returns, so the state is either
			// still running or already failed again, never the first failure.
			if err := f.u.Apply(false); err != nil {
				t.Fatalf("retry: %v", err)
			}
			s = f.waitFor("the second failure", func(s State) bool { return s.Pitlane.Phase == "" && s.Pitlane.ApplyError != "" })
			if !strings.Contains(s.Pitlane.ApplyError, tc.want) {
				t.Errorf("retry error %q does not mention %q", s.Pitlane.ApplyError, tc.want)
			}
		})
	}
}

// An automatic attempt that failed is not repeated before the next day, a
// manual one is not blocked.
func TestAutoUpdateBacksOffAfterFailure(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	_ = f.store.Set(keyAutoPull, "false")
	f.setFile("pitlane_linux_amd64", []byte("tampered"))
	if err := f.u.SaveSettings(Settings{AutoUpdate: true, Window: Window{3, 5}}); err != nil {
		t.Fatal(err)
	}
	var clock sync.Mutex
	now := at(3)
	f.u.Now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	ctx := context.Background()
	f.u.tick(ctx)
	f.waitFor("the failed attempt", func(s State) bool { return s.Pitlane.ApplyError != "" && s.Pitlane.Phase == "" })

	f.u.tick(ctx)
	if s := f.u.State().Pitlane; s.Phase != "" || !strings.Contains(s.AutoStatus, "retrying tomorrow") {
		t.Fatalf("second attempt not held back: %+v", s)
	}
	clock.Lock()
	now = now.Add(25 * time.Hour)
	clock.Unlock()
	f.u.tick(ctx)
	f.waitFor("the retry", func(s State) bool {
		return s.Pitlane.ApplyError != "" && s.Pitlane.Phase == "" && !strings.Contains(s.Pitlane.AutoStatus, "tomorrow")
	})
	f.wantUntouched()
}

func TestApplyRefusals(t *testing.T) {
	t.Parallel()
	t.Run("no release known", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		if err := f.u.Apply(false); err == nil || !strings.Contains(err.Error(), "check for updates first") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("already up to date", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		f.u.Version = "0.2.1"
		f.u.Check(context.Background())
		if err := f.u.Apply(false); err == nil || !strings.Contains(err.Error(), "up to date") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("only pre-releases newer", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		f.mu.Lock()
		f.releases = []Release{{Tag: "v0.3.0-rc.1", Prerelease: true}}
		f.mu.Unlock()
		f.u.Check(context.Background())
		if err := f.u.Apply(false); err == nil {
			t.Fatal("a pre-release was installed")
		}
	})
	t.Run("automatic update skips a new minor in 0.x", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		f.mu.Lock()
		f.releases = []Release{f.release("v0.3.0")}
		f.smoke = "pitlane 0.3.0\n"
		f.mu.Unlock()
		f.u.Check(context.Background())
		if err := f.u.Apply(true); err == nil {
			t.Fatal("automatic update installed a new minor version")
		}
		if err := f.u.Apply(false); err != nil { // manually it is allowed
			t.Fatalf("manual update: %v", err)
		}
		select { // let the install finish before the temporary directory goes
		case <-f.restart:
		case <-time.After(10 * time.Second):
			t.Fatal("no restart")
		}
	})
	t.Run("an update is already running", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		f.u.Check(context.Background())
		block := make(chan struct{})
		f.u.smoke = func(context.Context, string) (string, error) { <-block; return "pitlane 0.2.1", nil }
		if err := f.u.Apply(false); err != nil {
			t.Fatal(err)
		}
		if err := f.u.Apply(false); err == nil || !strings.Contains(err.Error(), "already running") {
			t.Errorf("second Apply: %v", err)
		}
		close(block)
		select {
		case <-f.restart:
		case <-time.After(10 * time.Second):
			t.Fatal("no restart")
		}
	})
	t.Run("self-update disabled", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		f.u.Probe.Getenv = func(k string) string {
			if k == "PITLANE_SELF_UPDATE" {
				return "false"
			}
			return ""
		}
		f.u.Check(context.Background())
		err := f.u.Apply(false)
		if err == nil || !strings.Contains(err.Error(), "PITLANE_SELF_UPDATE") || !strings.Contains(err.Error(), "install.sh") {
			t.Fatalf("err = %v", err)
		}
		f.wantUntouched()
	})
}

// A GitHub outage or rate limit keeps what was known and says why.
func TestCheckErrorsKeepPreviousReleases(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	ctx := context.Background()
	f.u.Check(ctx)
	if s := f.u.State().Pitlane; s.Latest != "v0.2.1" || s.CheckError != "" || s.CheckedAt == 0 {
		t.Fatalf("first check: %+v", s)
	}
	for _, tc := range []struct {
		code int
		want string
	}{{http.StatusTooManyRequests, "rate limited"}, {http.StatusForbidden, "rate limited"}, {http.StatusInternalServerError, "HTTP 500"}, {http.StatusNotFound, "private"}} {
		f.setStatus(tc.code)
		f.u.Check(ctx)
		s := f.u.State().Pitlane
		if !strings.Contains(s.CheckError, tc.want) || s.Latest != "v0.2.1" || !s.UpdateAvailable {
			t.Errorf("HTTP %d: %+v", tc.code, s)
		}
	}
	f.setStatus(0)
	f.u.Check(ctx)
	if s := f.u.State().Pitlane; s.CheckError != "" {
		t.Errorf("error not cleared: %q", s.CheckError)
	}
}

func TestStateReportsAttestationAndManualInstall(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	f.u.Check(context.Background())
	if f.u.State().Pitlane.Attestation {
		t.Error("attestation reported without a verifier")
	}
	f.u.Verify = func(context.Context, Release, string) error { return nil }
	if f.u.State().Pitlane.Attestation {
		t.Error("attestation reported although the verifier is not an attestation check")
	}
	f.u.Attested = true
	if !f.u.State().Pitlane.Attestation {
		t.Error("attestation not reported")
	}
	// A development build never claims an update, whatever GitHub lists.
	f = newFlow(t)
	f.u.Version, f.u.Probe.Version = "dev", "dev"
	f.u.Check(context.Background())
	s := f.u.State().Pitlane
	if s.UpdateAvailable || s.AutoCandidate != "" || s.ManualCommand == "" || s.Capability.Mode != ModeDev {
		t.Errorf("dev build: %+v", s)
	}
}

// ---------------------------------------------------------------- Run

func TestRunStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { f.u.Run(ctx); close(done) }() // default 20 s start delay: returns without waiting for it
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run ignored the cancelled context")
	}
	if !f.u.State().Pitlane.Capability.CanSelfUpdate || f.u.State().Pitlane.CheckedAt != 0 {
		t.Errorf("Run checked before its start delay: %+v", f.u.State().Pitlane)
	}
}

func TestRunChecksPeriodically(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	_ = f.store.Set(keyAutoPull, "false")
	f.u.startDelay, f.u.every = time.Nanosecond, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.u.Run(ctx); close(done) }()
	f.waitFor("the first check", func(s State) bool { return s.Pitlane.Latest == "v0.2.1" && s.Runner.ImageID != "" })
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// ---------------------------------------------------------------- HTTP

func serve(t *testing.T, h http.Handler, method, path, body string) (int, string, http.Header) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b), rec.Result().Header
}

func decodeState(t *testing.T, body string) State {
	t.Helper()
	var s State
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return s
}

func TestHandlerRoutes(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	h := f.u.Handler()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/api/updates", http.StatusMethodNotAllowed},
		{"DELETE", "/api/updates", http.StatusMethodNotAllowed},
		{"GET", "/api/updates/check", http.StatusMethodNotAllowed},
		{"GET", "/api/updates/apply", http.StatusMethodNotAllowed},
		{"PUT", "/api/updates/apply", http.StatusMethodNotAllowed},
		{"GET", "/api/updates/pull-runner", http.StatusMethodNotAllowed},
		{"GET", "/api/updates/settings", http.StatusMethodNotAllowed},
		{"POST", "/api/updates/settings", http.StatusMethodNotAllowed},
		{"GET", "/api/updates/unknown", http.StatusNotFound},
	} {
		if code, _, _ := serve(t, h, tc.method, tc.path, ""); code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
	f.wantUntouched()
	if f.images.pulls != 0 {
		t.Error("a refused request started a pull")
	}
}

func TestHandlerStateAndCheck(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	h := f.u.Handler()
	code, body, hdr := serve(t, h, "GET", "/api/updates", "")
	if code != http.StatusOK || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("GET: %d %q", code, hdr.Get("Content-Type"))
	}
	if s := decodeState(t, body); s.Pitlane.Current != "0.2.0" || s.Pitlane.Latest != "" {
		t.Errorf("before the check: %+v", s.Pitlane)
	}
	code, body, _ = serve(t, h, "POST", "/api/updates/check", "")
	if s := decodeState(t, body); code != http.StatusOK || s.Pitlane.Latest != "v0.2.1" || !s.Pitlane.UpdateAvailable {
		t.Errorf("check: %d %+v", code, s.Pitlane)
	}
	f.setStatus(http.StatusTooManyRequests)
	code, body, _ = serve(t, h, "POST", "/api/updates/check", "")
	if s := decodeState(t, body); code != http.StatusOK || !strings.Contains(s.Pitlane.CheckError, "rate limited") {
		t.Errorf("rate limited check: %d %+v", code, s.Pitlane)
	}
}

func TestHandlerSettings(t *testing.T) {
	t.Parallel()
	const valid = `{"autoUpdate":true,"window":{"start":22,"end":4},"autoPull":false}`
	pad := func(n int) string { return strings.Repeat(" ", n-len(valid)) + valid } // n bytes in all
	tests := []struct {
		name string
		body string
		want int
		set  *Settings // settings after the request (nil: unchanged)
	}{
		{"valid", valid, http.StatusOK, &Settings{AutoUpdate: true, Window: Window{22, 4}}},
		{"exactly 4096 bytes", pad(4096), http.StatusOK, &Settings{AutoUpdate: true, Window: Window{22, 4}}},
		{"4097 bytes", pad(4097), http.StatusBadRequest, nil},
		{"far over the limit", pad(1 << 20), http.StatusBadRequest, nil},
		{"invalid JSON", `{"autoUpdate":`, http.StatusBadRequest, nil},
		{"empty body", ``, http.StatusBadRequest, nil},
		{"wrong type", `{"autoUpdate":"yes"}`, http.StatusBadRequest, nil},
		{"not an object", `[1,2]`, http.StatusBadRequest, nil},
		{"hour out of range", `{"window":{"start":0,"end":24}}`, http.StatusBadRequest, nil},
		{"negative hour", `{"window":{"start":-1,"end":4}}`, http.StatusBadRequest, nil},
		{"missing fields use zero values", `{}`, http.StatusOK, &Settings{Window: Window{0, 0}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFlow(t)
			before := f.u.LoadSettings()
			code, body, _ := serve(t, f.u.Handler(), "PUT", "/api/updates/settings", tc.body)
			if code != tc.want {
				t.Fatalf("status %d, want %d (%s)", code, tc.want, body)
			}
			after := f.u.LoadSettings()
			want := before
			if tc.set != nil {
				want = *tc.set
			}
			if after != want {
				t.Errorf("settings %+v, want %+v", after, want)
			}
			if code == http.StatusBadRequest {
				var e map[string]string
				if json.Unmarshal([]byte(body), &e) != nil || e["error"] == "" {
					t.Errorf("error body %q", body)
				}
			} else if s := decodeState(t, body); s.Settings != after {
				t.Errorf("response settings %+v, stored %+v", s.Settings, after)
			}
		})
	}
}

// A settings change the store cannot save is the server's failure (500,
// logged, the cause kept out of the response), not a bad request.
func TestHandlerSettingsStoreFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	f := newFlow(t)
	f.u.Store = &failingStore{failKey: keyAutoPull}
	f.u.Log = newBufLogger(&buf)
	code, body, _ := serve(t, f.u.Handler(), "PUT", "/api/updates/settings", `{"autoUpdate":true,"window":{"start":22,"end":4}}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 (%s)", code, body)
	}
	var e map[string]string
	if json.Unmarshal([]byte(body), &e) != nil || e["error"] == "" || strings.Contains(body, "disk full") {
		t.Errorf("error body %q: want a generic message, not the cause", body)
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "disk full") {
		t.Errorf("failure not logged: %s", buf.String())
	}
}

// An invalid window stays a 400 and does not touch the store or the log.
func TestHandlerSettingsInvalidWindowIsABadRequest(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	f := newFlow(t)
	f.u.Log = newBufLogger(&buf)
	code, body, _ := serve(t, f.u.Handler(), "PUT", "/api/updates/settings", `{"window":{"start":0,"end":24}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "between 0 and 23") {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("a client mistake was logged as an error: %s", buf.String())
	}
}

func TestHandlerApply(t *testing.T) {
	t.Parallel()
	t.Run("without a release", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		code, body, _ := serve(t, f.u.Handler(), "POST", "/api/updates/apply", "")
		if code != http.StatusConflict || !strings.Contains(body, "check for updates first") {
			t.Fatalf("%d %s", code, body)
		}
		f.wantUntouched()
	})
	t.Run("in a container", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		f.u.Probe.Exists = func(p string) bool { return p == "/.dockerenv" }
		f.u.Check(context.Background())
		code, body, _ := serve(t, f.u.Handler(), "POST", "/api/updates/apply", "")
		if code != http.StatusConflict || !strings.Contains(body, "docker compose pull") {
			t.Fatalf("%d %s", code, body)
		}
		f.wantUntouched()
	})
	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		f := newFlow(t)
		h := f.u.Handler()
		f.u.Check(context.Background())
		code, body, _ := serve(t, h, "POST", "/api/updates/apply", "")
		if code != http.StatusAccepted || decodeState(t, body).Pitlane.Phase == "" {
			t.Fatalf("%d %s", code, body)
		}
		select {
		case <-f.restart:
		case <-time.After(10 * time.Second):
			t.Fatal("no restart")
		}
		if f.binary() != "new binary" {
			t.Error("binary not replaced")
		}
		// The next request is refused: the version is installed, the process is about to restart.
		if code, _, _ := serve(t, h, "POST", "/api/updates/apply", ""); code != http.StatusConflict {
			t.Errorf("apply while restarting: %d", code)
		}
	})
}

// blockingImages blocks Pull until released.
type blockingImages struct {
	fakeImages
	started chan struct{}
	release chan struct{}
}

func (b *blockingImages) Pull(ctx context.Context, ref string) error {
	b.started <- struct{}{}
	<-b.release
	return b.fakeImages.Pull(ctx, ref)
}

func TestHandlerPullRunner(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	img := &blockingImages{fakeImages: fakeImages{id: "sha256:old"}, started: make(chan struct{}, 1), release: make(chan struct{})}
	f.u.Images = img
	h := f.u.Handler()

	code, body, _ := serve(t, h, "POST", "/api/updates/pull-runner", "")
	if code != http.StatusAccepted {
		t.Fatalf("first request: %d %s", code, body)
	}
	<-img.started
	f.waitFor("the pull to be reported", func(s State) bool { return s.Runner.Pulling })
	if code, body, _ := serve(t, h, "POST", "/api/updates/pull-runner", ""); code != http.StatusConflict || !strings.Contains(body, "already running") {
		t.Errorf("second request while pulling: %d %s", code, body)
	}
	close(img.release)
	s := f.waitFor("the pull to end", func(s State) bool {
		return !s.Runner.Pulling && s.Runner.PulledAt != 0 && s.Runner.ImageID == "sha256:new" && s.Runner.Version != ""
	})
	if s.Runner.PullError != "" || s.Runner.Version != "2.340.0" {
		t.Errorf("runner state %+v", s.Runner)
	}
}

func TestHandlerPullRunnerFailure(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	f.images.pullErr = errors.New("manifest unknown")
	code, _, _ := serve(t, f.u.Handler(), "POST", "/api/updates/pull-runner", "")
	if code != http.StatusAccepted { // the pull runs in the background
		t.Fatalf("status %d", code)
	}
	s := f.waitFor("the failed pull", func(s State) bool { return s.Runner.PullError != "" && !s.Runner.Pulling })
	if !strings.Contains(s.Runner.PullError, "manifest unknown") || s.Runner.PulledAt != 0 {
		t.Errorf("runner state %+v", s.Runner)
	}
}

// ---------------------------------------------------------------- pieces

type failingStore struct {
	memStore
	failKey string
}

func (s *failingStore) Set(k, v string) error {
	if k == s.failKey {
		return errors.New("disk full")
	}
	return s.memStore.Set(k, v)
}

func TestSaveSettingsStoreError(t *testing.T) {
	t.Parallel()
	for _, key := range []string{keyAuto, keyAutoPull, keyWindowStart, keyWindowEnd} {
		u := &Updater{Store: &failingStore{failKey: key}}
		if err := u.SaveSettings(Settings{Window: Window{3, 5}}); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestLoadSettingsDefaultsAndGarbage(t *testing.T) {
	t.Parallel()
	u := &Updater{Probe: fakeProbe(), Store: &memStore{}}
	if got := u.LoadSettings(); got != (Settings{AutoPull: true, Window: Window{3, 5}}) {
		t.Errorf("defaults: %+v", got)
	}
	st := &memStore{m: map[string]string{keyAuto: "true", keyAutoPull: "nope", keyWindowStart: "x", keyWindowEnd: "7"}}
	u = &Updater{Store: st}
	if got := u.LoadSettings(); got != (Settings{AutoUpdate: true, Window: Window{0, 7}}) {
		t.Errorf("garbage: %+v", got)
	}
}

func TestInspectImage(t *testing.T) {
	t.Parallel()
	t.Run("no docker", func(t *testing.T) {
		t.Parallel()
		u := &Updater{Probe: fakeProbe(), Store: &memStore{}}
		u.InspectImage(context.Background())
		if err := u.PullRunner(context.Background()); err == nil || !strings.Contains(err.Error(), "docker") {
			t.Errorf("PullRunner without images: %v", err)
		}
	})
	t.Run("inspect error is reported and cleared", func(t *testing.T) {
		t.Parallel()
		img := &scriptedImages{inspectErr: errors.New("no such image")}
		u := &Updater{Probe: fakeProbe(), Store: &memStore{}, Images: img, RunnerImage: func() string { return "r:latest" }}
		u.InspectImage(context.Background())
		if s := u.State().Runner; !strings.Contains(s.VersionErr, "inspect r:latest: no such image") || s.ImageID != "" {
			t.Fatalf("%+v", s)
		}
		img.inspectErr = nil
		img.info = ImageInfo{ID: "sha256:1"}
		img.version = "2.1.0"
		u.InspectImage(context.Background())
		if s := u.State().Runner; s.VersionErr != "" || s.Version != "2.1.0" || s.ImageID != "sha256:1" {
			t.Fatalf("%+v", s)
		}
	})
	t.Run("version read once per image", func(t *testing.T) {
		t.Parallel()
		img := &scriptedImages{info: ImageInfo{ID: "sha256:1"}, version: "2.1.0"}
		u := &Updater{Probe: fakeProbe(), Store: &memStore{}, Images: img}
		for range 3 {
			u.InspectImage(context.Background())
		}
		if img.versionCalls != 1 {
			t.Errorf("RunnerVersion called %d times for one image", img.versionCalls)
		}
		img.info = ImageInfo{ID: "sha256:2"}
		img.version = "2.2.0"
		u.InspectImage(context.Background())
		if img.versionCalls != 2 || u.State().Runner.Version != "2.2.0" {
			t.Errorf("new image: %d calls, %+v", img.versionCalls, u.State().Runner)
		}
	})
	t.Run("version unreadable", func(t *testing.T) {
		t.Parallel()
		img := &scriptedImages{info: ImageInfo{ID: "sha256:1"}, versionErr: errors.New("not a runner image")}
		u := &Updater{Probe: fakeProbe(), Store: &memStore{}, Images: img}
		u.InspectImage(context.Background())
		if s := u.State().Runner; s.VersionErr != "not a runner image" || s.Version != "" || s.Status != RunnerUnknown {
			t.Errorf("%+v", s)
		}
		// Retried on the next inspection.
		u.InspectImage(context.Background())
		if img.versionCalls != 2 {
			t.Errorf("%d calls", img.versionCalls)
		}
	})
}

type scriptedImages struct {
	info         ImageInfo
	inspectErr   error
	version      string
	versionErr   error
	versionCalls int
}

func (s *scriptedImages) Pull(context.Context, string) error { return nil }
func (s *scriptedImages) Inspect(context.Context, string) (ImageInfo, error) {
	return s.info, s.inspectErr
}
func (s *scriptedImages) RunnerVersion(context.Context, string) (string, error) {
	s.versionCalls++
	return s.version, s.versionErr
}

func TestPullRunnerAlreadyRunning(t *testing.T) {
	t.Parallel()
	u := &Updater{Probe: fakeProbe(), Store: &memStore{}, Images: &fakeImages{}, pulling: true}
	if err := u.PullRunner(context.Background()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v", err)
	}
}

func TestPullRunnerUnchangedImage(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	img := &scriptedImages{info: ImageInfo{ID: "sha256:same"}, version: "2.0.0"}
	u := &Updater{Probe: fakeProbe(), Store: &memStore{}, Images: img, Now: func() time.Time { return at(3) }, Log: newBufLogger(&buf)}
	if err := u.PullRunner(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "already up to date") || strings.Contains(buf.String(), "runner image updated") {
		t.Errorf("log: %s", buf.String())
	}
	if s := u.State().Runner; s.PulledAt != at(3).Unix() || s.Pulling {
		t.Errorf("%+v", s)
	}
}

func TestDefaultClockLoggerAndNotify(t *testing.T) {
	t.Parallel()
	u := &Updater{}
	if d := time.Since(u.now()); d < 0 || d > time.Minute {
		t.Errorf("default clock off by %v", d)
	}
	if u.log() == nil {
		t.Error("no default logger")
	}
	u.notify() // nil callback: nothing happens
	called := 0
	u.Notify = func() { called++ }
	u.notify()
	if called != 1 {
		t.Errorf("notify called %d times", called)
	}
	if u.runnerImage() != "" {
		t.Error("runner image without a provider")
	}
}

// A pull is attempted at most once an hour even when it keeps failing, and
// only when auto-pull is on.
func TestTickAutoPull(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	f.images.pullErr = errors.New("registry down")
	var clock sync.Mutex
	now := at(3)
	f.u.Now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	advance := func(d time.Duration) { clock.Lock(); now = now.Add(d); clock.Unlock() }
	ctx := context.Background()

	_ = f.store.Set(keyAutoPull, "false")
	f.u.tick(ctx)
	if f.images.pulls != 0 {
		t.Fatalf("auto-pull off, yet %d pulls", f.images.pulls)
	}
	_ = f.store.Set(keyAutoPull, "true")
	f.u.tick(ctx)
	f.u.tick(ctx)
	if f.images.pulls != 1 {
		t.Fatalf("pulls: %d, want 1 (retry is hourly)", f.images.pulls)
	}
	advance(59 * time.Minute)
	f.u.tick(ctx)
	if f.images.pulls != 1 {
		t.Fatalf("retried after 59 minutes: %d", f.images.pulls)
	}
	advance(2 * time.Minute)
	f.u.tick(ctx)
	if f.images.pulls != 2 {
		t.Fatalf("no retry after an hour: %d", f.images.pulls)
	}
	// Releases are fetched once per CheckEvery.
	f.setStatus(http.StatusInternalServerError)
	first := f.u.State().Pitlane.CheckedAt
	advance(CheckEvery - 2*time.Hour) // 4 h 01 since the check
	f.u.tick(ctx)
	if f.u.State().Pitlane.CheckedAt != first || f.u.State().Pitlane.CheckError != "" {
		t.Error("checked again before CheckEvery")
	}
	advance(3 * time.Hour)
	f.u.tick(ctx)
	if f.u.State().Pitlane.CheckError == "" {
		t.Error("no check after CheckEvery")
	}
}

// The runner releases and the Pitlane releases fail independently.
func TestCheckRunnerReleasesFailure(t *testing.T) {
	t.Parallel()
	f := newFlow(t)
	f.mu.Lock()
	f.runnerSt = http.StatusBadGateway
	f.mu.Unlock()
	var buf bytes.Buffer
	f.u.Log = newBufLogger(&buf)
	f.u.Check(context.Background())
	s := f.u.State()
	if s.Pitlane.CheckError != "" || s.Pitlane.Latest != "v0.2.1" {
		t.Errorf("Pitlane state affected by the runner repository: %+v", s.Pitlane)
	}
	if !strings.Contains(s.Runner.CheckError, "HTTP 502") || s.Runner.Status != RunnerUnknown {
		t.Errorf("runner state %+v", s.Runner)
	}
	if !strings.Contains(buf.String(), "update check: runner releases") {
		t.Errorf("failure not logged: %s", buf.String())
	}
}
