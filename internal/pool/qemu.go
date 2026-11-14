package pool

import (
	"context"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// ensureQEMU makes multi-architecture builds work. Since Linux 6.7,
// binfmt_misc registrations belong to the user namespace and live only as long
// as the filesystem stays mounted somewhere. In unprivileged LXC containers
// nothing mounts it, so the registration made by setup-qemu-action vanishes
// as soon as its helper container exits. Pitlane keeps it mounted and
// registers the QEMU emulators once at start.
func (p *Pool) ensureQEMU(ctx context.Context) {
	if p.Config().NoQEMU {
		return
	}
	if _, err := os.Stat(binfmtDir + "/register"); err != nil {
		if err := syscall.Mount("binfmt_misc", binfmtDir, "binfmt_misc", 0, ""); err != nil {
			p.log.Info("binfmt_misc not mountable, multi-arch builds (QEMU) unavailable", "error", err)
			return
		}
	}
	const img = binfmtImage
	if err := p.EnsureImage(ctx, img, false); err != nil {
		p.log.Warn("QEMU emulators", "error", err)
		return
	}
	c, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: img, Cmd: []string{"--install", "all"}, Labels: map[string]string{LabelManaged: "true", LabelRole: "init"},
	}, &container.HostConfig{Privileged: true}, nil, nil, "")
	if err != nil {
		p.log.Warn("QEMU emulators", "error", err)
		return
	}
	defer p.Remove(context.Background(), c.ID)
	if err := p.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err == nil && p.Wait(ctx, c.ID) == 0 {
		base := trustedQEMU(readBinfmt())
		p.mu.Lock()
		p.binfmt = base
		p.mu.Unlock()
		p.saveBinfmt(base)
		p.log.Info("QEMU emulators registered: multi-arch builds are available", "handlers", len(base))
	}
}

const binfmtDir = "/proc/sys/fs/binfmt_misc"

// readBinfmt returns the registered binfmt_misc handlers (name -> definition).
func readBinfmt() map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(binfmtDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.Name() == "register" || e.Name() == "status" {
			continue
		}
		if b, err := os.ReadFile(binfmtDir + "/" + e.Name()); err == nil {
			out[e.Name()] = string(b)
		}
	}
	return out
}

// hostMachine returns the ELF e_machine of the host as hex bytes, the way it
// ends a binfmt_misc magic string.
func hostMachine() string {
	return map[string]string{"amd64": "3e00", "arm64": "b700", "386": "0300", "riscv64": "f300", "ppc64le": "1500", "s390x": "0016"}[runtime.GOARCH]
}

// trustedQEMU keeps only the handlers Pitlane itself registers: QEMU
// emulators whose interpreter is the static QEMU binary installed by binfmt.
func trustedQEMU(all map[string]string) map[string]string {
	// ELF e_machine of the host, as it appears at the end of a binfmt magic:
	// a handler matching native binaries would intercept every program.
	native := hostMachine()
	out := map[string]string{}
	for name, def := range all {
		if !strings.HasPrefix(name, "qemu-") || !strings.Contains(def, "interpreter /usr/bin/"+name+"\n") {
			continue
		}
		magic := ""
		for _, l := range strings.Split(def, "\n") {
			if v, ok := strings.CutPrefix(l, "magic "); ok {
				magic = v
			}
		}
		if magic == "" || (native != "" && strings.HasSuffix(magic, native)) {
			continue
		}
		out[name] = def
	}
	return out
}

// maybeRegisterQEMU registers the QEMU handlers when requested and no job
// runs. Slots are not handed out meanwhile, so no job can race the snapshot
// of what Pitlane registered.
func (p *Pool) maybeRegisterQEMU(ctx context.Context) {
	p.mu.Lock()
	idle := true
	for _, s := range p.slots {
		if s.Status == "busy" || s.Status == "cleaning" {
			idle = false
		}
	}
	if !p.qemuWant || !idle || p.cfg.NoQEMU {
		p.mu.Unlock()
		return
	}
	p.qemuBusy, p.qemuWant = true, false
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.qemuBusy = false
		p.mu.Unlock()
		p.signal()
	}()
	// Runner containers may exist that the slot table does not know about yet
	// (adopted after a restart): never register while any runner is alive.
	list, err := p.cli.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", LabelRole+"=runner"))})
	if err != nil || len(list) > 0 {
		p.mu.Lock()
		p.qemuWant = true
		p.mu.Unlock()
		return
	}
	// Remove whatever is there (nothing should be), then register.
	for name := range readBinfmt() {
		_ = os.WriteFile(binfmtDir+"/"+name, []byte("-1"), 0)
	}
	p.ensureQEMU(ctx)
}

// auditBinfmt runs after every job. Slot daemons are privileged, so a job
// could register its own binfmt_misc handler, which would then apply to every
// later job. Unknown or modified handlers are removed, and Pitlane's QEMU
// handlers are registered again if they were tampered with.
func (p *Pool) auditBinfmt(ctx context.Context) {
	if _, err := os.Stat(binfmtDir + "/register"); err != nil {
		return // not mounted: registrations cannot outlive the job's own mount
	}
	p.mu.Lock()
	base := p.binfmt
	p.mu.Unlock()
	if base == nil || p.Config().NoQEMU {
		base = map[string]string{} // fail closed: nothing is trusted
	}
	cur := readBinfmt()
	tampered := false
	for name, def := range cur {
		if want, ok := base[name]; ok && want == def {
			continue
		}
		p.log.Warn("removing unexpected binfmt_misc handler", "handler", name)
		_ = os.WriteFile(binfmtDir+"/"+name, []byte("-1"), 0)
		if _, ok := base[name]; ok {
			tampered = true
		}
	}
	for name := range base {
		if _, ok := cur[name]; !ok {
			tampered = true
		}
	}
	if tampered && len(base) > 0 {
		// Fail closed: drop every handler now; they are registered again
		// only once no job runs (registration must not race with jobs).
		p.log.Warn("QEMU handlers were modified by a job: removed, re-registering when idle")
		for name := range readBinfmt() {
			_ = os.WriteFile(binfmtDir+"/"+name, []byte("-1"), 0)
		}
		p.mu.Lock()
		p.binfmt = nil
		p.qemuWant = !p.cfg.NoQEMU
		p.mu.Unlock()
		p.saveBinfmt(nil)
	}
}
