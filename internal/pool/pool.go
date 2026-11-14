// Package pool manages the Docker side of Pitlane: a fixed number of warm
// "slots" (one long-lived Docker-in-Docker daemon each, keeping images and
// build cache between jobs), the shared cache volumes and the ephemeral
// runner containers attached to those slots.
package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Labels put on every Docker object Pitlane owns.
const (
	LabelManaged = "dev.pitlane.managed"
	LabelRole    = "dev.pitlane.role"
	LabelSlot    = "dev.pitlane.slot"
	LabelTarget  = "dev.pitlane.target"
	LabelRunner  = "dev.pitlane.runner"
	LabelOwner   = "dev.pitlane.owner"
	// LabelStrict records on a runner container whether its job runs in
	// strict isolation ("true" or "false"), so a runner adopted after a
	// restart is released the same way. Missing (containers of older
	// versions) counts as strict: fail closed.
	LabelStrict = "dev.pitlane.strict"

	mirrorName    = "pitlane-mirror"
	mirrorNetwork = "pitlane-mirror"
	runnerUID     = "1001:1001"
	workDir       = "/home/runner/_work"
	// The slot daemon only listens on loopback: the runner shares the slot's
	// network namespace, nothing else can reach the Docker API.
	dindAddr = "tcp://127.0.0.1:2375"
	// Bumped whenever the slot container definition changes: older slots are
	// recreated (their volumes, hence their cache, are kept).
	slotSpecVersion = "3"
	LabelSpec       = "dev.pitlane.spec"
)

// Config tunes the pool. Every field can change at runtime via Configure.
type Config struct {
	Slots       int    // number of parallel jobs (warm dind daemons)
	RunnerImage string // image of the GitHub runner containers
	DindImage   string // image of the Docker-in-Docker daemons
	CPUShares   int64  // relative CPU weight per runner (0 = Docker default)
	MemoryMB    int64  // hard memory limit per runner and per slot daemon (0 = unlimited)
	CacheScope  string // "owner" (caches never cross GitHub owners) or "shared"
	Mirror      bool   // run a pull-through Docker Hub mirror
	KeepBuildGB int    // buildx cache kept per slot after each job
	ImageOS     string // value of ImageOS for setup-* actions
	StateFile   string // where slot ownership is persisted (outside any slot's reach)
	NoQEMU      bool   // disable multi-arch emulation (binfmt_misc + QEMU)
	// Minimum free disk space (GB) on Docker's filesystem: below it, idle
	// caches are pruned after each job (0 = never prune).
	MinFreeDiskGB int
}

func (c *Config) defaults() {
	if c.Slots <= 0 {
		c.Slots = 2
	}
	if c.RunnerImage == "" || isDefaultRunnerImage(c.RunnerImage) {
		c.RunnerImage = DefaultRunnerImage
	}
	if c.DindImage == "" || c.DindImage == legacyDindImage {
		c.DindImage = DefaultDindImage
	}
	if c.CacheScope == "" {
		c.CacheScope = "owner"
	}
	if c.KeepBuildGB <= 0 {
		c.KeepBuildGB = 20
	}
	if c.ImageOS == "" {
		c.ImageOS = "ubuntu24"
	}
}

// SlotState is a read-only snapshot of a slot.
type SlotState struct {
	Index    int    `json:"index"`
	Status   string `json:"status"` // starting, ready, busy, cleaning, error, retiring
	Owner    string `json:"owner"`
	TargetID int64  `json:"targetId"`
	Runner   string `json:"runner"`
	Error    string `json:"error,omitempty"`
	Since    int64  `json:"since"`
}

type slot struct {
	SlotState
	retire  bool
	upgrade bool // recreate the slot container once its current job is done
	// dirty marks a slot that may still hold another owner's data (a wipe or
	// rebuild failed). It is never handed out; the retry loop rebuilds it.
	dirty bool
}

// UnknownOwner marks a slot whose previous owner is not known (no or lost
// persisted state): it is wiped before its first job, whatever the cache
// scope, since it may hold a strict job's data. Parentheses never appear in
// GitHub logins.
const UnknownOwner = "(unknown)"

// StrictOwner marks a slot running a strict-isolation job: it is wiped before
// and after, and never trusted by anyone else.
const StrictOwner = "(strict)"

// persistedSlot is what survives a Pitlane restart. It lives in Pitlane's
// data directory, which jobs cannot reach, so it can be trusted.
type persistedSlot struct {
	Owner     string `json:"owner"`
	TargetID  int64  `json:"targetId"`
	Externals string `json:"externals"` // runner image ID copied into the externals volume
}

// Pool owns the slots.
type Pool struct {
	cli *client.Client
	log *slog.Logger

	mu        sync.Mutex
	cfg       Config
	slots     map[int]*slot
	chowned   map[string]bool
	externals map[int]string // slot -> runner image ID copied into its externals volume
	released  chan struct{}
	restored  map[int]persistedSlot
	binfmt    map[string]string // binfmt_misc entries registered by Pitlane (name -> content)
	qemuWant  bool              // QEMU should be (re)registered as soon as no job runs
	qemuBusy  bool              // registration in progress: no slot is handed out

	pruneMu sync.Mutex   // one disk reclaim at a time
	volMu   sync.RWMutex // cache volume removal (write) vs runner creation (read)

	stores storeSync // slot pnpm stores merged with their owner's canonical store

	sops slotOps // nil: Docker (tests set a fake)
}

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

// New connects to Docker and prepares the pool. Call Start to bring slots up.
func New(cfg Config, log *slog.Logger) (*Pool, error) {
	cfg.defaults()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	p := &Pool{cli: cli, log: log, cfg: cfg, slots: map[int]*slot{}, chowned: map[string]bool{}, externals: map[int]string{},
		restored: map[int]persistedSlot{}, released: make(chan struct{}, 1)}
	if b, err := os.ReadFile(cfg.StateFile); err == nil {
		if err := json.Unmarshal(b, &p.restored); err != nil {
			// Fail closed: every slot restores with an unknown owner.
			log.Error("slot state unreadable, every slot will be wiped before its first job", "file", cfg.StateFile, "error", err)
			p.restored = map[int]persistedSlot{}
		}
		if pruneRetired(p.restored, cfg.Slots, p.slots) > 0 {
			p.saveLocked()
		}
		for i, st := range p.restored {
			if st.Externals != "" {
				p.externals[i] = st.Externals
			}
		}
	}
	return p, nil
}

// pruneRetired drops the persisted state of slots beyond the configured
// count that no longer exist (left by a former, higher slot count) and
// returns how many were dropped. Forgetting a slot is fail closed: if it
// comes back, its owner is unknown and it is wiped before its first job.
func pruneRetired(state map[int]persistedSlot, slots int, live map[int]*slot) int {
	n := 0
	for i := range state {
		if i < slots || live[i] != nil {
			continue
		}
		delete(state, i)
		n++
	}
	return n
}

// saveLocked persists slot ownership. Callers hold p.mu.
func (p *Pool) saveLocked() {
	if p.cfg.StateFile == "" {
		return
	}
	// Start from the previous state so slots being retired keep their last
	// owner; those beyond the slot count are dropped once gone (pruneRetired).
	out := map[int]persistedSlot{}
	for i, st := range p.restored {
		out[i] = st
	}
	for i, s := range p.slots {
		out[i] = persistedSlot{Owner: s.Owner, TargetID: s.TargetID, Externals: p.externals[i]}
	}
	pruneRetired(out, p.cfg.Slots, p.slots)
	p.restored = out
	if err := writeState(p.cfg.StateFile, out); err != nil {
		// Fail closed: a stale file could restore an owner that no longer
		// matches the slot's data, and skip its wipe. Without a file every
		// slot restores with an unknown owner and is wiped.
		p.log.Error("cannot persist slot ownership, removing the state file: slots will be wiped after a restart", "file", p.cfg.StateFile, "error", err)
		if rerr := os.Remove(p.cfg.StateFile); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			p.log.Error("cannot remove the stale slot state file: remove it before restarting Pitlane", "file", p.cfg.StateFile, "error", rerr)
		}
	}
}

// writeState atomically replaces the state file.
func writeState(file string, state map[int]persistedSlot) error { return writeJSONFile(file, state) }

// writeJSONFile atomically replaces a 0600 JSON file.
func writeJSONFile(file string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, file)
}

// Docker exposes the Docker client (used for logs).
func (p *Pool) Docker() *client.Client { return p.cli }

// Config returns the current configuration.
func (p *Pool) Config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// Released is signalled whenever a slot becomes free.
func (p *Pool) Released() <-chan struct{} { return p.released }

func (p *Pool) signal() {
	select {
	case p.released <- struct{}{}:
	default:
	}
}

// Start pulls images, starts the mirror and every slot. Slots that already
// exist (e.g. after a Pitlane restart) are reused with their warm cache.
func (p *Pool) Start(ctx context.Context) error {
	cfg := p.Config()
	if err := p.EnsureImage(ctx, cfg.DindImage, false); err != nil {
		return err
	}
	if err := p.EnsureImage(ctx, cfg.RunnerImage, false); err != nil {
		if cfg.RunnerImage != DefaultRunnerImage {
			return err
		}
		p.log.Warn("Pitlane runner image unavailable, using GitHub's official image", "error", err)
		cfg.RunnerImage = OfficialRunnerImage
		if err := p.EnsureImage(ctx, cfg.RunnerImage, false); err != nil {
			return err
		}
		p.mu.Lock()
		p.cfg.RunnerImage = OfficialRunnerImage
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.qemuWant = !cfg.NoQEMU
	p.mu.Unlock()
	p.restoreBinfmt(ctx)
	if err := p.ensureMirror(ctx, cfg); err != nil {
		p.log.Warn("registry mirror unavailable, continuing without it", "error", err)
	}
	go p.retryFailed(ctx)
	return p.Configure(ctx, cfg)
}

// retryFailed restarts slots in error every 30 seconds.
func (p *Pool) retryFailed(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.auditBinfmt(ctx)
		p.maybeRegisterQEMU(ctx)
		p.mu.Lock()
		var failed, dirty []int
		for i, s := range p.slots {
			if s.Status == "error" && !s.retire {
				s.Status = "starting"
				if s.dirty {
					dirty = append(dirty, i)
				} else {
					failed = append(failed, i)
				}
			}
		}
		p.mu.Unlock()
		for _, i := range failed {
			p.log.Info("retrying slot", "slot", i)
			go p.startSlot(ctx, i)
		}
		for _, i := range dirty {
			p.log.Info("retrying slot rebuild", "slot", i)
			go p.rebuildSlot(ctx, i)
		}
	}
}

// Configure applies a new configuration: slots are added or retired, other
// settings apply to the next runners.
func (p *Pool) Configure(ctx context.Context, cfg Config) error {
	cfg.defaults()
	p.mu.Lock()
	old := p.cfg
	p.cfg = cfg
	var toStart []int
	for i := 0; i < cfg.Slots; i++ {
		if s, ok := p.slots[i]; ok {
			s.retire = false
			continue
		}
		st, known := p.restored[i]
		if !known {
			// Its volumes may hold data from an owner we do not know about:
			// fail closed, the first job wipes it.
			st.Owner = UnknownOwner
		}
		p.slots[i] = &slot{SlotState: SlotState{Index: i, Status: "starting", Since: time.Now().Unix(), Owner: st.Owner, TargetID: st.TargetID}}
		toStart = append(toStart, i)
	}
	var toRemove []int
	for i, s := range p.slots {
		if i >= cfg.Slots {
			s.retire = true
			if s.Status != "busy" && s.Status != "cleaning" {
				toRemove = append(toRemove, i)
			}
		}
	}
	p.mu.Unlock()

	if old.RunnerImage != cfg.RunnerImage {
		go func() {
			if err := p.EnsureImage(context.Background(), cfg.RunnerImage, false); err != nil {
				p.log.Error("pull runner image", "error", err)
			}
		}()
	}
	if cfg.Mirror && !old.Mirror {
		if err := p.ensureMirror(ctx, cfg); err != nil {
			p.log.Warn("registry mirror", "error", err)
		}
	}
	for _, i := range toRemove {
		p.removeSlot(ctx, i)
	}
	var wg sync.WaitGroup
	for _, i := range toStart {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p.startSlot(ctx, i)
		}(i)
	}
	wg.Wait()
	return nil
}

func slotName(i int) string    { return fmt.Sprintf("pitlane-slot-%d", i) }
func slotNetwork(i int) string { return fmt.Sprintf("pitlane-slot-%d", i) }
func slotDocker(i int) string  { return fmt.Sprintf("pitlane-slot-%d-docker", i) }
func slotWork(i int) string    { return fmt.Sprintf("pitlane-slot-%d-work", i) }

// slotExternals holds a copy of the runner's /home/runner/externals (the Node
// runtimes used by JavaScript actions). Job containers (`container:`) get it
// mounted at /__e, so it must exist on the slot daemon's side too.
func slotExternals(i int) string { return fmt.Sprintf("pitlane-slot-%d-externals", i) }

const externalsDir = "/home/runner/externals"

// pnpmStoreDir lives in the slot work volume (kept between jobs of the same
// owner, wiped on owner change and for strict jobs).
const pnpmStoreDir = ".pnpm-store"

func (p *Pool) setStatus(i int, status, errMsg string) {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.Status, s.Error, s.Since = status, errMsg, time.Now().Unix()
		if status == "ready" {
			s.Runner = ""
		}
	}
	p.mu.Unlock()
	if status == "ready" {
		p.signal()
	}
}

func (p *Pool) startSlot(ctx context.Context, i int) {
	if err := p.ensureSlot(ctx, i); err != nil {
		p.log.Error("slot failed to start", "slot", i, "error", err)
		p.setStatus(i, "error", err.Error())
		return
	}
	p.mu.Lock()
	s := p.slots[i]
	busy := s != nil && s.Status == "busy"
	dirty := s != nil && s.dirty
	p.mu.Unlock()
	if dirty {
		p.setStatus(i, "error", "slot must be rebuilt before reuse")
		return
	}
	if !busy {
		p.setStatus(i, "ready", "")
	}
	p.log.Info("slot ready", "slot", i)
}

func (p *Pool) ensureSlot(ctx context.Context, i int) error {
	cfg := p.Config()
	if err := p.ensureNetwork(ctx, slotNetwork(i), false); err != nil {
		return err
	}
	for _, v := range []string{slotDocker(i), slotWork(i), slotExternals(i)} {
		if err := p.ensureVolume(ctx, v); err != nil {
			return err
		}
	}
	name := slotName(i)
	info, err := p.cli.ContainerInspect(ctx, name)
	switch {
	case cerrdefs.IsNotFound(err):
		cmd := []string{"--host=" + dindAddr, "--host=unix:///var/run/docker.sock", "--tls=false"}
		if cfg.Mirror {
			cmd = append(cmd, "--registry-mirror=http://"+mirrorName+":5000")
		}
		labels := map[string]string{LabelManaged: "true", LabelRole: "dind", LabelSlot: strconv.Itoa(i), LabelSpec: slotSpecVersion}
		hc := &container.HostConfig{
			Privileged:    true,
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: slotDocker(i), Target: "/var/lib/docker"},
				{Type: mount.TypeVolume, Source: slotWork(i), Target: workDir},
				{Type: mount.TypeVolume, Source: slotExternals(i), Target: externalsDir, ReadOnly: true},
			},
		}
		if cfg.MemoryMB > 0 {
			hc.Memory = cfg.MemoryMB * 1024 * 1024
		}
		_, err = p.cli.ContainerCreate(ctx, &container.Config{
			Image: cfg.DindImage,
			// Skip dockerd-entrypoint.sh: it would add its own TCP listener on
			// 0.0.0.0. The "dind" wrapper only prepares cgroups and mounts.
			Entrypoint: []string{"dind", "dockerd"},
			Cmd:        cmd,
			Env:        []string{"DOCKER_TLS_CERTDIR="},
			Labels:     labels,
			Hostname:   name,
		}, hc, &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{slotNetwork(i): {Aliases: []string{name}}}}, nil, name)
		if err != nil {
			return fmt.Errorf("create dind: %w", err)
		}
	case err != nil:
		return err
	case info.Config.Labels[LabelSpec] != slotSpecVersion && p.slotHasRunner(ctx, i):
		// Outdated, but a job is running in it: upgrade once the job is done.
		p.mu.Lock()
		if s, ok := p.slots[i]; ok {
			s.upgrade = true
		}
		p.mu.Unlock()
		return p.waitDind(ctx, name)
	case info.Config.Labels[LabelSpec] != slotSpecVersion:
		// Slot created by an older Pitlane: recreate it, its volumes (and
		// cache) are kept.
		p.log.Info("recreating slot with the current definition", "slot", i)
		if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil {
			return err
		}
		return p.ensureSlot(ctx, i)
	case info.State != nil && info.State.Running:
		if cfg.Mirror {
			p.connectMirror(ctx, i)
		}
		return p.waitDind(ctx, name)
	}
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
	_, _, err = p.exec(ctx, name, "chown "+runnerUID+" "+workDir)
	return err
}

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

// connectMirror attaches the mirror to the slot's own network so slots never
// share a network with each other.
func (p *Pool) connectMirror(ctx context.Context, i int) {
	_ = p.cli.NetworkDisconnect(ctx, mirrorNetwork, slotName(i), true) // layout of older versions
	err := p.cli.NetworkConnect(ctx, slotNetwork(i), mirrorName, &network.EndpointSettings{Aliases: []string{mirrorName}})
	if err != nil && !cerrdefs.IsConflict(err) && !strings.Contains(err.Error(), "already exists") {
		p.log.Warn("connect mirror to slot network", "slot", i, "error", err)
	}
}

func (p *Pool) waitDind(ctx context.Context, name string) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, code, err := p.exec(ctx, name, "docker info >/dev/null 2>&1"); err == nil && code == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("docker daemon in " + name + " did not become ready")
}

func (p *Pool) removeSlot(ctx context.Context, i int) {
	p.log.Info("retiring slot", "slot", i)
	p.forgetStore(i)
	_ = p.cli.ContainerRemove(ctx, slotName(i), container.RemoveOptions{Force: true, RemoveVolumes: false})
	p.mu.Lock()
	p.saveLocked() // record its current owner while it is still live: its volumes are kept
	delete(p.slots, i)
	delete(p.externals, i)
	p.saveLocked() // now gone: forgotten if beyond the slot count (fail closed)
	p.mu.Unlock()
}

// rebuildSlot deletes a slot with all its volumes and starts it again empty.
func (p *Pool) rebuildSlot(ctx context.Context, i int) {
	if err := p.resetSlot(ctx, i); err != nil {
		p.setStatus(i, "error", "rebuild: "+err.Error())
		return
	}
	p.startSlot(ctx, i)
}

// resetSlot deletes a slot's container and volumes through the host's Docker
// API, never trusting anything inside the slot: its jobs were root on its
// daemon and could have tampered with its filesystem or left processes. The
// slot stays dirty (with its previous owner) until everything is gone; it
// then has no owner. The slot container is not recreated.
func (p *Pool) resetSlot(ctx context.Context, i int) error {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.dirty = true // cleared only once everything is really gone
	}
	p.mu.Unlock()
	p.forgetStore(i)
	err := p.cli.ContainerRemove(ctx, slotName(i), container.RemoveOptions{Force: true})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	if err := p.DropSlotCache(ctx, i); err != nil {
		// Keep the previous owner: the slot will be wiped again before any
		// other owner can get it.
		return err
	}
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.Owner, s.TargetID, s.dirty = "", 0, false
	}
	delete(p.chowned, slotDocker(i))
	delete(p.chowned, slotWork(i))
	delete(p.chowned, slotExternals(i))
	delete(p.externals, i)
	p.saveLocked()
	p.mu.Unlock()
	return nil
}

// DropSlotCache deletes a retired slot's volumes (frees disk).
func (p *Pool) DropSlotCache(ctx context.Context, i int) error {
	for _, v := range []string{slotDocker(i), slotWork(i), slotExternals(i)} {
		if err := p.cli.VolumeRemove(ctx, v, true); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove volume %s: %w", v, err)
		}
	}
	return nil
}

// Lease is a slot reserved for one runner.
type Lease struct {
	Slot     int
	Owner    string
	TargetID int64
	Strict   bool   // strict isolation: fresh caches, slot wiped before and after
	Runner   string // set by the scaler once the runner name is known
	wipe     bool   // the slot last served another owner: Prepare wipes it
}

// Acquire reserves a free slot, preferring one that already ran jobs for the
// same target, then the same owner (warmest cache). It returns false when
// every slot is busy.
func (p *Pool) Acquire(ctx context.Context, owner string, targetID int64, strict bool) (Lease, bool) {
	if strict {
		owner = StrictOwner
	}
	p.mu.Lock()
	if p.qemuBusy {
		p.mu.Unlock()
		return Lease{}, false
	}
	var best *slot
	score := -1
	for _, s := range p.slots {
		if s.Status != "ready" || s.retire || s.dirty {
			continue
		}
		sc := 0
		switch {
		case strict && s.Owner == "":
			sc = 3 // already clean: no wipe needed
		case strict:
			sc = 0
		case s.TargetID == targetID:
			sc = 3
		case s.Owner == owner:
			sc = 2
		case s.Owner == "":
			sc = 1
		}
		if sc > score || (sc == score && s.Index < best.Index) {
			best, score = s, sc
		}
	}
	if best == nil {
		p.mu.Unlock()
		return Lease{}, false
	}
	// The shared scope lets owners share caches, but a slot left by a strict
	// job, or whose last owner is unknown (it may have been a strict job), is
	// always wiped.
	sticky := best.Owner == StrictOwner || best.Owner == UnknownOwner
	needWipe := (p.cfg.CacheScope == "owner" || strict || sticky) && best.Owner != "" && (best.Owner != owner || strict)
	best.Status, best.Since = "busy", time.Now().Unix()
	if !needWipe && (best.Owner != owner || best.TargetID != targetID) {
		best.Owner, best.TargetID = owner, targetID
		p.saveLocked()
	}
	idx := best.Index
	p.mu.Unlock()
	return Lease{Slot: idx, Owner: owner, TargetID: targetID, Strict: strict, wipe: needWipe}, true
}

// ErrSlotRebuilding means Prepare failed to wipe the slot; it is left dirty
// for the retry loop to rebuild and must not be released.
var ErrSlotRebuilding = errors.New("slot wipe failed, slot is being rebuilt")

// wipeFailed takes a slot whose wipe failed out of service: dirty (its
// previous owner is kept) and in error, so the retry loop rebuilds it.
func (p *Pool) wipeFailed(i int, err error) {
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.dirty = true
	}
	p.mu.Unlock()
	p.setStatus(i, "error", "wipe failed, slot will be rebuilt: "+err.Error())
}

// Prepare readies a reserved slot for a runner: it wipes the slot when it
// last served another owner and refreshes the runner externals if needed.
func (p *Pool) Prepare(ctx context.Context, l Lease) error {
	if l.wipe {
		// The slot is rebuilt from the host side (a fresh daemon start, about
		// 15 s): nothing inside it is trusted, and Release may not have
		// emptied the previous job's workspace (failed cleanup, Pitlane
		// stopped between the job's end and its release). The previous
		// owner's caches are useless to the new one anyway.
		p.log.Info("slot changes owner, rebuilding it", "slot", l.Slot, "owner", l.Owner)
		if err := p.ops().wipe(ctx, l.Slot); err != nil {
			// Never hand out a slot that may still hold another owner's data.
			p.log.Error("slot wipe failed, it will be rebuilt", "slot", l.Slot, "error", err)
			p.wipeFailed(l.Slot, err)
			return ErrSlotRebuilding
		}
		p.mu.Lock()
		if s, ok := p.slots[l.Slot]; ok {
			s.Owner, s.TargetID = l.Owner, l.TargetID
		}
		delete(p.externals, l.Slot) // re-copied from the image below
		p.saveLocked()
		p.mu.Unlock()
	}
	if err := p.ensureExternals(ctx, l.Slot, p.Config().RunnerImage); err != nil {
		return err
	}
	if l.Strict {
		p.forgetStore(l.Slot)
	} else if !p.stores.isSynced(l.Slot, pnpmStoreVolume(p.Config().CacheScope, l.Owner)) && p.slotStoreEmpty(ctx, l.Slot) {
		// New or wiped slot: fill its pnpm store from the owner's canonical
		// one before the job installs (a store left by an earlier job of
		// the same owner is merged after this job instead, off the clock).
		p.syncPnpmStore(ctx, l.Slot, l.Owner)
	}
	return nil
}

// strictVolumes returns the throwaway cache volumes of a strict runner.
func strictVolumes(runner string) (cache, tools string) {
	return "pitlane-strict-" + runner + "-cache", "pitlane-strict-" + runner + "-tools"
}

// releaseStrict wipes everything a strict job could have touched: the slot
// is rebuilt from the host side, its throwaway caches removed. On any
// failure it stays dirty and is rebuilt rather than reused.
func (p *Pool) releaseStrict(ctx context.Context, l Lease) {
	p.setStatus(l.Slot, "cleaning", "")
	err := p.ops().wipe(ctx, l.Slot)
	if l.Runner != "" {
		c, t := strictVolumes(l.Runner)
		for _, v := range []string{c, t} {
			if rerr := p.ops().removeVolume(ctx, v); rerr != nil && err == nil {
				err = rerr
			}
			p.mu.Lock()
			delete(p.chowned, v)
			p.mu.Unlock()
		}
	}
	p.ops().auditBinfmt(ctx)
	if err != nil {
		p.log.Error("strict cleanup failed, the slot will be rebuilt", "slot", l.Slot, "error", err)
		p.wipeFailed(l.Slot, err)
		return
	}
	p.mu.Lock()
	if s, ok := p.slots[l.Slot]; ok {
		s.Owner, s.TargetID = "", 0
	}
	delete(p.externals, l.Slot) // the privileged job could have altered it
	p.saveLocked()
	retire := p.slots[l.Slot] != nil && p.slots[l.Slot].retire
	p.mu.Unlock()
	if retire {
		p.removeSlot(ctx, l.Slot)
		p.signal()
		return
	}
	p.setStatus(l.Slot, "ready", "")
}

// clearDir returns a command removing everything in dir (hidden files
// included) except the top-level entries named in keep.
func clearDir(dir string, keep ...string) string {
	cmd := "find " + dir + " -mindepth 1 -maxdepth 1"
	for _, k := range keep {
		cmd += " ! -name " + k
	}
	return cmd + " -exec rm -rf {} +"
}

// releaseScript cleans what a job left in its slot while keeping the owner's
// caches (images, build cache, actions, pnpm store). The daemon prunes are
// best effort; the script fails when the workspace cannot be emptied or a
// container of the job survives, and the slot is then never reused as is.
func releaseScript(keepBuildGB int) string {
	return fmt.Sprintf(`docker ps -aq | xargs -r docker rm -f >/dev/null 2>&1
docker network prune -f >/dev/null 2>&1
docker volume prune -af >/dev/null 2>&1
docker image prune -f >/dev/null 2>&1
docker builder prune -f --keep-storage %dgb >/dev/null 2>&1
%s &&
chown %s %s &&
left=$(docker ps -aq) && test -z "$left"`, keepBuildGB, clearDir(workDir, "_actions", pnpmStoreDir), runnerUID, workDir)
}

// Release cleans what the job left in the slot (containers, networks,
// volumes, workspace) while keeping images and build cache, then frees it.
// Fail closed: when the cleanup fails the slot is marked dirty, so it is never
// handed out again until it has been rebuilt from scratch.
func (p *Pool) Release(ctx context.Context, l Lease) {
	if l.Strict {
		p.releaseStrict(ctx, l)
		return
	}
	p.setStatus(l.Slot, "cleaning", "")
	cfg := p.Config()
	_, code, err := p.ops().exec(ctx, slotName(l.Slot), releaseScript(cfg.KeepBuildGB))
	failed := err != nil || code != 0
	if failed {
		p.log.Error("slot cleanup failed, rebuilding the slot before any reuse", "slot", l.Slot, "code", code, "error", err)
		p.mu.Lock()
		if s, ok := p.slots[l.Slot]; ok {
			s.dirty = true // previous owner is kept
		}
		p.mu.Unlock()
	}
	p.ops().auditBinfmt(ctx)
	p.mu.Lock()
	s, ok := p.slots[l.Slot]
	owned := ok && !s.dirty && s.Owner == l.Owner
	p.mu.Unlock()
	if owned {
		// The job's containers are gone: publish the packages it downloaded
		// and fill the slot with the others for the next job.
		p.syncPnpmStore(ctx, l.Slot, l.Owner)
	}
	p.mu.Lock()
	s, ok = p.slots[l.Slot]
	retire := ok && s.retire
	upgrade := ok && s.upgrade
	if upgrade {
		s.upgrade = false
	}
	dirty := ok && s.dirty
	p.mu.Unlock()
	if retire {
		p.removeSlot(ctx, l.Slot)
		p.signal()
		return
	}
	if dirty {
		// The retry loop rebuilds dirty slots in error (a rebuild recreates
		// the container, which also covers a pending upgrade).
		msg := "cleanup failed, slot will be rebuilt"
		if err != nil {
			msg += ": " + err.Error()
		}
		p.setStatus(l.Slot, "error", msg)
		return
	}
	if upgrade {
		p.log.Info("upgrading slot after its job", "slot", l.Slot)
		_ = p.cli.ContainerRemove(ctx, slotName(l.Slot), container.RemoveOptions{Force: true})
		if err := p.ensureSlot(ctx, l.Slot); err != nil {
			p.setStatus(l.Slot, "error", err.Error())
			return
		}
	}
	p.setStatus(l.Slot, "ready", "")
}

var externalsCopies = make(chan struct{}, 2)

// slotHasRunner reports whether a runner container is running in a slot.
func (p *Pool) slotHasRunner(ctx context.Context, i int) bool {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(
		filters.Arg("label", LabelRole+"=runner"), filters.Arg("label", LabelSlot+"="+strconv.Itoa(i)))})
	return err != nil || len(list) > 0 // when unsure, do not kill anything
}

// ensureExternals copies the runner image's externals into the slot volume.
// The copy is redone when the runner image or the slot owner changes (tracked
// in Pitlane's state file, never in the volume, which slot jobs can reach);
// between jobs of the same owner it is shared like the other caches.
func (p *Pool) ensureExternals(ctx context.Context, i int, img string) error {
	info, err := p.cli.ImageInspect(ctx, img)
	if err != nil {
		return err
	}
	p.mu.Lock()
	done := p.externals[i] == info.ID
	p.mu.Unlock()
	if done {
		return nil
	}
	// Copies are disk-heavy (~600 MB): never run many at once.
	select {
	case externalsCopies <- struct{}{}:
		defer func() { <-externalsCopies }()
	case <-ctx.Done():
		return ctx.Err()
	}
	p.log.Info("copying runner externals into slot", "slot", i)
	// Always a full copy: the volume is reachable from the privileged slot
	// daemon, so nothing inside it (such as a marker file) can be trusted.
	script := `find /x -mindepth 1 -delete && cp -a ` + externalsDir + `/. /x/`
	c, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: img, User: "root", Entrypoint: []string{"sh", "-c", script},
		Labels: map[string]string{LabelManaged: "true", LabelRole: "init"},
	}, &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: slotExternals(i), Target: "/x"}}}, nil, nil, "")
	if err != nil {
		return err
	}
	defer p.Remove(context.Background(), c.ID)
	if err := p.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
		return err
	}
	if code := p.Wait(ctx, c.ID); code != 0 {
		return fmt.Errorf("copy runner externals into slot %d: exit code %d", i, code)
	}
	p.mu.Lock()
	p.externals[i] = info.ID
	p.saveLocked()
	p.mu.Unlock()
	return nil
}

// MarkRunner records which runner occupies a slot (for the UI).
func (p *Pool) MarkRunner(slotIdx int, runner string) {
	p.mu.Lock()
	if s, ok := p.slots[slotIdx]; ok {
		s.Runner = runner
	}
	p.mu.Unlock()
}

// Slots returns a snapshot of every slot.
func (p *Pool) Slots() []SlotState {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]SlotState, 0, len(p.slots))
	for i := 0; i < len(p.slots)+8; i++ {
		if s, ok := p.slots[i]; ok {
			st := s.SlotState
			if s.retire {
				st.Status = "retiring"
			}
			out = append(out, st)
		}
	}
	return out
}

// FreeSlots counts ready slots.
func (p *Pool) FreeSlots() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.slots {
		if s.Status == "ready" && !s.retire {
			n++
		}
	}
	return n
}

// RunnerSpec describes a runner container to start.
type RunnerSpec struct {
	Name      string
	JITConfig string
	TargetID  int64
	Owner     string
	Lease     Lease
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func cacheVolume(scope, owner string) string {
	if scope == "shared" {
		return "pitlane-cache"
	}
	return "pitlane-cache-" + strings.ToLower(unsafeChars.ReplaceAllString(owner, "_"))
}

// runnerLogConfig bounds what Docker keeps of a runner's output (a job
// controls it): at most two 50 MB files, whatever the daemon's default.
func runnerLogConfig() container.LogConfig {
	return container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "50m", "max-file": "2"}}
}

// StartRunner creates and starts an ephemeral runner container in a slot.
func (p *Pool) StartRunner(ctx context.Context, spec RunnerSpec) (string, error) {
	// Cache volumes must not be pruned between their creation and their use.
	p.volMu.RLock()
	defer p.volMu.RUnlock()
	cfg := p.Config()
	cacheVol := cacheVolume(cfg.CacheScope, spec.Owner)
	toolVol := strings.Replace(cacheVol, "pitlane-cache", "pitlane-toolcache", 1)
	if spec.Lease.Strict {
		// Throwaway caches, removed when the job ends.
		cacheVol, toolVol = strictVolumes(spec.Name)
	}
	for _, v := range []string{cacheVol, toolVol} {
		if err := p.ensureVolume(ctx, v); err != nil {
			return "", err
		}
	}
	slotIdx := spec.Lease.Slot
	env := []string{
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + spec.JITConfig,
		"DOCKER_HOST=tcp://127.0.0.1:2375",
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache",
		"AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache",
		"ImageOS=" + cfg.ImageOS,
		"XDG_CACHE_HOME=/home/runner/.cache",
		"npm_config_cache=/home/runner/.cache/npm",
		// Same filesystem as the workspace: pnpm hardlinks packages into
		// node_modules instead of copying them (minutes saved per install).
		"npm_config_store_dir=" + workDir + "/" + pnpmStoreDir,
		"YARN_CACHE_FOLDER=/home/runner/.cache/yarn",
		"BUN_INSTALL_CACHE_DIR=/home/runner/.cache/bun",
		"GOMODCACHE=/home/runner/.cache/go-mod",
		"PIP_CACHE_DIR=/home/runner/.cache/pip",
		"PITLANE_SLOT=" + strconv.Itoa(slotIdx),
		// The official image dumps the runner's internal debug traces to stdout
		// (meant for Kubernetes); keep the runner output readable instead.
		"ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=0",
	}
	hc := &container.HostConfig{
		Init: ptr(true),
		// Share the slot daemon's network namespace (like a Kubernetes pod):
		// ports published by `services:` are reachable on localhost.
		NetworkMode: container.NetworkMode("container:" + slotName(slotIdx)),
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: slotWork(slotIdx), Target: workDir},
			{Type: mount.TypeVolume, Source: slotExternals(slotIdx), Target: externalsDir, ReadOnly: true},
			{Type: mount.TypeVolume, Source: toolVol, Target: "/opt/hostedtoolcache"},
			{Type: mount.TypeVolume, Source: cacheVol, Target: "/home/runner/.cache"},
		},
	}
	hc.LogConfig = runnerLogConfig()
	hc.CPUShares = cfg.CPUShares
	if cfg.MemoryMB > 0 {
		hc.Memory = cfg.MemoryMB * 1024 * 1024
	}
	created, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: cfg.RunnerImage,
		User:  "runner",
		Cmd:   []string{"/home/runner/run.sh"},
		Env:   env,
		Labels: map[string]string{
			LabelManaged: "true", LabelRole: "runner", LabelSlot: strconv.Itoa(slotIdx),
			LabelTarget: strconv.FormatInt(spec.TargetID, 10), LabelRunner: spec.Name, LabelOwner: spec.Owner,
			LabelStrict: strconv.FormatBool(spec.Lease.Strict),
		},
	}, hc, nil, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("create runner: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = p.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		return "", fmt.Errorf("start runner: %w", err)
	}
	p.MarkRunner(slotIdx, spec.Name)
	return created.ID, nil
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

// Logs copies a container's combined output to w.
func (p *Pool) Logs(ctx context.Context, id string, follow bool, w io.Writer) error {
	rc, err := p.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: follow, Timestamps: false})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = stdcopy.StdCopy(w, w, rc)
	return err
}

// Remove deletes a container.
func (p *Pool) Remove(ctx context.Context, id string) {
	_ = p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

// StopRunner stops a runner container; whoever waits on it then cleans up.
// SIGTERM comes first: the runner then cancels its job and reports it to
// GitHub at once (after a SIGKILL, GitHub keeps the job "in progress" for
// about 10 minutes, until it gives up on the lost runner). SIGKILL follows
// after grace. It blocks up to grace.
func (p *Pool) StopRunner(ctx context.Context, id string, grace time.Duration) error {
	secs := int(grace / time.Second)
	err := p.cli.ContainerStop(ctx, id, container.StopOptions{Signal: "SIGTERM", Timeout: &secs})
	if cerrdefs.IsNotFound(err) || cerrdefs.IsConflict(err) { // gone or already stopped
		return nil
	}
	return err
}

// Orphan is a runner container found at startup.
type Orphan struct {
	ID       string
	Name     string
	Slot     int
	TargetID int64
	Owner    string // GitHub owner of the target
	Strict   bool   // strict isolation (also for containers without the label)
	Running  bool
	Created  time.Time
}

// Lease returns the lease of the slot an adopted runner holds, so that its
// release goes through the same path as if Pitlane had not restarted: a
// strict runner's slot is rebuilt and its throwaway caches removed.
func (o Orphan) Lease() Lease {
	l := Lease{Slot: o.Slot, Owner: o.Owner, TargetID: o.TargetID, Strict: o.Strict, Runner: o.Name}
	if o.Strict {
		l.Owner = StrictOwner
	}
	return l
}

// RunnerContainers lists runner containers left by a previous run and marks
// their slots as busy so they are not handed out twice.
func (p *Pool) RunnerContainers(ctx context.Context) ([]Orphan, error) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"), filters.Arg("label", LabelRole+"=runner"))})
	if err != nil {
		return nil, err
	}
	var out []Orphan
	for _, c := range list {
		out = append(out, p.adoptContainer(c.ID, c.Labels, c.State, c.Created))
	}
	return out, nil
}

// adoptContainer marks the slot of a runner container left by a previous
// run as busy and returns it. A strict runner's slot gets the strict owner
// back (the label carries the target's real owner): nobody else may get the
// slot without a rebuild.
func (p *Pool) adoptContainer(id string, labels map[string]string, state string, created int64) Orphan {
	slotIdx, _ := strconv.Atoi(labels[LabelSlot])
	tid, _ := strconv.ParseInt(labels[LabelTarget], 10, 64)
	o := Orphan{ID: id, Name: labels[LabelRunner], Slot: slotIdx, TargetID: tid, Owner: labels[LabelOwner],
		Strict: labels[LabelStrict] != "false", Running: state == "running", Created: time.Unix(created, 0)}
	owner := o.Lease().Owner
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.slots[slotIdx]; ok {
		s.Status, s.Owner, s.TargetID, s.Runner = "busy", owner, tid, o.Name
	} else {
		p.slots[slotIdx] = &slot{SlotState: SlotState{Index: slotIdx, Status: "busy", Owner: owner, TargetID: tid, Runner: o.Name, Since: time.Now().Unix()}}
	}
	return o
}

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

// ensureVolume creates a volume and makes its root writable by the runner user.
func (p *Pool) ensureVolume(ctx context.Context, name string) error {
	p.mu.Lock()
	done := p.chowned[name]
	p.mu.Unlock()
	if done {
		return nil
	}
	if _, err := p.cli.VolumeInspect(ctx, name); err != nil {
		if _, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{LabelManaged: "true"}}); err != nil {
			return fmt.Errorf("create volume %s: %w", name, err)
		}
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

// TailJobOutput streams the step output of a running job. The runner writes
// it to _diag/pages/*.log (4 KB pages) before uploading it to GitHub; pages
// are read as they grow, oldest first, until the runner exits.
func (p *Pool) TailJobOutput(ctx context.Context, id string, w io.Writer) error {
	const (
		dir      = "/home/runner/_diag/pages"
		chunkMax = 64 << 10 // bytes read per file and per poll
		totalMax = 64 << 20 // bytes streamed per request
	)
	offsets := map[string]int{}
	total := 0
	for {
		// Regular files only (no symlinks), oldest first.
		listing, code, err := p.exec(ctx, id, `cd `+dir+` 2>/dev/null && find . -maxdepth 1 -type f -name '*.log' -printf '%T@ %s %f\n' | sort -n | head -n 500`)
		if err != nil || code != 0 {
			if info, ierr := p.cli.ContainerInspect(ctx, id); ierr != nil || info.State == nil || !info.State.Running {
				return nil // runner finished
			}
		}
		for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
			f := strings.Fields(line)
			if len(f) != 3 || !strings.HasSuffix(f[2], ".log") || strings.ContainsAny(f[2], "/'\"$`\\") {
				continue
			}
			name := f[2]
			size, _ := strconv.Atoi(f[1])
			off := offsets[name]
			if size <= off {
				continue
			}
			n := min(size-off, chunkMax)
			chunk, _, err := p.exec(ctx, id, fmt.Sprintf("tail -c +%d '%s/%s' | head -c %d", off+1, dir, name, n))
			if err != nil {
				continue
			}
			if len(chunk) > n {
				chunk = chunk[:n]
			}
			offsets[name] = off + len(chunk)
			total += len(chunk)
			if _, err := io.WriteString(w, chunk); err != nil {
				return err
			}
			if total >= totalMax {
				_, _ = io.WriteString(w, "\n[pitlane] output truncated: open the GitHub run page for the full log\n")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// execOutputMax bounds what exec keeps of a script's output: some of these
// containers are controlled by jobs.
const execOutputMax = 4 << 20

// exec runs a shell script inside a container and returns its output.
func (p *Pool) exec(ctx context.Context, name, script string) (string, int, error) {
	ex, err := p.cli.ContainerExecCreate(ctx, name, container.ExecOptions{Cmd: []string{"sh", "-c", script}, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return "", -1, err
	}
	att, err := p.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", -1, err
	}
	defer att.Close()
	// Read to the end (output beyond the cap is discarded): stopping early
	// would leave the process running and its exit code unknown.
	out := &capWriter{max: execOutputMax}
	if _, err := stdcopy.StdCopy(out, out, att.Reader); err != nil {
		return out.String(), -1, fmt.Errorf("read exec output: %w", err)
	}
	code, err := waitExec(ctx, func(ctx context.Context) (container.ExecInspect, error) {
		return p.cli.ContainerExecInspect(ctx, ex.ID)
	})
	return out.String(), code, err
}

// execExitWait bounds how long exec waits for a process whose output has
// ended to be reported as exited.
const execExitWait = 30 * time.Second

// waitExec polls an exec until Docker reports it finished and returns its
// exit code. Its output stream ending is not enough: the exit code is only
// meaningful once Running is false.
func waitExec(ctx context.Context, inspect func(context.Context) (container.ExecInspect, error)) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, execExitWait)
	defer cancel()
	delay := 10 * time.Millisecond
	for {
		ins, err := inspect(ctx)
		if err != nil {
			return -1, err
		}
		if !ins.Running {
			return ins.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, fmt.Errorf("exec still running: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, 500*time.Millisecond)
	}
}

// capWriter keeps the first max bytes written to it and discards the rest.
type capWriter struct {
	buf bytes.Buffer
	max int
}

func (w *capWriter) Write(b []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		w.buf.Write(b[:min(room, len(b))])
	}
	return len(b), nil
}

func (w *capWriter) String() string { return w.buf.String() }

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

func ptr[T any](v T) *T { return &v }
