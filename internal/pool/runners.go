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

// RunnerNames returns the runner names of every Pitlane runner container
// that still exists, running or not. It changes no slot state.
func (p *Pool) RunnerNames(ctx context.Context) (map[string]bool, error) {
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", LabelManaged+"=true"), filters.Arg("label", LabelRole+"=runner"))})
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
	if e := p.cacheEnv(ctx, cfg, spec); e != "" {
		env = append(env, e)
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
			LabelStrict: strconv.FormatBool(spec.Lease.Strict), LabelRepo: spec.Repo,
		},
	}, hc, nil, nil, spec.Name)
	if err != nil {
		p.revokeCache(spec.Name)
		return "", fmt.Errorf("create runner: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = p.cli.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true})
		p.revokeCache(spec.Name)
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
	// Untrusted is set when the target label is missing or invalid: the
	// container belongs to no known target and its slot is released as
	// strict (rebuilt). Slot is -1 when the slot label is missing or invalid.
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
// slot without a rebuild. A container whose target label is missing or
// invalid is untrusted and its slot strict the same way.
func (p *Pool) adoptContainer(id string, labels map[string]string, state string, created int64) Orphan {
	slotIdx, err := strconv.Atoi(labels[LabelSlot])
	if err != nil || slotIdx < 0 {
		slotIdx = -1
	}
	tid, err := strconv.ParseInt(labels[LabelTarget], 10, 64)
	untrusted := err != nil || tid <= 0
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
		s.Status, s.Owner, s.TargetID, s.Runner = "busy", owner, tid, o.Name
	} else {
		p.slots[slotIdx] = &slot{SlotState: SlotState{Index: slotIdx, Status: "busy", Owner: owner, TargetID: tid, Runner: o.Name, Since: time.Now().Unix()}}
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
