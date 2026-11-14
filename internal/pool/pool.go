// Package pool manages the Docker side of Pitlane: a fixed number of warm
// "slots" (one long-lived Docker-in-Docker daemon each, keeping images and
// build cache between jobs), the shared cache volumes and the ephemeral
// runner containers attached to those slots.
package pool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
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

	mirrorName    = "pitlane-mirror"
	mirrorNetwork = "pitlane-mirror"
	runnerUID     = "1001:1001"
	workDir       = "/home/runner/_work"
	// The slot daemon only listens on loopback: the runner shares the slot's
	// network namespace, nothing else can reach the Docker API.
	dindAddr = "tcp://127.0.0.1:2375"
	// Bumped whenever the slot container definition changes: older slots are
	// recreated (their volumes, hence their cache, are kept).
	slotSpecVersion = "2"
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
}

func (c *Config) defaults() {
	if c.Slots <= 0 {
		c.Slots = 2
	}
	if c.RunnerImage == "" {
		c.RunnerImage = "ghcr.io/actions/actions-runner:latest"
	}
	if c.DindImage == "" {
		c.DindImage = "docker:28-dind"
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
	retire bool
}

// Pool owns the slots.
type Pool struct {
	cli *client.Client
	log *slog.Logger

	mu       sync.Mutex
	cfg      Config
	slots    map[int]*slot
	chowned  map[string]bool
	released chan struct{}
}

// New connects to Docker and prepares the pool. Call Start to bring slots up.
func New(cfg Config, log *slog.Logger) (*Pool, error) {
	cfg.defaults()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Pool{cli: cli, log: log, cfg: cfg, slots: map[int]*slot{}, chowned: map[string]bool{}, released: make(chan struct{}, 1)}, nil
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
	for _, img := range []string{cfg.DindImage, cfg.RunnerImage} {
		if err := p.EnsureImage(ctx, img, false); err != nil {
			return err
		}
	}
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
		p.mu.Lock()
		var failed []int
		for i, s := range p.slots {
			if s.Status == "error" && !s.retire {
				s.Status = "starting"
				failed = append(failed, i)
			}
		}
		p.mu.Unlock()
		for _, i := range failed {
			p.log.Info("retrying slot", "slot", i)
			go p.startSlot(ctx, i)
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
		p.slots[i] = &slot{SlotState: SlotState{Index: i, Status: "starting", Since: time.Now().Unix()}}
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
	p.mu.Unlock()
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
	for _, v := range []string{slotDocker(i), slotWork(i)} {
		if err := p.ensureVolume(ctx, v); err != nil {
			return err
		}
	}
	name := slotName(i)
	info, err := p.cli.ContainerInspect(ctx, name)
	switch {
	case errdefs.IsNotFound(err):
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
	case info.Config.Labels[LabelSpec] != slotSpecVersion:
		// Slot created by an older Pitlane (Docker API on all interfaces):
		// recreate it, its volumes (and cache) are kept.
		p.log.Info("recreating slot with hardened settings", "slot", i)
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

// connectMirror attaches the mirror to the slot's own network so slots never
// share a network with each other.
func (p *Pool) connectMirror(ctx context.Context, i int) {
	_ = p.cli.NetworkDisconnect(ctx, mirrorNetwork, slotName(i), true) // layout of older versions
	err := p.cli.NetworkConnect(ctx, slotNetwork(i), mirrorName, &network.EndpointSettings{Aliases: []string{mirrorName}})
	if err != nil && !errdefs.IsConflict(err) && !strings.Contains(err.Error(), "already exists") {
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
	_ = p.cli.ContainerRemove(ctx, slotName(i), container.RemoveOptions{Force: true, RemoveVolumes: false})
	p.mu.Lock()
	delete(p.slots, i)
	p.mu.Unlock()
}

// rebuildSlot deletes a slot with all its volumes and starts it again empty.
func (p *Pool) rebuildSlot(ctx context.Context, i int) {
	_ = p.cli.ContainerRemove(ctx, slotName(i), container.RemoveOptions{Force: true})
	p.DropSlotCache(ctx, i)
	p.mu.Lock()
	if s, ok := p.slots[i]; ok {
		s.Owner, s.TargetID = "", 0
	}
	delete(p.chowned, slotDocker(i))
	delete(p.chowned, slotWork(i))
	p.mu.Unlock()
	p.startSlot(ctx, i)
}

// DropSlotCache deletes a retired slot's volumes (frees disk).
func (p *Pool) DropSlotCache(ctx context.Context, i int) {
	_ = p.cli.VolumeRemove(ctx, slotDocker(i), true)
	_ = p.cli.VolumeRemove(ctx, slotWork(i), true)
}

// Lease is a slot reserved for one runner.
type Lease struct {
	Slot  int
	Owner string
}

// Acquire reserves a free slot, preferring one that already ran jobs for the
// same target, then the same owner (warmest cache). It returns false when
// every slot is busy.
func (p *Pool) Acquire(ctx context.Context, owner string, targetID int64) (Lease, bool) {
	p.mu.Lock()
	var best *slot
	score := -1
	for _, s := range p.slots {
		if s.Status != "ready" || s.retire {
			continue
		}
		sc := 0
		switch {
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
	needWipe := p.cfg.CacheScope == "owner" && best.Owner != "" && best.Owner != owner
	best.Status, best.Since = "busy", time.Now().Unix()
	best.Owner, best.TargetID = owner, targetID
	idx := best.Index
	p.mu.Unlock()

	if needWipe {
		p.log.Info("slot changes owner, wiping its caches", "slot", idx, "owner", owner)
		script := "set -e; docker ps -aq | xargs -r docker rm -f >/dev/null; docker system prune -af --volumes >/dev/null; docker builder prune -af >/dev/null; rm -rf " + workDir + "/_actions"
		if _, code, err := p.exec(ctx, slotName(idx), script); err != nil || code != 0 {
			// Never hand out a slot that may still hold another owner's data.
			p.log.Error("slot wipe failed, rebuilding it from scratch", "slot", idx, "code", code, "error", err)
			p.setStatus(idx, "starting", "")
			go p.rebuildSlot(context.Background(), idx)
			return Lease{}, false
		}
	}
	return Lease{Slot: idx, Owner: owner}, true
}

// Release cleans what the job left in the slot (containers, networks,
// volumes, workspace) while keeping images and build cache, then frees it.
func (p *Pool) Release(ctx context.Context, l Lease) {
	p.setStatus(l.Slot, "cleaning", "")
	cfg := p.Config()
	script := fmt.Sprintf(`docker ps -aq | xargs -r docker rm -f >/dev/null 2>&1
docker network prune -f >/dev/null 2>&1
docker volume prune -af >/dev/null 2>&1
docker image prune -f >/dev/null 2>&1
docker builder prune -f --keep-storage %dgb >/dev/null 2>&1
find %s -mindepth 1 -maxdepth 1 ! -name _actions -exec rm -rf {} + 2>/dev/null
chown %s %s
true`, cfg.KeepBuildGB, workDir, runnerUID, workDir)
	if _, _, err := p.exec(ctx, slotName(l.Slot), script); err != nil {
		p.log.Warn("slot cleanup failed, restarting its daemon", "slot", l.Slot, "error", err)
		_ = p.cli.ContainerRestart(ctx, slotName(l.Slot), container.StopOptions{})
		_ = p.waitDind(ctx, slotName(l.Slot))
	}
	p.mu.Lock()
	s, ok := p.slots[l.Slot]
	retire := ok && s.retire
	p.mu.Unlock()
	if retire {
		p.removeSlot(ctx, l.Slot)
		p.signal()
		return
	}
	p.setStatus(l.Slot, "ready", "")
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

// StartRunner creates and starts an ephemeral runner container in a slot.
func (p *Pool) StartRunner(ctx context.Context, spec RunnerSpec) (string, error) {
	cfg := p.Config()
	cacheVol := cacheVolume(cfg.CacheScope, spec.Owner)
	toolVol := strings.Replace(cacheVol, "pitlane-cache", "pitlane-toolcache", 1)
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
		"npm_config_store_dir=/home/runner/.cache/pnpm-store",
		"YARN_CACHE_FOLDER=/home/runner/.cache/yarn",
		"BUN_INSTALL_CACHE_DIR=/home/runner/.cache/bun",
		"GOMODCACHE=/home/runner/.cache/go-mod",
		"PIP_CACHE_DIR=/home/runner/.cache/pip",
		"PITLANE_SLOT=" + strconv.Itoa(slotIdx),
	}
	hc := &container.HostConfig{
		Init: ptr(true),
		// Share the slot daemon's network namespace (like a Kubernetes pod):
		// ports published by `services:` are reachable on localhost.
		NetworkMode: container.NetworkMode("container:" + slotName(slotIdx)),
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: slotWork(slotIdx), Target: workDir},
			{Type: mount.TypeVolume, Source: toolVol, Target: "/opt/hostedtoolcache"},
			{Type: mount.TypeVolume, Source: cacheVol, Target: "/home/runner/.cache"},
		},
	}
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
	case <-errCh:
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

// Orphan is a runner container found at startup.
type Orphan struct {
	ID       string
	Name     string
	Slot     int
	TargetID int64
	Owner    string
	Running  bool
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
		slotIdx, _ := strconv.Atoi(c.Labels[LabelSlot])
		tid, _ := strconv.ParseInt(c.Labels[LabelTarget], 10, 64)
		o := Orphan{ID: c.ID, Name: c.Labels[LabelRunner], Slot: slotIdx, TargetID: tid, Owner: c.Labels[LabelOwner], Running: c.State == "running"}
		out = append(out, o)
		p.mu.Lock()
		if s, ok := p.slots[slotIdx]; ok {
			s.Status, s.Owner, s.TargetID, s.Runner = "busy", o.Owner, tid, o.Name
		} else {
			p.slots[slotIdx] = &slot{SlotState: SlotState{Index: slotIdx, Status: "busy", Owner: o.Owner, TargetID: tid, Runner: o.Name, Since: time.Now().Unix()}}
		}
		p.mu.Unlock()
	}
	return out, nil
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
	const img = "registry:2"
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
	if err != nil && !errdefs.IsConflict(err) {
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
	var buf bytes.Buffer
	_, _ = stdcopy.StdCopy(&buf, &buf, att.Reader)
	ins, err := p.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		return buf.String(), -1, err
	}
	return buf.String(), ins.ExitCode, nil
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

func ptr[T any](v T) *T { return &v }
