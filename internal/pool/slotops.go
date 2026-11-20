package pool

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// slotOps is what slot wiping and cleanup need from Docker and the host.
// Tests use a fake implementation.
type slotOps interface {
	exec(ctx context.Context, container, script string) (string, int, error)
	auditBinfmt(ctx context.Context)
	// wipe deletes the slot container and its volumes from the host side
	// and recreates them empty.
	wipe(ctx context.Context, slot int) error
	// removeVolume deletes a Docker volume (nil when it does not exist).
	removeVolume(ctx context.Context, name string) error
	// recreate replaces the slot container (its volumes are kept).
	recreate(ctx context.Context, slot int) error
	// imageID returns the ID of a local image.
	imageID(ctx context.Context, ref string) (string, error)
	// copyExternals copies the runner image's externals into the slot's
	// externals volume, replacing its content.
	copyExternals(ctx context.Context, slot int, img string) error
	// slotStoreEmpty reports whether the slot has no pnpm store content yet.
	slotStoreEmpty(ctx context.Context, slot int) bool
	// mergeStore merges the slot's pnpm store with the canonical store vol.
	mergeStore(ctx context.Context, slot int, vol string) (mergeStats, error)
}

func (p *Pool) ops() slotOps {
	if p.sops != nil {
		return p.sops
	}
	return dockerSlotOps{p}
}

// dockerSlotOps is the real slotOps.
type dockerSlotOps struct{ p *Pool }

func (d dockerSlotOps) exec(ctx context.Context, container, script string) (string, int, error) {
	return d.p.exec(ctx, container, script)
}

func (d dockerSlotOps) auditBinfmt(ctx context.Context) { d.p.auditBinfmt(ctx) }

func (d dockerSlotOps) removeVolume(ctx context.Context, name string) error {
	if err := d.p.cli.VolumeRemove(ctx, name, true); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

func (d dockerSlotOps) wipe(ctx context.Context, slot int) error {
	if err := d.p.resetSlot(ctx, slot); err != nil {
		return err
	}
	return d.p.ensureSlot(ctx, slot)
}

func (d dockerSlotOps) recreate(ctx context.Context, slot int) error {
	_ = d.p.cli.ContainerRemove(ctx, slotName(slot), container.RemoveOptions{Force: true})
	return d.p.ensureSlot(ctx, slot)
}

func (d dockerSlotOps) imageID(ctx context.Context, ref string) (string, error) {
	info, err := d.p.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

func (d dockerSlotOps) copyExternals(ctx context.Context, i int, img string) error {
	p := d.p
	// Always a full copy: the volume is reachable from the privileged slot
	// daemon, so nothing inside it (such as a marker file) can be trusted.
	script := `find /x -mindepth 1 -delete && cp -a ` + externalsDir + `/. /x/ && ` + shimScript("/x", shimInContainer)
	c, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: img, User: "root", Entrypoint: []string{"sh", "-c", script},
		Labels: map[string]string{LabelManaged: "true", LabelRole: "init"},
	}, &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: slotExternals(i), Target: "/x"}}}, nil, nil, "")
	if err != nil {
		return err
	}
	defer p.Remove(context.WithoutCancel(ctx), c.ID)
	if err := p.stageShim(ctx, c.ID); err != nil {
		p.log.Warn("cannot install the Node shim: this slot's jobs use GitHub's actions cache", "slot", i, "error", err)
	}
	if err := p.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		return err
	}
	if code := p.Wait(ctx, c.ID); code != 0 {
		return fmt.Errorf("copy runner externals into slot %d: exit code %d", i, code)
	}
	return nil
}
