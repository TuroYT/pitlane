package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseFiles(bin []byte) map[string][]byte {
	return map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(fmt.Sprintf("%x  pitlane_linux_amd64\n", sha256.Sum256(bin)))}
}

func TestInstallMoreFailures(t *testing.T) {
	t.Parallel()
	bin := []byte("new binary")
	want, _ := ParseVersion("0.3.0")
	tests := []struct {
		name    string
		files   map[string][]byte
		mut     func(t *testing.T, in *Installer)
		wantErr string
	}{
		{"checksums file too big", map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte(strings.Repeat("#", 64<<10+1))}, nil, "limit"},
		{"checksums file garbage", map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": []byte("not a checksum file\n")}, nil, "checksums line 1"},
		{"checksums file empty", map[string][]byte{"pitlane_linux_amd64": bin, "checksums.txt": nil}, nil, "no entry"},
		{"duplicate conflicting entries", map[string][]byte{"pitlane_linux_amd64": bin,
			"checksums.txt": []byte(fmt.Sprintf("%x  pitlane_linux_amd64\n%x  pitlane_linux_amd64\n", sha256.Sum256(bin), sha256.Sum256([]byte("evil"))))}, nil, "conflicting"},
		{"unexpected version output", releaseFiles(bin), func(t *testing.T, in *Installer) {
			in.Smoke = func(context.Context, string) (string, error) { return "hello world", nil }
		}, "unexpected version"},
		{"empty version output", releaseFiles(bin), func(t *testing.T, in *Installer) {
			in.Smoke = func(context.Context, string) (string, error) { return "", nil }
		}, "unexpected version"},
		{"version output is not a version", releaseFiles(bin), func(t *testing.T, in *Installer) {
			in.Smoke = func(context.Context, string) (string, error) { return "pitlane banana", nil }
		}, "reports version"},
		{"another program's output", releaseFiles(bin), func(t *testing.T, in *Installer) {
			in.Smoke = func(context.Context, string) (string, error) { return "other 0.3.0", nil }
		}, "unexpected version"},
		{"the directory is not writable", releaseFiles(bin), func(t *testing.T, in *Installer) {
			in.Executable = filepath.Join(t.TempDir(), "missing", "pitlane")
		}, "create temporary file"},
		{"the old backup cannot be removed", releaseFiles(bin), func(t *testing.T, in *Installer) {
			prev := in.Executable + ".prev"
			if err := os.MkdirAll(filepath.Join(prev, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "remove old backup"},
		{"the running binary cannot be backed up", releaseFiles(bin), func(t *testing.T, in *Installer) {
			if err := os.Remove(in.Executable); err != nil {
				t.Fatal(err)
			}
		}, "back up the current binary"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, g, rel := releaseServer(t, tc.files)
			in, exe := installer(t, g)
			if tc.mut != nil {
				tc.mut(t, in)
			}
			err := in.Install(context.Background(), rel, want, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if b, err := os.ReadFile(exe); err == nil && string(b) != "old binary" { //nolint:gosec // test file
				t.Errorf("binary replaced: %q", b)
			}
			if l := leftovers(t, filepath.Dir(exe)); len(l) != 0 {
				t.Errorf("temporary files left: %v", l)
			}
		})
	}
}

// A download cut short never gets installed, whether the connection drops
// or the server stops early.
func TestInstallTruncatedBinary(t *testing.T) {
	t.Parallel()
	bin := []byte(strings.Repeat("0123456789", 100))
	files := releaseFiles(bin)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		switch name {
		case "checksums.txt":
			_, _ = w.Write(files[name])
		case "pitlane_linux_amd64":
			w.Header().Set("Content-Length", fmt.Sprint(len(bin)))
			_, _ = w.Write(bin[:len(bin)/2])
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	g := &GitHub{HTTP: srv.Client(), Hosts: []string{srv.Listener.Addr().String()}}
	rel := Release{Tag: "v0.3.0"}
	for name := range files {
		rel.Assets = append(rel.Assets, Asset{Name: name, URL: srv.URL + "/dl/" + name})
	}
	in, exe := installer(t, g)
	called := false
	in.Smoke = func(context.Context, string) (string, error) { called = true; return "pitlane 0.3.0", nil }
	want, _ := ParseVersion("0.3.0")
	if err := in.Install(context.Background(), rel, want, nil); err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Error("the truncated binary was executed")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" { //nolint:gosec // test file
		t.Errorf("binary replaced: %q", b)
	}
	if l := leftovers(t, filepath.Dir(exe)); len(l) != 0 {
		t.Errorf("temporary files left: %v", l)
	}
}

// A binary shorter than announced by the checksum file is a mismatch even
// when the transfer itself ends cleanly.
func TestInstallShortButCleanDownload(t *testing.T) {
	t.Parallel()
	full := []byte("the complete binary")
	files := map[string][]byte{"pitlane_linux_amd64": full[:5], "checksums.txt": releaseFiles(full)["checksums.txt"]}
	_, g, rel := releaseServer(t, files)
	in, exe := installer(t, g)
	want, _ := ParseVersion("0.3.0")
	if err := in.Install(context.Background(), rel, want, nil); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" { //nolint:gosec // test file
		t.Errorf("binary replaced: %q", b)
	}
}

func TestInstallReplacesOldBackup(t *testing.T) {
	t.Parallel()
	bin := []byte("new binary")
	_, g, rel := releaseServer(t, releaseFiles(bin))
	in, exe := installer(t, g)
	if err := os.WriteFile(exe+".prev", []byte("ancient"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, _ := ParseVersion("0.3.0")
	if err := in.Install(context.Background(), rel, want, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "old binary" { //nolint:gosec // test file
		t.Errorf("backup = %q", b)
	}
	// Phases arrive in order and the attestation phase only exists with a verifier.
	var phases []string
	in2, _ := installer(t, g)
	in2.Verify = func(context.Context, Release, string) error { return nil }
	if err := in2.Install(context.Background(), rel, want, func(p string) { phases = append(phases, p) }); err != nil {
		t.Fatal(err)
	}
	wantPhases := []string{"downloading checksums", "downloading pitlane_linux_amd64", "verifying checksum", "verifying the build attestation", "testing the new binary", "installing"}
	if strings.Join(phases, "|") != strings.Join(wantPhases, "|") {
		t.Errorf("phases %q", phases)
	}
}

func TestInstallMaxBinaryDefault(t *testing.T) {
	t.Parallel()
	bin := []byte("new binary")
	_, g, rel := releaseServer(t, releaseFiles(bin))
	in, _ := installer(t, g)
	in.MaxBinary = 0 // default limit, far above this binary
	want, _ := ParseVersion("0.3.0")
	if err := in.Install(context.Background(), rel, want, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCopyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(dst, []byte("a much longer previous content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "content" { //nolint:gosec // test file
		t.Errorf("dst = %q (not truncated?)", b)
	}
	// A new backup is executable (Install removes the old one first).
	fresh := filepath.Join(dir, "fresh")
	if err := copyFile(src, fresh); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(fresh); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("the backup of an executable must stay executable: %v %v", fi, err)
	}
	if err := copyFile(filepath.Join(dir, "missing"), filepath.Join(dir, "x")); err == nil {
		t.Error("missing source accepted")
	}
	if err := copyFile(src, filepath.Join(dir, "no", "such", "dir")); err == nil {
		t.Error("unwritable destination accepted")
	}
	if err := copyFile(dir, filepath.Join(dir, "d2")); err == nil {
		t.Error("a directory was copied as a file")
	}
}

func TestDefaultSmoke(t *testing.T) {
	t.Parallel()
	echo, err := exec.LookPath("echo")
	if err != nil {
		t.Skip("no echo binary")
	}
	out, err := defaultSmoke(context.Background(), echo) // runs `echo version`
	if err != nil || strings.TrimSpace(out) != "version" {
		t.Fatalf("%q %v", out, err)
	}
	if f, err := exec.LookPath("false"); err == nil {
		if _, err := defaultSmoke(context.Background(), f); err == nil {
			t.Error("failing binary reported as healthy")
		}
	}
	if _, err := defaultSmoke(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing binary reported as healthy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := defaultSmoke(ctx, echo); err == nil {
		t.Error("cancelled context ignored")
	}
}

func TestDefaultProbe(t *testing.T) { // not parallel: sets an environment variable
	p := DefaultProbe("1.2.3")
	if p.Version != "1.2.3" || p.GOOS != runtime.GOOS || p.GOARCH != runtime.GOARCH {
		t.Errorf("%+v", p)
	}
	exe, err := p.Executable()
	if err != nil || !filepath.IsAbs(exe) {
		t.Errorf("executable %q %v", exe, err)
	}
	if !p.Exists("/") || p.Exists(filepath.Join(t.TempDir(), "nothing")) {
		t.Error("Exists")
	}
	t.Setenv("PITLANE_PROBE_TEST", "x")
	if p.Getenv("PITLANE_PROBE_TEST") != "x" {
		t.Error("Getenv")
	}
	dir := t.TempDir()
	if err := p.Writable(dir); err != nil {
		t.Errorf("writable temp dir: %v", err)
	}
	if l, _ := filepath.Glob(filepath.Join(dir, ".pitlane-write-test-*")); len(l) != 0 {
		t.Errorf("probe file left behind: %v", l)
	}
	if err := p.Writable(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory reported writable")
	}
}

func TestAssetName(t *testing.T) {
	t.Parallel()
	for arch, want := range map[string]string{"amd64": "pitlane_linux_amd64", "arm64": "pitlane_linux_arm64"} {
		if got := AssetName(arch); got != want {
			t.Errorf("%s: %s", arch, got)
		}
	}
}

func TestManualCommandModes(t *testing.T) {
	t.Parallel()
	tests := []struct{ mode, version, want, not string }{
		{ModeContainer, "v1.0.0", "docker compose pull", "install.sh"},
		{ModeDev, "v1.0.0", "make web build", "install.sh"},
		{ModeBinary, "", "install.sh", "PITLANE_VERSION"},
		{ModeSystemd, "v1.2.3", "PITLANE_VERSION=1.2.3", "v1.2.3"},
		{ModeManaged, "1.2.3", "PITLANE_VERSION=1.2.3", ""},
		{ModeUnsupport, "", "sudo", ""},
	}
	for _, tc := range tests {
		got := ManualCommand(tc.mode, tc.version)
		if !strings.Contains(got, tc.want) || (tc.not != "" && strings.Contains(got, tc.not)) {
			t.Errorf("%s/%q: %q", tc.mode, tc.version, got)
		}
	}
}
