package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// Who may build the binaries Pitlane installs. Set at build time only
// (forks: go build -ldflags "-X github.com/TuroYT/pitlane/internal/update.AttestationRepo=me/pitlane"),
// never from the UI or the environment.
var (
	// AttestationRepo is the repository whose workflow must have built a release binary.
	AttestationRepo = Repo
	// AttestationWorkflow is that workflow's path in the repository.
	AttestationWorkflow = ".github/workflows/release.yml"
)

const (
	// githubOIDCIssuer issues the certificates of GitHub Actions workflows.
	githubOIDCIssuer = "https://token.actions.githubusercontent.com"
	// slsaProvenance is the predicate of actions/attest-build-provenance.
	slsaProvenance = "https://slsa.dev/provenance/v1"
	// SkipAttestationEnv disables the attestation check (unsafe).
	SkipAttestationEnv = "PITLANE_UPDATE_SKIP_ATTESTATION"
)

// ErrNoAttestation means GitHub has no attestation for the binary's digest.
var ErrNoAttestation = errors.New("no build attestation found for this binary")

// AttestationVerifier checks, before a release binary is installed, that
// GitHub has a build provenance attestation for its exact SHA-256, signed
// through Sigstore's public-good instance (Fulcio certificate chain, signed
// certificate timestamp, Rekor transparency log inclusion) by the release
// workflow of AttestationRepo running for the release's tag.
type AttestationVerifier struct {
	GitHub   *GitHub
	Repo     string // "" = AttestationRepo
	Workflow string // "" = AttestationWorkflow
	// Asset is the release asset the binary must be attested as ("" =
	// AssetName(runtime.GOARCH)): one attestation covers every asset of a
	// release, the installers included.
	Asset string
	// CacheDir keeps Sigstore's TUF metadata ("" = no disk cache).
	CacheDir string
	// TrustedMaterial returns Sigstore's trusted root. Nil: fetched through
	// Sigstore's TUF repository (tests pass a fixed root).
	TrustedMaterial func(ctx context.Context) (root.TrustedMaterial, error)
	Log             *slog.Logger

	mu sync.Mutex // one TUF refresh at a time
}

func (a *AttestationVerifier) repo() string {
	if a.Repo != "" {
		return a.Repo
	}
	return AttestationRepo
}

func (a *AttestationVerifier) workflow() string {
	if a.Workflow != "" {
		return a.Workflow
	}
	return AttestationWorkflow
}

func (a *AttestationVerifier) asset() string {
	if a.Asset != "" {
		return a.Asset
	}
	return AssetName(runtime.GOARCH)
}

func (a *AttestationVerifier) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// trustedRoot gets Sigstore's public-good trusted root through its TUF
// repository (the client embeds the TUF root of trust, and refreshes and
// verifies the metadata on every call).
func (a *AttestationVerifier) trustedRoot(ctx context.Context) (root.TrustedMaterial, error) {
	if a.TrustedMaterial != nil {
		return a.TrustedMaterial(ctx)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	opts := tuf.DefaultOptions()
	opts.Context = ctx
	if a.CacheDir != "" {
		if err := os.MkdirAll(a.CacheDir, 0o700); err != nil {
			return nil, err
		}
		opts.CachePath = a.CacheDir
	} else {
		opts.DisableLocalCache = true
	}
	client, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("fetch the trusted root from Sigstore's TUF repository: %w", err)
	}
	tr, err := root.GetTrustedRoot(client)
	if err != nil {
		return nil, fmt.Errorf("read the Sigstore trusted root: %w", err)
	}
	return tr, nil
}

// fetchBundles returns the Sigstore bundles GitHub stores for a digest.
func (a *AttestationVerifier) fetchBundles(ctx context.Context, digest string) ([]*bundle.Bundle, error) {
	base := a.GitHub.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	resp, err := a.GitHub.get(ctx, fmt.Sprintf("%s/repos/%s/attestations/sha256:%s", base, a.repo(), digest))
	if err != nil {
		return nil, fmt.Errorf("GitHub attestations: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNoAttestation
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub attestations: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Attestations []struct {
			Bundle json.RawMessage `json:"bundle"`
		} `json:"attestations"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("GitHub attestations: %w", err)
	}
	var out []*bundle.Bundle
	var bad error
	for _, at := range body.Attestations {
		if len(at.Bundle) == 0 || string(at.Bundle) == "null" {
			continue
		}
		b := &bundle.Bundle{}
		if err := b.UnmarshalJSON(at.Bundle); err != nil {
			bad = err
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		if bad != nil {
			return nil, fmt.Errorf("%w (unreadable bundle: %v)", ErrNoAttestation, bad)
		}
		return nil, ErrNoAttestation
	}
	return out, nil
}

// identity is the certificate every accepted attestation must carry: issued
// for GitHub Actions, for the release workflow, at a v* tag.
func (a *AttestationVerifier) identity() (verify.CertificateIdentity, error) {
	san := "^" + regexp.QuoteMeta("https://github.com/"+a.repo()+"/"+a.workflow()+"@refs/tags/v") + `[^@]+$`
	return verify.NewShortCertificateIdentity(githubOIDCIssuer, "", "", san)
}

// VerifyDigest checks the attestations of a binary given its SHA-256.
func (a *AttestationVerifier) VerifyDigest(ctx context.Context, rel Release, digest []byte) error {
	hexDigest := hex.EncodeToString(digest)
	bundles, err := a.fetchBundles(ctx, hexDigest)
	if err != nil {
		return err
	}
	tr, err := a.trustedRoot(ctx)
	if err != nil {
		return err
	}
	v, err := verify.NewVerifier(tr, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return err
	}
	id, err := a.identity()
	if err != nil {
		return err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))
	var errs []error
	for _, b := range bundles {
		res, err := v.Verify(b, policy)
		if err == nil {
			err = a.checkResult(res, rel, hexDigest)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a.log().Info("update: build attestation verified", "release", rel.Tag, "sha256", hexDigest,
			"signer", res.Signature.Certificate.SubjectAlternativeName, "ref", res.Signature.Certificate.SourceRepositoryRef)
		return nil
	}
	return fmt.Errorf("no valid build attestation for sha256:%s: %w", hexDigest, errors.Join(errs...))
}

// checkResult adds what the identity policy does not cover: a SLSA
// provenance, attesting the digest as the expected asset, from the expected
// repository, built for this very release on a GitHub-hosted runner.
func (a *AttestationVerifier) checkResult(res *verify.VerificationResult, rel Release, hexDigest string) error {
	if res.Statement == nil || res.Statement.GetPredicateType() != slsaProvenance {
		return errors.New("not a SLSA build provenance attestation")
	}
	named := false
	for _, s := range res.Statement.GetSubject() {
		if s.GetName() == a.asset() && strings.EqualFold(s.GetDigest()["sha256"], hexDigest) {
			named = true
		}
	}
	if !named {
		return fmt.Errorf("the attestation does not cover this digest as %s", a.asset())
	}
	if res.Signature == nil || res.Signature.Certificate == nil {
		return errors.New("attestation not signed by a certificate")
	}
	c := res.Signature.Certificate
	if want := "https://github.com/" + a.repo(); !strings.EqualFold(c.SourceRepositoryURI, want) {
		return fmt.Errorf("built from %q, expected %s", c.SourceRepositoryURI, want)
	}
	if want := "refs/tags/" + rel.Tag; c.SourceRepositoryRef != want {
		return fmt.Errorf("built for %q, expected %s", c.SourceRepositoryRef, want)
	}
	if c.RunnerEnvironment != "github-hosted" {
		return fmt.Errorf("built on a %q runner, expected github-hosted", c.RunnerEnvironment)
	}
	return nil
}

// Verify is an update.Verifier: it hashes the downloaded binary and checks
// its attestation.
func (a *AttestationVerifier) Verify(ctx context.Context, rel Release, binaryPath string) error {
	f, err := os.Open(binaryPath) //nolint:gosec // G304: the binary the installer just downloaded
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if err := a.VerifyDigest(ctx, rel, h.Sum(nil)); err != nil {
		a.log().Error("update: build attestation verification failed, refusing to install", "release", rel.Tag, "error", err)
		return err
	}
	return nil
}

// ReleaseVerifier returns the verifier the updater uses: the build
// attestation check, unless SkipAttestationEnv is "true" (for forks that
// publish no attestation; the checksum is then the only check).
func ReleaseVerifier(getenv func(string) string, gh *GitHub, dataDir string, log *slog.Logger) (Verifier, bool) {
	if strings.EqualFold(getenv(SkipAttestationEnv), "true") {
		return func(context.Context, Release, string) error {
			log.Warn("update: build attestation check DISABLED by " + SkipAttestationEnv + "=true, relying on the checksum only (unsafe)")
			return nil
		}, false
	}
	a := &AttestationVerifier{GitHub: gh, CacheDir: filepath.Join(dataDir, "sigstore-tuf"), Log: log}
	return a.Verify, true
}
