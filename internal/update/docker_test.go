package update

import (
	"context"
	"os"
	"testing"

	"github.com/docker/docker/client"
)

// Needs a Docker daemon with ghcr.io/actions/actions-runner:latest pulled:
// PITLANE_DOCKER_TEST=1 go test ./internal/update -run Docker
func TestDockerRunnerVersion(t *testing.T) {
	if os.Getenv("PITLANE_DOCKER_TEST") == "" {
		t.Skip("set PITLANE_DOCKER_TEST=1 to run against the local Docker daemon")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	d := DockerImages{Cli: cli}
	ref := "ghcr.io/actions/actions-runner:latest"
	info, err := d.Inspect(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	v, err := d.RunnerVersion(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseVersion(v); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: id %s digest %s runner %s", ref, info.ID, info.Digest, v)
}
