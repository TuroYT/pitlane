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
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/TuroYT/pitlane/internal/cache"
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
	// LabelRepo records the repository of a repository target's runner
	// (absent for organization targets), so an adopted runner keeps the same
	// actions cache restriction.
	LabelRepo = "dev.pitlane.repo"

	mirrorName    = "pitlane-mirror"
	mirrorNetwork = "pitlane-mirror"
	runnerUID     = "1001:1001"
	workDir       = "/home/runner/_work"
	// The slot daemon only listens on loopback: the runner shares the slot's
	// network namespace, nothing else can reach the Docker API.
	dindAddr = "tcp://127.0.0.1:2375"
	// Bumped whenever the slot container definition changes: older slots are
	// recreated (their volumes, hence their cache, are kept).
	slotSpecVersion = "4"
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
	// ActionsCache serves actions/cache from the Pitlane host (internal/cache)
	// for non-strict jobs; ActionsCacheGB bounds what it stores, and
	// ActionsCacheOwnerPct (percent of it) what one owner may store.
	ActionsCache         bool
	ActionsCacheGB       int
	ActionsCacheOwnerPct int
	// MemoryAdmission admits a job only when the predicted memory of the
	// running jobs plus its own fits the host's memory minus
	// MemoryReserveMB (see admit.go).
	MemoryAdmission bool
	MemoryReserveMB int64
	// StrictSlots is how many slots only strict jobs use (AutoStrictSlots:
	// one while a strict target is enabled, else none).
	StrictSlots int
}

// AutoStrictSlots lets the pool reserve one slot for strict jobs while a
// strict target is enabled (SetStrictDemand), none otherwise.
const AutoStrictSlots = -1

// DefaultMemoryReserveMB is the memory kept for the host (system, Docker,
// Pitlane, page cache) when admitting jobs.
const DefaultMemoryReserveMB = 2048

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
	if c.ActionsCacheGB <= 0 {
		c.ActionsCacheGB = 20
	}
	if c.ActionsCacheOwnerPct <= 0 || c.ActionsCacheOwnerPct > 100 {
		c.ActionsCacheOwnerPct = cache.DefaultOwnerPercent
	}
	if c.MemoryReserveMB < 0 {
		c.MemoryReserveMB = DefaultMemoryReserveMB
	}
	if c.StrictSlots < AutoStrictSlots {
		c.StrictSlots = AutoStrictSlots
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
	// LastUsed is when the slot was last released after a job (unix
	// seconds, 0 = never or unknown): the least recently used warm slot is
	// the one sacrificed when a wipe cannot be avoided.
	LastUsed int64 `json:"lastUsed,omitempty"`
	// StrictOnly marks a slot reserved for strict jobs.
	StrictOnly bool `json:"strictOnly,omitempty"`
	// Predicted is the memory reserved for the slot's job (bytes, 0 when
	// the slot is free), Actual what its runner and slot use now (runner
	// memory.current plus the slot's anonymous memory, 0 when unknown).
	Predicted int64 `json:"predicted,omitempty"`
	Actual    int64 `json:"actual,omitempty"`
}

type slot struct {
	SlotState
	retire  bool
	upgrade bool // recreate the slot container once its current job is done
	// dirty marks a slot that may still hold another owner's data (a wipe or
	// rebuild failed). It is never handed out; the retry loop rebuilds it.
	dirty bool
	// wiping marks a slot leased with a wipe still to do: its owner is still
	// the previous one, but its cache no longer counts as that owner's.
	wiping bool
}

// persistedSlot is what survives a Pitlane restart. It lives in Pitlane's
// data directory, which jobs cannot reach, so it can be trusted.
type persistedSlot struct {
	Owner     string `json:"owner"`
	TargetID  int64  `json:"targetId"`
	Externals string `json:"externals"` // runner image ID copied into the externals volume
	LastUsed  int64  `json:"lastUsed,omitempty"`
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

	strictWaitSince time.Time        // a strict job has been waiting for a clean slot since then
	strictDemand    bool             // a strict target is enabled (AutoStrictSlots)
	reserved        map[int]int64    // slot -> memory reserved for its job (bytes)
	meters          map[int]*Meter   // slot -> measurement of its running job
	memHead         *memWaiter       // oldest request refused for memory
	memTotal        func() int64     // nil: hostMemory (tests set a value)
	wipes           map[string]int64 // slot wipes before a job, by reason
	now             func() time.Time // nil: time.Now (tests set a clock)

	pruneMu sync.Mutex   // one disk reclaim at a time
	volMu   sync.RWMutex // cache volume removal (write) vs runner creation (read)

	stores storeSync // slot pnpm stores merged with their owner's canonical store

	cache     CacheBroker // actions cache server, nil when not serving
	cachePort int
	// cacheLimitsMu serializes applyCacheLimits.
	cacheLimitsMu sync.Mutex
	selfOnce      sync.Once
	self          *selfInfo  // container Pitlane runs in (nil on the host)
	fwdMu         sync.Mutex // cache forwarder creation
	shimExe       string     // binary installed as the Node shim; "" = the running Pitlane

	sops slotOps // nil: Docker (tests set a fake)

	cgroups    cgroupFS  // zero: the host's (tests set fake directories)
	cgroupWarn sync.Once // "cgroups not readable" is logged once
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
	defer p.Remove(context.Background(), c.ID)
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
		out[i] = persistedSlot{Owner: s.Owner, TargetID: s.TargetID, Externals: p.externals[i], LastUsed: s.LastUsed}
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

// Docker exposes the Docker client. Only the updater uses it (runner image
// inspection); everything else goes through the pool's own methods.
func (p *Pool) Docker() *client.Client { return p.cli }

// Ping checks that the Docker daemon answers.
func (p *Pool) Ping(ctx context.Context) error {
	_, err := p.cli.Ping(ctx)
	return err
}

// Config returns the current configuration.
func (p *Pool) Config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// Released is signalled whenever a slot becomes free.
func (p *Pool) Released() <-chan struct{} { return p.released }

// clock returns the current time.
func (p *Pool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Pool) signal() {
	select {
	case p.released <- struct{}{}:
	default:
	}
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
