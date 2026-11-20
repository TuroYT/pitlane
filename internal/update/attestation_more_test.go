package update

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// verifiedResult runs the real v0.1.1 attestation through the Sigstore
// verifier and returns what checkResult receives.
func verifiedResult(t *testing.T) (*verify.VerificationResult, string) {
	t.Helper()
	var paths []string
	a := newTestVerifier(t, fixtureAttestations(t), &paths)
	hexDigest := ""
	digest := fixtureDigest(t, "pitlane_linux_amd64")
	for _, b := range digest {
		hexDigest += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&15])
	}
	bundles, err := a.fetchBundles(context.Background(), hexDigest)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := a.trustedRoot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v, err := verify.NewVerifier(tr, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.identity()
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.Verify(bundles[0], verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id)))
	if err != nil {
		t.Fatal(err)
	}
	return res, hexDigest
}

// checkResult adds the checks the certificate identity policy does not make.
func TestAttestationCheckResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mut     func(a *AttestationVerifier, res *verify.VerificationResult, rel *Release, digest *string)
		wantErr string
	}{
		{"the genuine attestation", func(*AttestationVerifier, *verify.VerificationResult, *Release, *string) {}, ""},
		{"digest in capitals is the same digest", func(_ *AttestationVerifier, _ *verify.VerificationResult, _ *Release, d *string) {
			*d = strings.ToUpper(*d)
		}, ""},
		{"no statement", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Statement = nil
		}, "not a SLSA build provenance"},
		{"not a provenance", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Statement.PredicateType = "https://example.com/other/v1"
		}, "not a SLSA build provenance"},
		{"another subject name", func(a *AttestationVerifier, _ *verify.VerificationResult, _ *Release, _ *string) {
			a.Asset = "pitlane_linux_arm64"
		}, "does not cover this digest as pitlane_linux_arm64"},
		{"another digest", func(_ *AttestationVerifier, _ *verify.VerificationResult, _ *Release, d *string) {
			*d = strings.Repeat("ab", 32)
		}, "does not cover this digest"},
		{"no subjects", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Statement.Subject = nil
		}, "does not cover this digest"},
		{"no signature", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature = nil
		}, "not signed by a certificate"},
		{"signed by a key, not a certificate", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature.Certificate = nil
		}, "not signed by a certificate"},
		{"another repository", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature.Certificate.SourceRepositoryURI = "https://github.com/evil/pitlane"
		}, "built from"},
		{"repository in another case is the same repository", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature.Certificate.SourceRepositoryURI = strings.ToUpper(res.Signature.Certificate.SourceRepositoryURI)
		}, ""},
		{"built for another tag", func(_ *AttestationVerifier, _ *verify.VerificationResult, rel *Release, _ *string) {
			rel.Tag = "v9.9.9"
		}, "built for"},
		{"built from a branch", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature.Certificate.SourceRepositoryRef = "refs/heads/main"
		}, "built for"},
		{"self-hosted runner", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature.Certificate.RunnerEnvironment = "self-hosted"
		}, "self-hosted"},
		{"unknown runner environment", func(_ *AttestationVerifier, res *verify.VerificationResult, _ *Release, _ *string) {
			res.Signature.Certificate.RunnerEnvironment = ""
		}, "github-hosted"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, digest := verifiedResult(t)
			a := &AttestationVerifier{Asset: "pitlane_linux_amd64"}
			rel := v011
			tc.mut(a, res, &rel, &digest)
			err := a.checkResult(res, rel, digest)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAttestationDefaults(t *testing.T) {
	t.Parallel()
	a := &AttestationVerifier{}
	if a.repo() != AttestationRepo || a.repo() != Repo {
		t.Errorf("repo %q", a.repo())
	}
	if a.workflow() != AttestationWorkflow {
		t.Errorf("workflow %q", a.workflow())
	}
	if a.asset() != AssetName(runtime.GOARCH) {
		t.Errorf("asset %q", a.asset())
	}
	if a.log() == nil {
		t.Error("no default logger")
	}
	a = &AttestationVerifier{Repo: "me/fork", Workflow: "w.yml", Asset: "x", Log: newBufLogger(&bytes.Buffer{})}
	if a.repo() != "me/fork" || a.workflow() != "w.yml" || a.asset() != "x" {
		t.Errorf("overrides ignored: %q %q %q", a.repo(), a.workflow(), a.asset())
	}
	id, err := a.identity()
	if err != nil {
		t.Fatal(err)
	}
	if id.SubjectAlternativeName.Regexp.String() != `^https://github\.com/me/fork/w\.yml@refs/tags/v[^@]+$` {
		t.Errorf("identity pattern %s", id.SubjectAlternativeName.Regexp.String())
	}
}

func TestAttestationFetchFailures(t *testing.T) {
	t.Parallel()
	good := fixtureAttestations(t)
	digest := fixtureDigest(t, "pitlane_linux_amd64")
	var doc struct {
		Attestations []map[string]any `json:"attestations"`
	}
	if err := json.Unmarshal(good, &doc); err != nil || len(doc.Attestations) == 0 {
		t.Fatalf("fixture: %v", err)
	}
	broken := map[string]any{"bundle": map[string]any{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json"}}
	forged := tamper(t, good, func(env map[string]any) {
		sig := env["signatures"].([]any)[0].(map[string]any)
		raw, _ := base64.StdEncoding.DecodeString(sig["sig"].(string))
		raw[0] ^= 0xff
		sig["sig"] = base64.StdEncoding.EncodeToString(raw)
	})
	var forgedDoc struct {
		Attestations []map[string]any `json:"attestations"`
	}
	_ = json.Unmarshal(forged, &forgedDoc)
	mixed, _ := json.Marshal(map[string]any{"attestations": append(append([]map[string]any{broken}, forgedDoc.Attestations...), doc.Attestations...)})

	tests := []struct {
		name    string
		body    []byte
		wantErr string // "" = verified
		is      error
	}{
		{"unreadable bundle only", []byte(`{"attestations":[{"bundle":{"x":1}}]}`), "unreadable bundle", ErrNoAttestation},
		{"missing bundle key", []byte(`{"attestations":[{}]}`), "", ErrNoAttestation},
		{"a good attestation after broken and forged ones", mixed, "", nil},
		{"forged only", forged, "no valid build attestation", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var paths []string
			a := newTestVerifier(t, tc.body, &paths)
			err := a.VerifyDigest(context.Background(), v011, digest)
			isGood := tc.name == "a good attestation after broken and forged ones"
			switch {
			case isGood && err != nil:
				t.Fatalf("refused: %v", err)
			case !isGood && err == nil:
				t.Fatal("accepted")
			case !isGood && tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			case !isGood && tc.is != nil && !errors.Is(err, tc.is):
				t.Fatalf("err = %v, want %v", err, tc.is)
			}
		})
	}
}

func TestAttestationAPIErrors(t *testing.T) {
	t.Parallel()
	digest := fixtureDigest(t, "pitlane_linux_amd64")
	for _, code := range []int{403, 429, 500, 502} {
		g, _ := tlsGitHub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
		a := &AttestationVerifier{GitHub: g, TrustedMaterial: fixtureRoot(t)}
		err := a.VerifyDigest(context.Background(), v011, digest)
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", code)) || errors.Is(err, ErrNoAttestation) {
			t.Errorf("HTTP %d: %v (an outage is not a missing attestation)", code, err)
		}
	}
	// The API host is restricted like every other URL.
	a := &AttestationVerifier{GitHub: &GitHub{APIBase: "https://evil.example"}, TrustedMaterial: fixtureRoot(t)}
	if err := a.VerifyDigest(context.Background(), v011, digest); err == nil || !strings.Contains(err.Error(), "unexpected host") {
		t.Errorf("foreign API host: %v", err)
	}
}

// A trusted root that cannot be had refuses the installation (and is only
// asked for once an attestation exists).
func TestAttestationTrustedRootFailures(t *testing.T) {
	t.Parallel()
	digest := fixtureDigest(t, "pitlane_linux_amd64")
	var paths []string
	a := newTestVerifier(t, fixtureAttestations(t), &paths)
	rootErr := errors.New("TUF repository unreachable")
	a.TrustedMaterial = func(context.Context) (root.TrustedMaterial, error) { return nil, rootErr }
	if err := a.VerifyDigest(context.Background(), v011, digest); !errors.Is(err, rootErr) {
		t.Fatalf("err = %v", err)
	}
	asked := false
	b := newTestVerifier(t, nil, &paths)
	b.TrustedMaterial = func(context.Context) (root.TrustedMaterial, error) { asked = true; return nil, rootErr }
	if err := b.VerifyDigest(context.Background(), v011, digest); !errors.Is(err, ErrNoAttestation) || asked {
		t.Fatalf("err = %v, trusted root fetched: %v", err, asked)
	}

	// Without an injected root, the Sigstore TUF cache directory must be usable.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &AttestationVerifier{CacheDir: filepath.Join(file, "tuf")}
	if _, err := c.trustedRoot(context.Background()); err == nil {
		t.Fatal("unusable cache directory accepted")
	}
}

func TestAttestationVerifyMissingFile(t *testing.T) {
	t.Parallel()
	var paths []string
	a := newTestVerifier(t, nil, &paths)
	if err := a.Verify(context.Background(), v011, filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrNoAttestation) {
		t.Fatalf("err = %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("GitHub contacted for a file that does not exist: %v", paths)
	}
	// A directory cannot be hashed.
	if err := a.Verify(context.Background(), v011, t.TempDir()); err == nil {
		t.Error("a directory was verified")
	}
}

// The failure is logged: it is what the operator reads after a refused update.
func TestAttestationVerifyLogsRefusal(t *testing.T) {
	t.Parallel()
	var paths []string
	a := newTestVerifier(t, nil, &paths)
	var buf bytes.Buffer
	a.Log = newBufLogger(&buf)
	bin := filepath.Join(t.TempDir(), "pitlane")
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Verify(context.Background(), v011, bin); err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(buf.String(), "refusing to install") {
		t.Errorf("log: %s", buf.String())
	}
}
