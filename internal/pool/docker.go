package pool

import (
	"context"
	"fmt"
	"io"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
)

// EnsureImage pulls an image if missing (or always when force is set).
func (p *Pool) EnsureImage(ctx context.Context, ref string, force bool) error {
	if !force {
		if _, err := p.cli.ImageInspect(ctx, ref); err == nil {
			return nil
		}
	}
	p.log.Info("pulling image", "image", ref)
	rc, err := p.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc)
	return err
}

func (p *Pool) ensureMirror(ctx context.Context, cfg Config) error {
	if !cfg.Mirror {
		return nil
	}
	if err := p.ensureNetwork(ctx, mirrorNetwork, false); err != nil {
		return err
	}
	if _, err := p.cli.ContainerInspect(ctx, mirrorName); err == nil {
		_ = p.cli.ContainerStart(ctx, mirrorName, container.StartOptions{})
		return nil
	}
	const img = mirrorImage
	if err := p.EnsureImage(ctx, img, false); err != nil {
		return err
	}
	if err := p.ensureVolume(ctx, "pitlane-mirror-data"); err != nil {
		return err
	}
	_, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image:  img,
		Env:    []string{"REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io", "REGISTRY_STORAGE_DELETE_ENABLED=true"},
		Labels: map[string]string{LabelManaged: "true", LabelRole: "mirror"},
	}, &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		Mounts:        []mount.Mount{{Type: mount.TypeVolume, Source: "pitlane-mirror-data", Target: "/var/lib/registry"}},
	}, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{mirrorNetwork: {Aliases: []string{mirrorName}}}}, nil, mirrorName)
	if err != nil {
		return err
	}
	return p.cli.ContainerStart(ctx, mirrorName, container.StartOptions{})
}

func (p *Pool) ensureNetwork(ctx context.Context, name string, internal bool) error {
	if _, err := p.cli.NetworkInspect(ctx, name, network.InspectOptions{}); err == nil {
		return nil
	}
	_, err := p.cli.NetworkCreate(ctx, name, network.CreateOptions{Driver: "bridge", Internal: internal, Labels: map[string]string{LabelManaged: "true"}})
	if err != nil && !cerrdefs.IsConflict(err) {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// createVolume creates a managed volume unless it exists.
func (p *Pool) createVolume(ctx context.Context, name string) error {
	if _, err := p.cli.VolumeInspect(ctx, name); err == nil {
		return nil
	}
	if _, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{LabelManaged: "true"}}); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// ensureVolume creates a volume and makes its root writable by the runner user.
func (p *Pool) ensureVolume(ctx context.Context, name string) error {
	p.mu.Lock()
	done := p.chowned[name]
	p.mu.Unlock()
	if done {
		return nil
	}
	if err := p.createVolume(ctx, name); err != nil {
		return err
	}
	cfg := p.Config()
	c, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: cfg.RunnerImage, User: "root", Entrypoint: []string{"chown", runnerUID, "/v"},
		Labels: map[string]string{LabelManaged: "true", LabelRole: "init"},
	}, &container.HostConfig{AutoRemove: true, Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/v"}}}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("prepare volume %s: %w", name, err)
	}
	if err := p.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		return err
	}
	p.Wait(ctx, c.ID)
	p.mu.Lock()
	p.chowned[name] = true
	p.mu.Unlock()
	return nil
}

// DiskUsage returns the bytes used by each slot's Docker data volume.
func (p *Pool) DiskUsage(ctx context.Context) map[string]int64 {
	du, err := p.cli.DiskUsage(ctx, types.DiskUsageOptions{Types: []types.DiskUsageObject{types.VolumeObject}})
	out := map[string]int64{}
	if err != nil {
		return out
	}
	for _, v := range du.Volumes {
		if v.Labels[LabelManaged] == "true" && v.UsageData != nil {
			out[v.Name] = v.UsageData.Size
		}
	}
	return out
}

// Wait blocks until the container exits and returns its exit code.
func (p *Pool) Wait(ctx context.Context, id string) int64 {
	statusCh, errCh := p.cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	select {
	case st := <-statusCh:
		return st.StatusCode
	case err := <-errCh:
		if err != nil && ctx.Err() == nil {
			p.log.Warn("wait for container", "container", id, "error", err)
		}
		return -1
	case <-ctx.Done():
		return -1
	}
}

// Remove deletes a container. A container already gone, or already being
// removed (a scale-down and the runner's own exit race), is not an error;
// any other failure is logged (the container stays until the next restart
// finds it).
func (p *Pool) Remove(ctx context.Context, id string) {
	if err := p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsConflict(err) {
		p.log.Warn("remove container", "container", id, "error", err)
	}
}

// alreadyConnected reports whether a NetworkConnect error only means that the
// container is already on the network.
func alreadyConnected(err error) bool {
	return cerrdefs.IsConflict(err) || strings.Contains(err.Error(), "already exists")
}
