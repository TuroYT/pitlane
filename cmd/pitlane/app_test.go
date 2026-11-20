package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/server"
	"github.com/TuroYT/pitlane/internal/store"
	"github.com/TuroYT/pitlane/internal/update"
)

// recorder records the lifecycle calls in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type fakeUI struct {
	rec *recorder
	tok server.SetupToken
}

func (f fakeUI) EnsureSetupToken() server.SetupToken { f.rec.add("setup token"); return f.tok }
func (f fakeUI) Notify()                             { f.rec.add("notify") }

type fakeSlots struct {
	rec *recorder
	err error
}

func (f fakeSlots) Start(context.Context) error { f.rec.add("slots start"); return f.err }

// fakeScaler checks on Shutdown that the UI still answers: the scaler
// stops before the HTTP server.
type fakeScaler struct {
	rec *recorder
	err error
	ui  string // URL of the UI
}

func (f fakeScaler) Start(context.Context) error { f.rec.add("scaler start"); return f.err }

func (f fakeScaler) Shutdown() {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(f.ui)
	if err != nil {
		f.rec.add("scaler shutdown, UI down")
		return
	}
	_ = resp.Body.Close()
	f.rec.add("scaler shutdown, UI up")
}

// fakeUpdater runs then calls onRun (a restart request, or nothing).
type fakeUpdater struct {
	rec   *recorder
	onRun func()
}

func (f fakeUpdater) Run(context.Context) {
	f.rec.add("updater run")
	f.onRun()
}

// newTestApp is an app with fake components, its UI on a loopback port
// and a real actions cache.
func newTestApp(t *testing.T, rec *recorder, slotsErr, scalerErr error) (*app, *syncBuffer, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log, buf := testLogger()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		log:     log,
		ui:      fakeUI{rec: rec, tok: server.SetupToken{Value: "secret-token", File: "/data/setup-token", Saved: true}},
		slots:   fakeSlots{rec: rec, err: slotsErr},
		scaler:  fakeScaler{rec: rec, err: scalerErr, ui: "http://" + ln.Addr().String()},
		updates: fakeUpdater{rec: rec, onRun: cancel},
		http:    newHTTPServer("127.0.0.1:8080", http.NotFoundHandler()),
		ln:      ln,
		cache:   startActionsCache(log, t.TempDir(), "127.0.0.1:8091", loopbackListen(t)),
		stop:    cancel,
	}
	if a.cache == nil {
		t.Fatal("cache not started")
	}
	return a, buf, ctx
}

// assertClosed checks that serve closed both listeners.
func assertClosed(t *testing.T, a *app) {
	t.Helper()
	if !refused(a.ln.Addr()) {
		t.Error("the UI still listens")
	}
	select {
	case <-a.cache.done:
	default:
		t.Error("the actions cache still serves")
	}
}

func TestServeStartsThenShutsDownInOrder(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a, buf, ctx := newTestApp(t, rec, nil, nil)
	if err := a.serve(ctx); err != nil {
		t.Fatalf("serve: %v", err)
	}
	want := []string{"setup token", "slots start", "scaler start", "notify", "updater run", "scaler shutdown, UI up"}
	if got := rec.list(); !reflect.DeepEqual(got, want) {
		t.Errorf("calls %q\nwant  %q", got, want)
	}
	assertClosed(t, a)
	log := buf.String()
	for _, msg := range []string{
		`msg="pitlane listening" addr=127.0.0.1:8080 version=` + version,
		`file=/data/setup-token`,
		`msg="starting warm slots (first start pulls images and may take a minute)"`,
		`msg="shutting down (busy runners keep running and are adopted on restart)"`,
	} {
		if !strings.Contains(log, msg) {
			t.Errorf("log lacks %s:\n%s", msg, log)
		}
	}
	if strings.Contains(log, "secret-token") {
		t.Error("the saved setup token was logged")
	}
}

func TestServeRestartRequest(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a, _, ctx := newTestApp(t, rec, nil, nil)
	a.updates = fakeUpdater{rec: rec, onRun: a.requestRestart}
	if err := a.serve(ctx); !errors.Is(err, errRestart) {
		t.Fatalf("serve = %v, want errRestart", err)
	}
	if got := rec.list(); got[len(got)-1] != "scaler shutdown, UI up" {
		t.Errorf("calls %q", got)
	}
	assertClosed(t, a)
}

func TestServeStartFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name               string
		slotsErr, scaleErr error
		wantErr            string
		wantCalls          []string
	}{
		{"slots", boom, nil, "start slots: boom", []string{"setup token", "slots start"}},
		{"scaler", nil, boom, "start scaler: boom", []string{"setup token", "slots start", "scaler start"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{}
			a, _, ctx := newTestApp(t, rec, tc.slotsErr, tc.scaleErr)
			err := a.serve(ctx)
			if !errors.Is(err, boom) || err.Error() != tc.wantErr {
				t.Fatalf("serve = %v, want %q", err, tc.wantErr)
			}
			// Nothing started: the scaler is not shut down, nor the updater run.
			if got := rec.list(); !reflect.DeepEqual(got, tc.wantCalls) {
				t.Errorf("calls %q, want %q", got, tc.wantCalls)
			}
			assertClosed(t, a)
		})
	}
}

// A UI that stops serving stops Pitlane.
func TestServeHTTPErrorStops(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	a, buf, ctx := newTestApp(t, rec, nil, nil)
	ln := a.ln
	t.Cleanup(func() { _ = ln.Close() })
	a.ln = failingListener{ln}
	a.updates = fakeUpdater{rec: rec, onRun: func() {}} // only the HTTP error ends ctx
	if err := a.serve(ctx); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !strings.Contains(buf.String(), `msg="http server" error="accept: dead socket"`) {
		t.Errorf("log %q", buf.String())
	}
}

func TestAnyBusy(t *testing.T) {
	t.Parallel()
	if anyBusy(nil) || anyBusy([]pool.SlotState{{Status: "idle"}, {Status: "starting"}}) {
		t.Error("busy without a busy slot")
	}
	if !anyBusy([]pool.SlotState{{Status: "idle"}, {Status: "busy"}}) {
		t.Error("a busy slot is not busy")
	}
}

func TestLogSetupToken(t *testing.T) {
	t.Parallel()
	const secret = "0123456789abcdef01234567"
	for _, tc := range []struct {
		name         string
		tok          server.SetupToken
		level        slog.Level // the level the operator logs at
		wantToken    bool
		wantFile     bool
		wantNoOutput bool
	}{
		{"saved", server.SetupToken{Value: secret, File: "/data/setup-token", Saved: true}, slog.LevelInfo, false, true, false},
		{"not saved", server.SetupToken{Value: secret, File: "/data/setup-token"}, slog.LevelInfo, true, false, false},
		// The token is the only way in: it must survive a quiet log level.
		{"not saved, warn level", server.SetupToken{Value: secret, File: "/data/setup-token"}, slog.LevelWarn, true, false, false},
		{"setup complete", server.SetupToken{}, slog.LevelInfo, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logSetupToken(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level})), tc.tok)
			out := buf.String()
			if got := strings.Contains(out, secret); got != tc.wantToken {
				t.Errorf("token in the log = %v, want %v: %s", got, tc.wantToken, out)
			}
			if got := strings.Contains(out, "/data/setup-token"); got != tc.wantFile {
				t.Errorf("file in the log = %v, want %v: %s", got, tc.wantFile, out)
			}
			if tc.wantNoOutput && out != "" {
				t.Errorf("unexpected log: %s", out)
			}
		})
	}
}

// noDocker points the Docker client at a socket that does not exist, so
// nothing reaches a real daemon. Tests using it cannot run in parallel.
func noDocker(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "no-docker.sock"))
}

// runConfig is a valid configuration on a fresh data directory, the UI on
// a free loopback port.
func runConfig(t *testing.T) server.Config {
	return server.Config{Version: version, DataDir: t.TempDir(), Listen: "127.0.0.1:0"}
}

// runSystem is testSystem with the real listener for the UI (loopback port
// 0) and a free loopback port for the actions cache.
func runSystem(t *testing.T, env map[string]string) system {
	sys, _, _ := testSystem(t, env)
	cache := loopbackListen(t)
	sys.listen = func(network, addr string) (net.Listener, error) {
		if addr == "127.0.0.1:8091" {
			return cache(network, addr)
		}
		return net.Listen(network, addr)
	}
	return sys
}

// Run wires everything for real and only fails when the warm slots try to
// reach Docker.
func TestRunUntilDocker(t *testing.T) {
	noDocker(t)
	for _, skipAttestation := range []bool{false, true} {
		log, buf := testLogger()
		env := map[string]string{}
		if skipAttestation {
			env[update.SkipAttestationEnv] = "true"
		}
		cfg := runConfig(t)
		err := run(context.Background(), log, cfg, "127.0.0.1:8091", runSystem(t, env))
		if err == nil || !strings.HasPrefix(err.Error(), "start slots: ") {
			t.Fatalf("run = %v, want a start slots error", err)
		}
		out := buf.String()
		for _, msg := range []string{
			`msg="actions cache listening" component=cache addr=127.0.0.1:8091`,
			`msg="pitlane listening" addr=127.0.0.1:0`,
			`file=` + filepath.Join(cfg.DataDir, "setup-token"),
			`msg="starting warm slots`,
		} {
			if !strings.Contains(out, msg) {
				t.Errorf("log lacks %s:\n%s", msg, out)
			}
		}
		warned := strings.Contains(out, "self-update will NOT verify build attestations")
		if warned != skipAttestation {
			t.Errorf("attestation warning = %v with skip = %v", warned, skipAttestation)
		}
		for _, dir := range []string{"actions-cache", "logs"} {
			if _, err := os.Stat(filepath.Join(cfg.DataDir, dir)); err != nil {
				t.Error(err)
			}
		}
	}
}

func TestRunFailsEarly(t *testing.T) {
	noDocker(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt := t.TempDir()
	if err := os.WriteFile(filepath.Join(corrupt, "pitlane.db"), bytes.Repeat([]byte("not sqlite "), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	noPing := func(context.Context, *pool.Pool) error { return errors.New("connection refused") }
	for _, tc := range []struct {
		name    string
		dataDir string
		listen  string
		ping    func(context.Context, *pool.Pool) error
		wantErr string
	}{
		{"unwritable data directory", filepath.Join(file, "data"), "", nil, "open store: mkdir " + file},
		{"corrupt database", corrupt, "", nil, "open store: migrate: "},
		{"Docker unreachable", "", "", noPing, "cannot reach Docker (is the socket mounted / is Docker running?): connection refused"},
		{"bad listen address", "", "127.0.0.1:notaport", nil, `listen on "127.0.0.1:notaport": `},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := runConfig(t)
			if tc.dataDir != "" {
				cfg.DataDir = tc.dataDir
			}
			if tc.listen != "" {
				cfg.Listen = tc.listen
			}
			sys := runSystem(t, nil)
			var cache net.Listener
			sys.listen = func(network, addr string) (net.Listener, error) {
				if addr == "127.0.0.1:8091" {
					var err error
					cache, err = loopbackListen(t)(network, addr)
					return cache, err
				}
				return net.Listen(network, addr)
			}
			if tc.ping != nil {
				sys.ping = tc.ping
			}
			log, _ := testLogger()
			err := run(context.Background(), log, cfg, "127.0.0.1:8091", sys)
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("run = %v, want %q", err, tc.wantErr)
			}
			if cache != nil && !refused(cache.Addr()) {
				t.Error("the actions cache still listens")
			}
		})
	}
}

func TestRunBadDockerHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "not a docker host")
	log, _ := testLogger()
	err := run(context.Background(), log, runConfig(t), "off", runSystem(t, nil))
	if err == nil || !strings.HasPrefix(err.Error(), "docker: ") {
		t.Fatalf("run = %v, want a docker error", err)
	}
}

func TestDaemon(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		args        []string
		wantCode    int
		wantStderr  string
		wantDefault bool // the default logger was installed
	}{
		{"help", []string{"-h"}, 0, "Usage of pitlane:", false},
		{"usage error", []string{"--bogus"}, 2, "flag provided but not defined", false},
		{"bad log level", []string{"--log-level", "loud"}, 2, "pitlane: invalid --log-level", false},
		{"no auth off loopback", []string{"--insecure-no-auth", "--listen", ":8080"}, 1, "level=ERROR msg=fatal error=\"--insecure-no-auth needs a loopback", false},
		{"bad trusted proxy", []string{"--trusted-proxies", "nope"}, 1, "msg=fatal", false},
		{"run fails", []string{"--data", filepath.Join(file, "d"), "--cache-listen", "off", "--log-level", "warn"}, 1, "msg=fatal error=\"open store: ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sys, stdout, stderr := testSystem(t, nil)
			installed := false
			sys.setDefaultLogger = func(*slog.Logger) { installed = true }
			code := sys.main(context.Background(), append([]string{"pitlane"}, tc.args...))
			if code != tc.wantCode {
				t.Errorf("exit code %d, want %d: %s", code, tc.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.wantStderr)
			}
			if installed != tc.wantDefault {
				t.Errorf("default logger installed = %v, want %v", installed, tc.wantDefault)
			}
			if stdout.String() != "" {
				t.Errorf("stdout %q", stdout.String())
			}
		})
	}
}

func TestProgramName(t *testing.T) {
	t.Parallel()
	if programName(nil) != "pitlane" || programName([]string{"/usr/bin/pitlane"}) != "/usr/bin/pitlane" {
		t.Error("programName")
	}
}

func TestReexec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pitlane-1.2.0")
	if err := os.WriteFile(bin, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "pitlane")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	args := []string{"pitlane", "--listen", ":8080"}

	t.Run("exec", func(t *testing.T) {
		t.Parallel()
		sys, _, _ := testSystem(t, nil)
		sys.executable = func() (string, error) { return link, nil }
		var gotExe string
		var gotArgs, gotEnv []string
		sys.exec = func(exe string, argv, envv []string) error {
			gotExe, gotArgs, gotEnv = exe, argv, envv
			return errors.New("exec format error")
		}
		log, buf := testLogger()
		if code := sys.reexec(log, args); code != 1 {
			t.Errorf("exit code %d", code)
		}
		// The symlink is resolved: it may point to the new binary already.
		if gotExe != bin || !reflect.DeepEqual(gotArgs, args) || !reflect.DeepEqual(gotEnv, []string{"A=1"}) {
			t.Errorf("exec(%q, %q, %q)", gotExe, gotArgs, gotEnv)
		}
		for _, msg := range []string{
			`msg="restarting into the updated binary" path=` + bin,
			`msg="re-exec failed, exiting so the service manager restarts Pitlane" error="exec format error"`,
		} {
			if !strings.Contains(buf.String(), msg) {
				t.Errorf("log lacks %s: %s", msg, buf.String())
			}
		}
	})
	for name, exe := range map[string]func() (string, error){
		"no executable":    func() (string, error) { return "", errors.New("unknown") },
		"dangling symlink": func() (string, error) { return filepath.Join(dir, "missing"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sys, _, _ := testSystem(t, nil)
			sys.executable = exe
			sys.exec = func(string, []string, []string) error {
				t.Error("exec called")
				return nil
			}
			log, buf := testLogger()
			if code := sys.reexec(log, args); code != 1 {
				t.Errorf("exit code %d", code)
			}
			if !strings.Contains(buf.String(), "re-exec failed") {
				t.Errorf("log %q", buf.String())
			}
		})
	}
}

// Every field of the real system is set: a missing one would only show up
// as a nil function call in production.
func TestRealSystem(t *testing.T) {
	noDocker(t)
	sys := realSystem()
	v := reflect.ValueOf(sys)
	for i := range v.NumField() {
		if v.Field(i).IsNil() {
			t.Errorf("realSystem().%s is nil", v.Type().Field(i).Name)
		}
	}
	ctx, stop := sys.signals(context.Background())
	stop()
	<-ctx.Done()
	p, err := pool.New(pool.Config{StateFile: filepath.Join(t.TempDir(), "slots.json")}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.ping(context.Background(), p); err == nil {
		t.Error("ping reached a Docker daemon that does not exist")
	}
}

// The updater reads the pool and calls back into the app.
func TestNewUpdater(t *testing.T) {
	noDocker(t)
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p, err := pool.New(pool.Config{StateFile: filepath.Join(dataDir, "slots.json"), RunnerImage: "runner:test"}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var notified, restarted bool
	log, _ := testLogger()
	upd := newUpdater(log, func(string) string { return "" }, dataDir, st, p, func() { notified = true }, func() { restarted = true })
	if upd.Version != version || !upd.Attested || upd.Store == nil || upd.GitHub == nil || upd.Verify == nil {
		t.Errorf("updater %+v", upd)
	}
	if got := upd.RunnerImage(); got != "runner:test" {
		t.Errorf("RunnerImage() = %q", got)
	}
	if upd.Busy() {
		t.Error("busy without slots")
	}
	if err := upd.Images.Pull(context.Background(), "runner:test"); err == nil {
		t.Error("pulled without Docker")
	}
	upd.Notify()
	upd.Restart()
	if !notified || !restarted {
		t.Errorf("notified %v, restarted %v", notified, restarted)
	}
}
