package pool

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
)

// Slot daemon (Docker-in-Docker) settings.
const (
	dindReadyTimeout = 90 * time.Second       // how long a daemon may take to answer after its start
	dindPollInterval = 500 * time.Millisecond // between two readiness checks
	mirrorURL        = "http://" + mirrorName + ":5000"
)

// ensureSlot makes slot i's daemon run with the current definition: its
// network and volumes exist, its container is created (or recreated when
// outdated), started and answering.
func (p *Pool) ensureSlot(ctx context.Context, i int) error {
	cfg := p.Config()
	if err := p.ensureSlotResources(ctx, i); err != nil {
		return err
	}
	name := slotName(i)
	info, err := p.cli.ContainerInspect(ctx, name)
	switch {
	case cerrdefs.IsNotFound(err):
		if err := p.createDind(ctx, cfg, i); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("inspect dind: %w", err)
	case info.Config.Labels[LabelSpec] != slotSpecVersion:
		return p.upgradeDind(ctx, i)
	case info.State != nil && info.State.Running:
		if cfg.Mirror {
			p.connectMirror(ctx, i)
		}
		return p.waitDind(ctx, name)
	}
	return p.startDind(ctx, cfg, i)
}

// ensureSlotResources creates the slot's network and volumes.
func (p *Pool) ensureSlotResources(ctx context.Context, i int) error {
	if err := p.ensureNetwork(ctx, slotNetwork(i), false); err != nil {
		return err
	}
	for _, v := range slotVolumes(i) {
		if err := p.ensureVolume(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// createDind creates the slot container (not started).
func (p *Pool) createDind(ctx context.Context, cfg Config, i int) error {
	cc, hc, nc := dindSpec(cfg, i)
	if _, err := p.cli.ContainerCreate(ctx, cc, hc, nc, nil, slotName(i)); err != nil {
		return fmt.Errorf("create dind: %w", err)
	}
	return nil
}

// dindSpec is the definition of slot i's container. Changing it requires
// bumping slotSpecVersion, so that existing slots are recreated.
func dindSpec(cfg Config, i int) (*container.Config, *container.HostConfig, *network.NetworkingConfig) {
	name := slotName(i)
	cmd := []string{"--host=" + dindAddr, "--host=unix:///var/run/docker.sock", "--tls=false"}
	if cfg.Mirror {
		cmd = append(cmd, "--registry-mirror="+mirrorURL)
	}
	cc := &container.Config{
		Image: cfg.DindImage,
		// Skip dockerd-entrypoint.sh: it would add its own TCP listener on
		// 0.0.0.0. The "dind" wrapper only prepares cgroups and mounts.
		Entrypoint: []string{"dind", "dockerd"},
		Cmd:        cmd,
		Env:        []string{"DOCKER_TLS_CERTDIR="},
		Labels:     map[string]string{LabelManaged: "true", LabelRole: "dind", LabelSlot: strconv.Itoa(i), LabelSpec: slotSpecVersion},
		Hostname:   name,
	}
	hc := &container.HostConfig{
		Privileged:    true,
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		// /run (= /var/run) on tmpfs: emptied on every start. After an
		// unclean stop (host out of memory, power loss) a docker.pid left
		// in the container's filesystem made dockerd refuse to start
		// ("process with PID 1 is still running") and the slot restarted
		// in a loop.
		Tmpfs: map[string]string{"/run": "rw,nosuid,nodev,mode=755"},
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: slotDocker(i), Target: "/var/lib/docker"},
			{Type: mount.TypeVolume, Source: slotWork(i), Target: workDir},
			{Type: mount.TypeVolume, Source: slotExternals(i), Target: externalsDir, ReadOnly: true},
		},
	}
	hc.Memory = cfg.memoryBytes()
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{slotNetwork(i): {Aliases: []string{name}}}}
	return cc, hc, nc
}

// upgradeDind replaces a slot container created by an older Pitlane. While a
// job runs in it, the slot is only marked: Release recreates it once the job
// is done. Otherwise it is recreated now; its volumes (and cache) are kept.
func (p *Pool) upgradeDind(ctx context.Context, i int) error {
	name := slotName(i)
	if p.slotHasRunner(ctx, i) {
		p.mu.Lock()
		if s, ok := p.slots[i]; ok {
			s.upgrade = true
		}
		p.mu.Unlock()
		return p.waitDind(ctx, name)
	}
	p.log.Info("recreating slot with the current definition", "slot", i)
	if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("remove outdated dind: %w", err)
	}
	return p.ensureSlot(ctx, i)
}

// startDind starts a stopped (or just created) slot container and waits for
// its daemon.
func (p *Pool) startDind(ctx context.Context, cfg Config, i int) error {
	name := slotName(i)
	if cfg.Mirror {
		p.connectMirror(ctx, i)
	}
	if err := p.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return fmt.Errorf("start dind: %w", err)
	}
	if err := p.waitDind(ctx, name); err != nil {
		return err
	}
	// The work directory is shared with runner containers (uid 1001).
	_, _, err := p.ops().exec(ctx, name, "chown "+runnerUID+" "+workDir)
	return err
}

// connectMirror attaches the mirror to the slot's own network so slots never
// share a network with each other.
func (p *Pool) connectMirror(ctx context.Context, i int) {
	_ = p.cli.NetworkDisconnect(ctx, mirrorNetwork, slotName(i), true) // layout of older versions
	err := p.cli.NetworkConnect(ctx, slotNetwork(i), mirrorName, &network.EndpointSettings{Aliases: []string{mirrorName}})
	if err != nil && !alreadyConnected(err) {
		p.log.Warn("connect mirror to slot network", "slot", i, "error", err)
	}
}

// waitDind waits until the daemon in container name answers.
func (p *Pool) waitDind(ctx context.Context, name string) error {
	deadline := time.Now().Add(dindReadyTimeout)
	for time.Now().Before(deadline) {
		if _, code, err := p.ops().exec(ctx, name, "docker info >/dev/null 2>&1"); err == nil && code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(dindPollInterval):
		}
	}
	return errors.New("docker daemon in " + name + " did not become ready")
}

// slotHasRunner reports whether a runner container is running in a slot.
func (p *Pool) slotHasRunner(ctx context.Context, i int) bool {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", LabelRole+"=runner"), filters.Arg("label", LabelSlot+"="+strconv.Itoa(i)))})
	return err != nil || len(list) > 0 // when unsure, do not kill anything
}
