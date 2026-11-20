package pool

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
)

// runnerFilter selects Pitlane's runner containers.
func runnerFilter() filters.Args {
	return filters.NewArgs(filters.Arg("label", LabelManaged+"=true"), filters.Arg("label", LabelRole+"=runner"))
}

// RunnerNames returns the runner names of every Pitlane runner container
// that still exists, running or not. It changes no slot state.
func (p *Pool) RunnerNames(ctx context.Context) (map[string]bool, error) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: runnerFilter()})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range list {
		out[c.Labels[LabelRunner]] = true
		for _, n := range c.Names {
			if len(n) > 1 {
				out[n[1:]] = true // "/pitlane-xxxx"
			}
		}
	}
	return out, nil
}

// RunnerSpec describes a runner container to start.
type RunnerSpec struct {
	Name      string
	JITConfig string
	TargetID  int64
	Owner     string
	Repo      string // repository of a repository target ("" for an organization)
	Lease     Lease
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func cacheVolume(scope, owner string) string {
	if scope == scopeShared {
		return "pitlane-cache"
	}
	return "pitlane-cache-" + strings.ToLower(unsafeChars.ReplaceAllString(owner, "_"))
}

// runnerLogConfig bounds what Docker keeps of a runner's output (a job
// controls it): at most two 50 MB files, whatever the daemon's default.
func runnerLogConfig() container.LogConfig {
	return container.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "50m", "max-file": "2"}}
}

// Cache directories inside a runner container, each on its own volume.
const (
	runnerCacheDir = "/home/runner/.cache"
	runnerToolDir  = "/opt/hostedtoolcache"
)

// StartRunner creates and starts an ephemeral runner container in a slot.
func (p *Pool) StartRunner(ctx context.Context, spec RunnerSpec) (string, error) {
	// Cache volumes must not be pruned between their creation and their use.
	p.volMu.RLock()
	defer p.volMu.RUnlock()
	cfg := p.Config()
	cacheVol, toolVol := runnerVolumes(cfg.CacheScope, spec)
	for _, v := range []string{cacheVol, toolVol} {
		if err := p.ensureVolume(ctx, v); err != nil {
			return "", err
		}
	}
	env := runnerEnv(cfg, spec)
	if e := p.cacheEnv(ctx, cfg, spec); e != "" {
		env = append(env, e)
	}
	cc, hc := runnerContainer(cfg, spec, env, cacheVol, toolVol)
	id, err := p.createRunner(ctx, spec.Name, cc, hc)
	if err != nil {
		return "", err
	}
	p.MarkRunner(spec.Lease.Slot, spec.Name)
	return id, nil
}

// runnerVolumes returns the cache and tool cache volumes of a runner: its
// owner's (or everyone's, in the shared scope), or throwaway ones for a
// strict job, removed when the job ends.
func runnerVolumes(scope string, spec RunnerSpec) (cacheVol, toolVol string) {
	if spec.Lease.Strict {
		return strictVolumes(spec.Name)
	}
	cacheVol = cacheVolume(scope, spec.Owner)
	return cacheVol, strings.Replace(cacheVol, "pitlane-cache", "pitlane-toolcache", 1)
}

// runnerEnv is the environment of a runner container (without the actions
// cache URL, see cacheEnv).
func runnerEnv(cfg Config, spec RunnerSpec) []string {
	return []string{
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + spec.JITConfig,
		"DOCKER_HOST=" + dindAddr,
		"RUNNER_TOOL_CACHE=" + runnerToolDir,
		"AGENT_TOOLSDIRECTORY=" + runnerToolDir,
		"ImageOS=" + cfg.ImageOS,
		"XDG_CACHE_HOME=" + runnerCacheDir,
		"npm_config_cache=" + runnerCacheDir + "/npm",
		// Same filesystem as the workspace: pnpm hardlinks packages into
		// node_modules instead of copying them (minutes saved per install).
		"npm_config_store_dir=" + workDir + "/" + pnpmStoreDir,
		"YARN_CACHE_FOLDER=" + runnerCacheDir + "/yarn",
		"BUN_INSTALL_CACHE_DIR=" + runnerCacheDir + "/bun",
		"GOMODCACHE=" + runnerCacheDir + "/go-mod",
		"PIP_CACHE_DIR=" + runnerCacheDir + "/pip",
		"PITLANE_SLOT=" + strconv.Itoa(spec.Lease.Slot),
		// The official image dumps the runner's internal debug traces to stdout
		// (meant for Kubernetes); keep the runner output readable instead.
		"ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=0",
	}
}

// runnerContainer is the definition of a runner container.
func runnerContainer(cfg Config, spec RunnerSpec, env []string, cacheVol, toolVol string) (*container.Config, *container.HostConfig) {
	slotIdx := spec.Lease.Slot
	cc := &container.Config{
		Image: cfg.RunnerImage,
		User:  "runner",
		Cmd:   []string{"/home/runner/run.sh"},
		Env:   env,
		Labels: map[string]string{
			LabelManaged: "true", LabelRole: "runner", LabelSlot: strconv.Itoa(slotIdx),
			LabelTarget: strconv.FormatInt(spec.TargetID, 10), LabelRunner: spec.Name, LabelOwner: spec.Owner,
			LabelStrict: strconv.FormatBool(spec.Lease.Strict), LabelRepo: spec.Repo,
		},
	}
	hc := &container.HostConfig{
		Init: ptr(true),
		// Share the slot daemon's network namespace (like a Kubernetes pod):
		// ports published by `services:` are reachable on localhost.
		NetworkMode: container.NetworkMode("container:" + slotName(slotIdx)),
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: slotWork(slotIdx), Target: workDir},
			{Type: mount.TypeVolume, Source: slotExternals(slotIdx), Target: externalsDir, ReadOnly: true},
			{Type: mount.TypeVolume, Source: toolVol, Target: runnerToolDir},
			{Type: mount.TypeVolume, Source: cacheVol, Target: runnerCacheDir},
		},
		LogConfig: runnerLogConfig(),
	}
	hc.CPUShares = cfg.CPUShares
	hc.Memory = cfg.memoryBytes()
	return cc, hc
}

// createRunner creates and starts the runner container name. On failure
// nothing is left behind: neither the container (it carries a JIT config)
// nor the runner's actions cache token.
func (p *Pool) createRunner(ctx context.Context, name string, cc *container.Config, hc *container.HostConfig) (string, error) {
	created, err := p.cli.ContainerCreate(ctx, cc, hc, nil, nil, name)
	if err != nil {
		if ctx.Err() != nil {
			// Canceled while Docker may have created it: remove it by name.
			p.removeAfterFailedStart(ctx, name)
		}
		p.revokeCache(name)
		return "", fmt.Errorf("create runner: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		p.removeAfterFailedStart(ctx, created.ID)
		p.revokeCache(name)
		return "", fmt.Errorf("start runner: %w", err)
	}
	return created.ID, nil
}

// removeAfterFailedStart removes a runner container that could not be
// started. It runs even when ctx is canceled (the target stopped while its
// runner was starting): the container carries a JIT config and the labels
// of a slot that is released and handed out again.
func (p *Pool) removeAfterFailedStart(ctx context.Context, id string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	p.Remove(cctx, id)
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
	// Untrusted is set when the target or owner label is missing or
	// invalid: the container belongs to no known target, or its slot's data
	// to no known owner, and its slot is released as strict (rebuilt). Slot
	// is -1 when the slot label is missing or invalid.
	Untrusted bool
	Running   bool
	Created   time.Time
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
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: runnerFilter()})
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
// slot without a rebuild. A container whose target or owner label is missing
// or invalid is untrusted and its slot strict the same way: a slot with no
// owner would otherwise look empty and go to any owner without a wipe.
func (p *Pool) adoptContainer(id string, labels map[string]string, state string, created int64) Orphan {
	slotIdx, err := strconv.Atoi(labels[LabelSlot])
	if err != nil || slotIdx < 0 {
		slotIdx = -1
	}
	tid, err := strconv.ParseInt(labels[LabelTarget], 10, 64)
	untrusted := err != nil || tid <= 0 || labels[LabelOwner] == ""
	if untrusted {
		tid = 0
	}
	o := Orphan{ID: id, Name: labels[LabelRunner], Slot: slotIdx, TargetID: tid, Owner: labels[LabelOwner],
		Strict: untrusted || labels[LabelStrict] != "false", Untrusted: untrusted, Running: state == "running", Created: time.Unix(created, 0)}
	if slotIdx < 0 {
		return o // no slot to mark: the scaler only stops and removes it
	}
	p.adoptCache(o, labels[LabelRepo])
	owner := o.Lease().Owner
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.slots[slotIdx]; ok {
		s.Status, s.Owner, s.TargetID, s.Runner = statusBusy, owner, tid, o.Name
	} else {
		p.slots[slotIdx] = &slot{SlotState: SlotState{Index: slotIdx, Status: statusBusy, Owner: owner, TargetID: tid, Runner: o.Name, Since: p.clock().Unix()}}
	}
	return o
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
