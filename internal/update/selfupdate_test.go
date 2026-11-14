package update

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeProbe() Probe {
	return Probe{
		Version: "0.2.0", GOOS: "linux", GOARCH: "amd64",
		Executable: func() (string, error) { return "/usr/local/bin/pitlane", nil },
		Exists:     func(string) bool { return false },
		Getenv:     func(string) string { return "" },
		Writable:   func(string) error { return nil },
	}
}

func TestDetect(t *testing.T) {
	env := func(k, v string) func(string) string {
		return func(key string) string {
			if key == k {
				return v
			}
			return ""
		}
	}
	cases := []struct {
		name string
		mut  func(*Probe)
		mode string
		can  bool
	}{
		{"plain binary", func(*Probe) {}, ModeBinary, true},
		{"systemd", func(p *Probe) { p.Getenv = env("INVOCATION_ID", "abc") }, ModeSystemd, true},
		{"docker", func(p *Probe) { p.Exists = func(f string) bool { return f == "/.dockerenv" } }, ModeContainer, false},
		{"podman", func(p *Probe) { p.Exists = func(f string) bool { return f == "/run/.containerenv" } }, ModeContainer, false},
		{"disabled", func(p *Probe) { p.Getenv = env("PITLANE_SELF_UPDATE", "false") }, ModeManaged, false},
		{"dev build", func(p *Probe) { p.Version = "dev" }, ModeDev, false},
		{"git describe", func(p *Probe) { p.Version = "v0.2.0-3-gabcdef0-dirty" }, ModeDev, false},
		{"darwin", func(p *Probe) { p.GOOS = "darwin" }, ModeUnsupport, false},
		{"riscv", func(p *Probe) { p.GOARCH = "riscv64" }, ModeUnsupport, false},
		{"no executable", func(p *Probe) { p.Executable = func() (string, error) { return "", errors.New("gone") } }, ModeUnsupport, false},
		{"read-only dir", func(p *Probe) {
			p.Getenv = env("INVOCATION_ID", "abc")
			p.Writable = func(string) error { return os.ErrPermission }
		}, ModeSystemd, false},
	}
	for _, c := range cases {
		p := fakeProbe()
		c.mut(&p)
		got := Detect(p)
		if got.Mode != c.mode || got.CanSelfUpdate != c.can {
			t.Errorf("%s: got %+v, want mode %s can %v", c.name, got, c.mode, c.can)
		}
		if !got.CanSelfUpdate && got.Reason == "" {
			t.Errorf("%s: no reason", c.name)
		}
	}
}

func TestDetectRealWritability(t *testing.T) {
	dir := t.TempDir()
	p := DefaultProbe("0.2.0")
	p.Exists = func(string) bool { return false }
	p.Executable = func() (string, error) { return filepath.Join(dir, "pitlane"), nil }
	if c := Detect(p); !c.CanSelfUpdate && c.Mode != ModeUnsupport {
		t.Errorf("writable temp dir: %+v", c)
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	p.Executable = func() (string, error) { return filepath.Join(ro, "pitlane"), nil }
	if c := Detect(p); c.CanSelfUpdate {
		t.Errorf("read-only dir: %+v", c)
	}
}

func TestManualCommand(t *testing.T) {
	if got := ManualCommand(ModeContainer, "v1.0.0"); !strings.Contains(got, "docker compose pull") {
		t.Error(got)
	}
	if got := ManualCommand(ModeSystemd, "v1.0.0"); !strings.Contains(got, "PITLANE_VERSION=1.0.0") || !strings.Contains(got, "install.sh") {
		t.Error(got)
	}
}

// releaseServer serves a fake release over HTTPS: files by name.
func releaseServer(t *testing.T, files map[string][]byte) (*httptest.Server, *GitHub, Release) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	g := &GitHub{HTTP: srv.Client(), Hosts: []string{srv.Listener.Addr().String()}}
	rel := Release{Tag: "v0.3.0", HTMLURL: "https://github.com/x/y/releases/v0.3.0"}
	for name := range files {
		rel.Assets = append(rel.Assets, Asset{Name: name, URL: srv.URL + "/dl/" + name})
	}
	return srv, g, rel
}

func installer(t *testing.T, g *GitHub) (*Installer, string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "pitlane")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil { //nolint:gosec // test executable
		t.Fatal(err)
	}
	return &Installer{
		GitHub: g, Executable: exe, Arch: "amd64",
		Smoke: func(ctx context.Context, path string) (string, error) { return "pitlane 0.3.0\n", nil },
	}, exe
}

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".pitlane-update-*"))
	return m
}

func TestInstallGood(t *testing.T) {
	bin := []byte("new binary")
	sums := fmt.Sprintf("%x  pitlane_linux_amd64\n%x  pitlane_linux_arm64\n", sha256.Sum256(bin), sha256.Sum256([]byte("arm")))
	_, g, rel := releaseServer(t, map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(sums)})
	in, exe := installer(t, g)
	verified := false
	in.Verify = func(ctx context.Context, r Release, path string) error {
		verified = true
		return nil
	}
	var phases []string
	want, _ := ParseVersion("0.3.0")
	if err := in.Install(context.Background(), rel, want, func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" { //nolint:gosec // test file
		t.Errorf("binary = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "old binary" { //nolint:gosec // test file
		t.Errorf("backup = %q", b)
	}
	if fi, _ := os.Stat(exe); fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("not executable: %v", fi.Mode())
	}
	if !verified {
		t.Error("verifier hook not called")
	}
	if len(phases) == 0 || phases[len(phases)-1] != "installing" {
		t.Errorf("phases %v", phases)
	}
	if l := leftovers(t, filepath.Dir(exe)); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

func TestInstallFailuresKeepTheBinary(t *testing.T) {
	bin := []byte("new binary")
	good := fmt.Sprintf("%x  pitlane_linux_amd64\n", sha256.Sum256(bin))
	cases := map[string]struct {
		files map[string][]byte
		mut   func(*Installer)
		want  string
	}{
		"bad checksum": {map[string][]byte{"pitlane_linux_amd64": []byte("tampered"), "checksums.txt": []byte(good)}, nil, "mismatch"},
		"missing entry": {map[string][]byte{"pitlane_linux_amd64": bin,
			"checksums.txt": []byte(fmt.Sprintf("%x  pitlane_linux_arm64\n", sha256.Sum256(bin)))}, nil, "no entry"},
		"no checksums file": {map[string][]byte{"pitlane_linux_amd64": bin}, nil, "checksums.txt"},
		"no asset":          {map[string][]byte{"checksums.txt": []byte(good)}, nil, "no pitlane_linux_amd64"},
		"too big":           {map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(good)}, func(in *Installer) { in.MaxBinary = 3 }, "limit"},
		"verifier refuses": {map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(good)}, func(in *Installer) {
			in.Verify = func(context.Context, Release, string) error { return errors.New("no attestation") }
		}, "no attestation"},
		"wrong version": {map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(good)}, func(in *Installer) {
			in.Smoke = func(context.Context, string) (string, error) { return "pitlane 0.2.9", nil }
		}, "expected 0.3.0"},
		"does not run": {map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(good)}, func(in *Installer) {
			in.Smoke = func(context.Context, string) (string, error) { return "exec format error", errors.New("exit 1") }
		}, "does not run"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, g, rel := releaseServer(t, c.files)
			in, exe := installer(t, g)
			if c.mut != nil {
				c.mut(in)
			}
			want, _ := ParseVersion("0.3.0")
			err := in.Install(context.Background(), rel, want, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if b, _ := os.ReadFile(exe); string(b) != "old binary" { //nolint:gosec // test file
				t.Errorf("binary replaced: %q", b)
			}
			if _, err := os.Stat(exe + ".prev"); err == nil {
				t.Error("backup created on failure")
			}
			if l := leftovers(t, filepath.Dir(exe)); len(l) != 0 {
				t.Errorf("temporary files left: %v", l)
			}
		})
	}
}

func TestGitHubRefusesUnsafeURLs(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("x")) }))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/file", http.StatusFound)
	}))
	defer srv.Close()
	g := &GitHub{HTTP: srv.Client(), Hosts: []string{srv.Listener.Addr().String()}}
	var sink strings.Builder
	if _, err := g.Download(context.Background(), srv.URL+"/a", 100, &sink); err == nil || !strings.Contains(err.Error(), "unexpected host") {
		t.Errorf("redirect to another host: %v", err)
	}
	if _, err := g.Download(context.Background(), "http://"+srv.Listener.Addr().String()+"/a", 100, &sink); err == nil || !strings.Contains(err.Error(), "non-HTTPS") {
		t.Errorf("plain HTTP: %v", err)
	}
	if _, err := (&GitHub{}).Download(context.Background(), "https://example.com/a", 100, &sink); err == nil {
		t.Error("default hosts accept example.com")
	}
}

func TestReleasesNotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "private") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]Release{{Tag: "v1.0.0"}, {Tag: "v1.1.0", Draft: true}})
	}))
	defer srv.Close()
	g := &GitHub{HTTP: srv.Client(), APIBase: srv.URL, Hosts: []string{srv.Listener.Addr().String()}}
	if _, err := g.Releases(context.Background(), "me/private", 5); !errors.Is(err, ErrNoRelease) {
		t.Errorf("404: %v", err)
	}
	rels, err := g.Releases(context.Background(), "me/public", 5)
	if err != nil || len(rels) != 1 {
		t.Errorf("drafts must be dropped: %v %v", rels, err)
	}
}
