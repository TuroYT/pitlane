package pool

import (
	"regexp"
	"strings"
)

// Images Pitlane pulls and runs. Third-party images are pinned by digest
// (several run privileged); Renovate keeps the tags and digests up to date
// (see renovate.json). Refresh one by hand with
// `docker buildx imagetools inspect <name>:<tag>` (or `crane digest`).
const (
	// GitHub's official runner image, used when Pitlane's own image is
	// unavailable or chosen in the settings.
	OfficialRunnerImage = "ghcr.io/actions/actions-runner:2.338.2@sha256:6d0b2d80d80f143f77dcb676e50920100bbb547e39e883454d57f45ab8c31300"
	// Docker-in-Docker daemon of every slot (privileged).
	DefaultDindImage = "docker:28-dind@sha256:2a232a42256f70d78e3cc5d2b5d6b3276710a0de0596c145f627ecfae90282ac"
	// Registers the QEMU emulators (privileged, on the host daemon).
	binfmtImage = "tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0"
	// Pull-through Docker Hub mirror.
	mirrorImage = "registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

	// Pitlane's runner image: GitHub's official image plus the build tools
	// most CIs expect (make, gcc, zip…). Releases publish it with the same
	// tag as the binary.
	runnerRepo = "ghcr.io/turoyt/pitlane-runner"
	// Runner image tag used by builds without a release version (dev,
	// `git describe` output): the latest release.
	devRunnerTag = "v1.1.0"

	// Former defaults: settings saved by older versions hold them verbatim.
	legacyRunnerImage = runnerRepo + ":latest"
	legacyDindImage   = "docker:28-dind"
)

// DefaultRunnerImage is Pitlane's runner image matching this binary's
// version. main calls SetVersion before creating the pool.
var DefaultRunnerImage = RunnerImageFor("")

// SetVersion makes DefaultRunnerImage the runner image released with
// version v.
func SetVersion(v string) { DefaultRunnerImage = RunnerImageFor(v) }

var releaseVersion = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-(?:alpha|beta|rc)(?:\.?\d+)?)?)$`)

// RunnerImageFor returns Pitlane's runner image released with version v
// ("0.2.0" or "v0.2.0"). Anything else (dev, git describe output) gets the
// runner image of the latest release.
func RunnerImageFor(v string) string {
	if m := releaseVersion.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
		return runnerRepo + ":v" + m[1]
	}
	return runnerRepo + ":" + devRunnerTag
}

// isDefaultRunnerImage reports whether ref is one of Pitlane's own runner
// images selected by default: the former ":latest" or a release tag. The UI
// saves the effective image with every settings change, so such a value
// follows the binary's version. Pin a specific runner image by digest.
func isDefaultRunnerImage(ref string) bool {
	if ref == legacyRunnerImage {
		return true
	}
	tag, ok := strings.CutPrefix(ref, runnerRepo+":v")
	return ok && releaseVersion.MatchString(tag)
}
