package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Install modes.
const (
	ModeSystemd   = "systemd"   // binary started by systemd (deploy/install.sh, Proxmox LXC)
	ModeBinary    = "binary"    // binary started some other way
	ModeContainer = "container" // Docker / Compose / Podman: update the image instead
	ModeDev       = "dev"       // development build, no comparable version
	ModeManaged   = "managed"   // self-update disabled with PITLANE_SELF_UPDATE=false
	ModeUnsupport = "unsupported"
)

// Repo is the GitHub repository Pitlane releases are published in.
const Repo = "TuroYT/pitlane"

// Capability says whether Pitlane can replace its own binary, and if not,
// what the operator should run instead.
type Capability struct {
	Mode          string `json:"mode"`
	CanSelfUpdate bool   `json:"canSelfUpdate"`
	Reason        string `json:"reason,omitempty"`
	Executable    string `json:"executable,omitempty"`
}

// Probe gives Detect access to the environment (replaced in tests).
type Probe struct {
	Version    string
	GOOS       string
	GOARCH     string
	Executable func() (string, error)
	Exists     func(path string) bool
	Getenv     func(string) string
	Writable   func(dir string) error
}

// DefaultProbe inspects the running process.
func DefaultProbe(version string) Probe {
	return Probe{
		Version: version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Executable: func() (string, error) {
			p, err := os.Executable()
			if err != nil {
				return "", err
			}
			return filepath.EvalSymlinks(p)
		},
		Exists: func(path string) bool { _, err := os.Stat(path); return err == nil },
		Getenv: os.Getenv,
		Writable: func(dir string) error {
			f, err := os.CreateTemp(dir, ".pitlane-write-test-*")
			if err != nil {
				return err
			}
			name := f.Name()
			_ = f.Close()
			return os.Remove(name)
		},
	}
}

// Detect decides whether a self-update is possible.
func Detect(p Probe) Capability {
	if strings.EqualFold(p.Getenv("PITLANE_SELF_UPDATE"), "false") {
		return Capability{Mode: ModeManaged, Reason: "self-update is disabled by PITLANE_SELF_UPDATE=false"}
	}
	if p.Exists("/.dockerenv") || p.Exists("/run/.containerenv") {
		return Capability{Mode: ModeContainer, Reason: "Pitlane runs in a container: update its image instead"}
	}
	if _, ok := ReleaseVersion(p.Version); !ok {
		return Capability{Mode: ModeDev, Reason: fmt.Sprintf("development build (%s): install a release to get updates", p.Version)}
	}
	if p.GOOS != "linux" || (p.GOARCH != "amd64" && p.GOARCH != "arm64") {
		return Capability{Mode: ModeUnsupport, Reason: fmt.Sprintf("releases are only published for linux/amd64 and linux/arm64, not %s/%s", p.GOOS, p.GOARCH)}
	}
	exe, err := p.Executable()
	if err != nil {
		return Capability{Mode: ModeUnsupport, Reason: "cannot locate the running binary: " + err.Error()}
	}
	mode := ModeBinary
	if p.Getenv("INVOCATION_ID") != "" {
		mode = ModeSystemd
	}
	if err := p.Writable(filepath.Dir(exe)); err != nil {
		return Capability{Mode: mode, Executable: exe, Reason: fmt.Sprintf("%s is not writable by Pitlane: %v", filepath.Dir(exe), err)}
	}
	return Capability{Mode: mode, CanSelfUpdate: true, Executable: exe}
}

// ManualCommand is what to run when Pitlane cannot update itself.
func ManualCommand(mode, version string) string {
	switch mode {
	case ModeContainer:
		return "docker compose pull && docker compose up -d"
	case ModeDev:
		return "git pull && make web build"
	}
	env := ""
	if version != "" {
		env = " PITLANE_VERSION=" + strings.TrimPrefix(version, "v")
	}
	return "curl -fsSL https://raw.githubusercontent.com/" + Repo + "/main/deploy/install.sh | sudo" + env + " sh"
}

// Verifier is the hook for an authenticity check of a downloaded release
// binary beyond its checksum (a signature or a GitHub build attestation).
// The checksum only proves the binary matches checksums.txt of the same
// release, i.e. it protects against corrupted or swapped downloads, not
// against a compromised release.
type Verifier func(ctx context.Context, rel Release, binaryPath string) error

// Installer downloads, verifies and installs a release binary in place.
type Installer struct {
	GitHub     *GitHub
	Executable string // binary to replace
	Arch       string // runtime.GOARCH
	Verify     Verifier
	// Smoke runs the new binary with "version" and returns its output.
	Smoke     func(ctx context.Context, path string) (string, error)
	MaxBinary int64 // default 200 MB
	Log       *slog.Logger
}

// AssetName is the release asset for an architecture.
func AssetName(arch string) string { return "pitlane_linux_" + arch }

const checksumsAsset = "checksums.txt"

func defaultSmoke(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The path is the verified release binary Pitlane is about to run anyway.
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput() //nolint:gosec // G204: verified release binary
	return string(out), err
}

// Install replaces the running binary with the release's. On success the old
// binary is kept as <executable>.prev; on any failure nothing is replaced.
func (in *Installer) Install(ctx context.Context, rel Release, want Version, phase func(string)) error {
	log := in.Log
	if log == nil {
		log = slog.Default()
	}
	if phase == nil {
		phase = func(string) {}
	}
	name := AssetName(in.Arch)
	bin, ok := rel.Asset(name)
	if !ok {
		return fmt.Errorf("release %s has no %s asset", rel.Tag, name)
	}
	sumAsset, ok := rel.Asset(checksumsAsset)
	if !ok {
		return fmt.Errorf("release %s has no %s: refusing to install an unverifiable binary", rel.Tag, checksumsAsset)
	}

	phase("downloading checksums")
	log.Info("update: downloading checksums", "release", rel.Tag, "url", sumAsset.URL)
	var sums bytes.Buffer
	if _, err := in.GitHub.Download(ctx, sumAsset.URL, 64<<10, &sums); err != nil {
		return err
	}
	parsed, err := ParseChecksums(sums.Bytes())
	if err != nil {
		return err
	}
	if _, ok := parsed[name]; !ok {
		return fmt.Errorf("checksums.txt of %s has no entry for %s", rel.Tag, name)
	}

	dir := filepath.Dir(in.Executable)
	tmp, err := os.CreateTemp(dir, ".pitlane-update-*")
	if err != nil {
		return fmt.Errorf("create temporary file next to %s: %w", in.Executable, err)
	}
	installed := false
	defer func() {
		_ = tmp.Close()
		if !installed {
			_ = os.Remove(tmp.Name())
		}
	}()

	limit := in.MaxBinary
	if limit <= 0 {
		limit = 200 << 20
	}
	phase("downloading " + name)
	log.Info("update: downloading binary", "release", rel.Tag, "asset", name, "size", bin.Size)
	h := sha256.New()
	n, err := in.GitHub.Download(ctx, bin.URL, limit, io.MultiWriter(tmp, h))
	if err != nil {
		return err
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	phase("verifying checksum")
	if err := VerifyChecksum(sums.Bytes(), name, got); err != nil {
		log.Error("update: checksum verification failed", "release", rel.Tag, "error", err)
		return err
	}
	log.Info("update: checksum verified", "release", rel.Tag, "bytes", n, "sha256", fmt.Sprintf("%x", got))

	if in.Verify != nil {
		phase("verifying signature")
		if err := in.Verify(ctx, rel, tmp.Name()); err != nil {
			return fmt.Errorf("release verification: %w", err)
		}
	} else {
		log.Warn("update: no signature/attestation verification configured, relying on the checksum only")
	}

	if err := tmp.Chmod(0o755); err != nil { //nolint:gosec // G302: an executable must be executable
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	phase("testing the new binary")
	smoke := in.Smoke
	if smoke == nil {
		smoke = defaultSmoke
	}
	out, err := smoke(ctx, tmp.Name())
	if err != nil {
		return fmt.Errorf("the new binary does not run (%v): %s", err, strings.TrimSpace(out))
	}
	if f := strings.Fields(out); len(f) < 2 || f[0] != "pitlane" {
		return fmt.Errorf("the new binary printed an unexpected version: %q", strings.TrimSpace(out))
	} else if v, err := ParseVersion(f[1]); err != nil || Compare(v, want) != 0 {
		return fmt.Errorf("the new binary reports version %q, expected %s", f[1], want)
	}

	phase("installing")
	prev := in.Executable + ".prev"
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old backup: %w", err)
	}
	if err := os.Link(in.Executable, prev); err != nil {
		if err := copyFile(in.Executable, prev); err != nil {
			return fmt.Errorf("back up the current binary to %s: %w", prev, err)
		}
	}
	if err := os.Rename(tmp.Name(), in.Executable); err != nil {
		return fmt.Errorf("replace %s: %w", in.Executable, err)
	}
	installed = true
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: directory of our own binary
		_ = d.Sync()
		_ = d.Close()
	}
	log.Info("update: new binary installed", "release", rel.Tag, "path", in.Executable, "backup", prev)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: our own binary
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755) //nolint:gosec // G302,G304: backup of our own executable
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
