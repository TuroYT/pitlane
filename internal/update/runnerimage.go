package update

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/TuroYT/pitlane/internal/pool"
)

// RunnerRepo publishes the GitHub Actions runner.
const RunnerRepo = "actions/runner"

// RunnerGrace is how long GitHub keeps accepting a runner version after a
// newer one is released, when the runner does not update itself (ephemeral
// runners in an image).
const RunnerGrace = 30 * 24 * time.Hour

// ImageInfo describes a local image.
type ImageInfo struct {
	ID      string
	Digest  string // repo digest (registry@sha256:…), empty for local-only images
	Created string
}

// Images is the Docker side of runner image updates (faked in tests).
type Images interface {
	Pull(ctx context.Context, ref string) error
	Inspect(ctx context.Context, ref string) (ImageInfo, error)
	// RunnerVersion reads the GitHub runner version inside the image.
	RunnerVersion(ctx context.Context, ref string) (string, error)
}

// DockerImages implements Images with the Docker API.
type DockerImages struct {
	Cli      *client.Client
	PullFunc func(ctx context.Context, ref string) error // pool.EnsureImage(ctx, ref, true)
}

// Pull pulls ref again from its registry.
func (d DockerImages) Pull(ctx context.Context, ref string) error { return d.PullFunc(ctx, ref) }

// Inspect returns the local image's ID and digest.
func (d DockerImages) Inspect(ctx context.Context, ref string) (ImageInfo, error) {
	info, err := d.Cli.ImageInspect(ctx, ref)
	if err != nil {
		return ImageInfo{}, err
	}
	out := ImageInfo{ID: info.ID, Created: info.Created}
	if len(info.RepoDigests) > 0 {
		out.Digest = info.RepoDigests[0]
		repo := ref
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		for _, d := range info.RepoDigests {
			if strings.HasPrefix(d, repo+"@") {
				out.Digest = d
			}
		}
	}
	return out, nil
}

// runnerDeps is a file of the runner's .NET build that carries its version.
// Reading it from a created (never started) container runs no code from the
// image and takes a few milliseconds.
const runnerDeps = "/home/runner/bin/Runner.Listener.deps.json"

var runnerVersionRe = regexp.MustCompile(`"Runner\.Listener/(\d+\.\d+\.\d+)"`)

// RunnerVersion reads the runner version from the image's files.
func (d DockerImages) RunnerVersion(ctx context.Context, ref string) (string, error) {
	c, err := d.Cli.ContainerCreate(ctx, &container.Config{
		Image: ref, Entrypoint: []string{"/bin/true"},
		Labels: map[string]string{pool.LabelManaged: "true", pool.LabelRole: "init"},
	}, nil, nil, nil, "")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = d.Cli.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true})
	}()
	rc, _, err := d.Cli.CopyFromContainer(ctx, c.ID, runnerDeps)
	if err != nil {
		return "", fmt.Errorf("%s not found in %s (not based on ghcr.io/actions/actions-runner?): %w", runnerDeps, ref, err)
	}
	defer func() { _ = rc.Close() }()
	return runnerVersionFromTar(rc)
}

func runnerVersionFromTar(r io.Reader) (string, error) {
	tr := tar.NewReader(r)
	if _, err := tr.Next(); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(tr, 4<<20))
	if err != nil {
		return "", err
	}
	m := runnerVersionRe.FindSubmatch(b)
	if m == nil {
		return "", errors.New("runner version not found in " + runnerDeps)
	}
	return string(m[1]), nil
}

// Runner image statuses.
const (
	RunnerCurrent  = "current"  // latest runner release
	RunnerOutdated = "outdated" // a newer runner exists, GitHub still accepts this one
	RunnerExpiring = "expiring" // GitHub refuses it within a week
	RunnerExpired  = "expired"  // past the grace period: GitHub may refuse it
	RunnerUnknown  = "unknown"
)

// RunnerStatus compares the image's runner version with the runner releases.
// GitHub stops accepting a runner that does not update itself about 30 days
// after a newer version is published; the deadline counts from the oldest
// release newer than the image's.
func RunnerStatus(imageVersion string, rels []Release, now time.Time) (string, time.Time) {
	cur, err := ParseVersion(imageVersion)
	if err != nil || len(rels) == 0 {
		return RunnerUnknown, time.Time{}
	}
	var superseded time.Time
	for _, r := range rels {
		v, err := ParseVersion(r.Tag)
		if err != nil || r.Prerelease || v.IsPrerelease() || Compare(v, cur) <= 0 {
			continue
		}
		if superseded.IsZero() || r.PublishedAt.Before(superseded) {
			superseded = r.PublishedAt
		}
	}
	if superseded.IsZero() {
		return RunnerCurrent, time.Time{}
	}
	deadline := superseded.Add(RunnerGrace)
	switch {
	case now.After(deadline):
		return RunnerExpired, deadline
	case deadline.Sub(now) < 7*24*time.Hour:
		return RunnerExpiring, deadline
	}
	return RunnerOutdated, deadline
}
