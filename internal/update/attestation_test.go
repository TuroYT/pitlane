package update

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// Fixtures, downloaded once:
//   - checksums-v0.1.1.txt: https://github.com/TuroYT/pitlane/releases/download/v0.1.1/checksums.txt
//   - attestations-v0.1.1-linux-amd64.json: GET https://api.github.com/repos/TuroYT/pitlane/attestations/sha256:<pitlane_linux_amd64 digest>
//   - sigstore-trusted-root.json: trusted_root.json of Sigstore's TUF repository (public-good instance)

func fixtureDigest(t *testing.T, asset string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "checksums-v0.1.1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == asset {
			d, err := hex.DecodeString(f[0])
			if err != nil {
				t.Fatal(err)
			}
			return d
		}
	}
	t.Fatalf("no %s in checksums", asset)
	return nil
}

func fixtureRoot(t *testing.T) func(context.Context) (root.TrustedMaterial, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sigstore-trusted-root.json"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := root.NewTrustedRootFromJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (root.TrustedMaterial, error) { return tr, nil }
}

// attestationServer serves body for any attestation request (status 200),
// or 404 when body is nil, and records the requested paths.
func attestationServer(t *testing.T, body []byte, paths *[]string) *GitHub {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		if body == nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &GitHub{HTTP: srv.Client(), APIBase: srv.URL, Hosts: []string{srv.Listener.Addr().String()}}
}

func fixtureAttestations(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "attestations-v0.1.1-linux-amd64.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tamper changes the DSSE envelope of every bundle in an API response.
func tamper(t *testing.T, body []byte, change func(env map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc["attestations"].([]any) {
		env := a.(map[string]any)["bundle"].(map[string]any)["dsseEnvelope"].(map[string]any)
		change(env)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newTestVerifier(t *testing.T, body []byte, paths *[]string) *AttestationVerifier {
	return &AttestationVerifier{GitHub: attestationServer(t, body, paths), TrustedMaterial: fixtureRoot(t), Asset: "pitlane_linux_amd64",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

var v011 = Release{Tag: "v0.1.1"}

func TestAttestationValid(t *testing.T) {
	var paths []string
	a := newTestVerifier(t, fixtureAttestations(t), &paths)
	digest := fixtureDigest(t, "pitlane_linux_amd64")
	if err := a.VerifyDigest(context.Background(), v011, digest); err != nil {
		t.Fatalf("real v0.1.1 attestation refused: %v", err)
	}
	arm := newTestVerifier(t, fixtureAttestations(t), &paths)
	arm.Asset = "pitlane_linux_arm64"
	if err := arm.VerifyDigest(context.Background(), v011, fixtureDigest(t, "pitlane_linux_arm64")); err != nil {
		t.Fatalf("arm64: %v", err)
	}
	paths = paths[:1]
	if want := "/repos/TuroYT/pitlane/attestations/sha256:" + hex.EncodeToString(digest); len(paths) != 1 || paths[0] != want {
		t.Errorf("requested %v, want %s", paths, want)
	}
}

func TestAttestationRefused(t *testing.T) {
	amd64 := fixtureDigest(t, "pitlane_linux_amd64")
	good := fixtureAttestations(t)
	for name, tc := range map[string]struct {
		body   []byte
		digest []byte
		rel    Release
		setup  func(a *AttestationVerifier)
		want   string
	}{
		// The attestation of other files (GitHub answering with the v0.1.1
		// attestation when asked for another digest).
		"wrong digest": {body: good, digest: bytes.Repeat([]byte{0xab}, 32), want: "digest"},
		// One attestation covers every asset: the arm64 binary (or the
		// installer) is not the amd64 binary.
		"other asset": {body: good, digest: fixtureDigest(t, "pitlane_linux_arm64"), want: "as pitlane_linux_amd64"},
		"installer":   {body: good, digest: fixtureDigest(t, "install.sh"), want: "as pitlane_linux_amd64"},
		"wrong workflow": {body: good, setup: func(a *AttestationVerifier) { a.Workflow = ".github/workflows/ci.yml" },
			want: "certificate identity"},
		"wrong repository": {body: good, setup: func(a *AttestationVerifier) { a.Repo = "someone/pitlane" },
			want: "certificate identity"},
		"other release":  {body: good, rel: Release{Tag: "v0.1.2"}, want: "refs/tags/v0.1.2"},
		"no attestation": {body: nil, want: ErrNoAttestation.Error()},
		"empty list":     {body: []byte(`{"attestations":[]}`), want: ErrNoAttestation.Error()},
		"null bundle":    {body: []byte(`{"attestations":[{"bundle":null}]}`), want: ErrNoAttestation.Error()},
		"garbage":        {body: []byte(`not json`), want: "GitHub attestations"},
		"tampered signature": {body: tamper(t, good, func(env map[string]any) {
			sig := env["signatures"].([]any)[0].(map[string]any)
			raw, _ := base64.StdEncoding.DecodeString(sig["sig"].(string))
			raw[len(raw)/2] ^= 0xff
			sig["sig"] = base64.StdEncoding.EncodeToString(raw)
		}), want: "no valid build attestation"},
		"tampered payload": {body: tamper(t, good, func(env map[string]any) {
			raw, _ := base64.StdEncoding.DecodeString(env["payload"].(string))
			env["payload"] = base64.StdEncoding.EncodeToString(bytes.Replace(raw, []byte("v0.1.1"), []byte("v0.1.9"), 1))
		}), want: "no valid build attestation"},
	} {
		t.Run(name, func(t *testing.T) {
			var paths []string
			a := newTestVerifier(t, tc.body, &paths)
			if tc.setup != nil {
				tc.setup(a)
			}
			digest, rel := tc.digest, tc.rel
			if digest == nil {
				digest = amd64
			}
			if rel.Tag == "" {
				rel = v011
			}
			err := a.VerifyDigest(context.Background(), rel, digest)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestAttestationVerifyHashesFile(t *testing.T) {
	var paths []string
	a := newTestVerifier(t, nil, &paths)
	bin := filepath.Join(t.TempDir(), "pitlane")
	if err := os.WriteFile(bin, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(context.Background(), v011, bin); !errors.Is(err, ErrNoAttestation) {
		t.Fatalf("got %v", err)
	}
	// sha256("hello")
	if want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; len(paths) != 1 || !strings.HasSuffix(paths[0], want) {
		t.Errorf("requested %v", paths)
	}
}

func TestReleaseVerifier(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == SkipAttestationEnv {
				return v
			}
			return ""
		}
	}
	if _, on := ReleaseVerifier(env(""), &GitHub{}, t.TempDir(), log); !on {
		t.Error("attestation check must be on by default")
	}
	if _, on := ReleaseVerifier(env("1"), &GitHub{}, t.TempDir(), log); !on {
		t.Error("only \"true\" disables the check")
	}
	v, on := ReleaseVerifier(env("TRUE"), &GitHub{}, t.TempDir(), log)
	if on || v(context.Background(), v011, "/nonexistent") != nil {
		t.Error("escape hatch must accept without checking")
	}
}
