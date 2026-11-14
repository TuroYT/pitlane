// Package scaler connects GitHub runner scale sets to the Docker slot pool.
// One listener runs per enabled target; when GitHub assigns jobs to a scale
// set, the matching number of ephemeral runners is started in free slots.
package scaler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// Credentials used to talk to GitHub. Either the App fields or PAT is set.
type Credentials struct {
	AppClientID string
	AppPEM      string
	PAT         string
}

// Valid reports whether credentials are usable.
func (c Credentials) Valid() bool { return c.PAT != "" || (c.AppClientID != "" && c.AppPEM != "") }

// Manager runs one listener per target.
type Manager struct {
	Version string
	// ListRunners lists a target's self-hosted runners through the REST API
	// (used to find orphan runners). Nil disables orphan cleanup.
	ListRunners func(ctx context.Context, t store.Target) ([]gh.Runner, error)

	store   *store.Store
	pool    *pool.Pool
	log     *slog.Logger
	logDir  string
	creds   func() Credentials
	changed func()

	mu      sync.Mutex
	targets map[int64]*target
	adopted map[int64][]*runner
	lost    map[int64]map[string]lostJob // found at startup, handed to targets by Sync
	// ctx is set by Start; until then Sync does nothing (the UI is served
	// while slots start, and runners left by a previous run are not adopted
	// yet).
	ctx context.Context
}

// New creates a manager. changed is called whenever visible state changes.
func New(st *store.Store, p *pool.Pool, dataDir string, creds func() Credentials, changed func(), log *slog.Logger) *Manager {
	logDir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	return &Manager{store: st, pool: p, log: log, logDir: logDir, creds: creds, changed: changed,
		targets: map[int64]*target{}, adopted: map[int64][]*runner{}, lost: map[int64]map[string]lostJob{}}
}

// Start adopts runners left by a previous process and starts every enabled target.
func (m *Manager) Start(ctx context.Context) error {
	orphans, err := m.pool.RunnerContainers(ctx)
	if err != nil {
		return err
	}
	running := map[string]*runner{}
	for _, o := range orphans {
		// Assume busy since the container was created: the job timeout then
		// still applies after a restart.
		r := &runner{name: o.Name, containerID: o.ID, lease: pool.Lease{Slot: o.Slot, Owner: o.Owner}, busy: true, startedAt: time.Now(), busySince: o.Created}
		if o.Created.Year() < 2000 {
			r.busySince = time.Now()
		}
		r.runnerID = m.store.RunnerIDByName(o.Name)
		if !o.Running {
			go m.finishRunner(nil, r)
			continue
		}
		m.log.Info("adopting runner from previous run", "runner", o.Name, "slot", o.Slot)
		m.mu.Lock()
		m.adopted[o.TargetID] = append(m.adopted[o.TargetID], r)
		m.mu.Unlock()
		running[o.Name] = r
	}
	m.findLostJobs(ctx, running)
	// Only now may Sync start listeners: adopted runners and lost jobs are
	// known, so they are handed to their target.
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Sync()
	go m.loop(ctx)
	return nil
}

func (m *Manager) loop(ctx context.Context) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	clean := time.NewTicker(time.Hour)
	defer clean.Stop()
	timeouts := time.NewTicker(30 * time.Second)
	defer timeouts.Stop()
	orphans := time.NewTicker(orphanInterval)
	defer orphans.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.pool.Released():
			m.reconcileAll()
		case <-tick.C:
			m.reconcileAll()
		case <-clean.C:
			m.housekeeping()
		case <-timeouts.C:
			m.enforceTimeouts()
		case <-orphans.C:
			go m.cleanupOrphansAll()
		}
	}
}

func (m *Manager) reconcileAll() {
	m.mu.Lock()
	ts := make([]*target, 0, len(m.targets))
	for _, t := range m.targets {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	// Oldest demand first so a busy target cannot starve the others.
	sort.Slice(ts, func(i, j int) bool { return ts[i].waitingSince().Before(ts[j].waitingSince()) })
	for _, t := range ts {
		t.reconcile()
	}
}

func (m *Manager) housekeeping() {
	retention := time.Duration(m.store.GetInt("history_days", 30)) * 24 * time.Hour
	if err := m.store.PruneJobs(retention); err != nil {
		m.log.Warn("prune job history", "error", err)
	}
	if err := m.store.PruneSessions(); err != nil {
		m.log.Warn("prune expired sessions", "error", err)
	}
	entries, _ := os.ReadDir(m.logDir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > retention {
			_ = os.Remove(filepath.Join(m.logDir, e.Name()))
		}
	}
}

// Sync starts, restarts or stops listeners so they match the stored targets.
// Before Start it only notifies the UI: Start syncs once it is ready.
func (m *Manager) Sync() {
	m.mu.Lock()
	started := m.ctx != nil
	m.mu.Unlock()
	if !started {
		m.changed()
		return
	}
	targets, err := m.store.Targets()
	if err != nil {
		m.log.Error("load targets", "error", err)
		return
	}
	want := map[int64]store.Target{}
	for _, t := range targets {
		if t.Enabled {
			want[t.ID] = t
		}
	}
	m.mu.Lock()
	for id, t := range m.targets {
		w, ok := want[id]
		if !ok || w.ScaleSetName != t.cfg().ScaleSetName || w.InstallationID != t.cfg().InstallationID {
			delete(m.targets, id)
			if ok {
				m.keepLostLocked(id, t)
			}
			go t.stop(!ok)
			continue
		}
		t.update(w)
	}
	creds := m.creds()
	for id, w := range want {
		if _, ok := m.targets[id]; ok || !creds.Valid() {
			continue
		}
		t := newTarget(m, w)
		for _, r := range m.adopted[id] {
			t.runners[r.name] = r
			go t.watch(r)
		}
		delete(m.adopted, id)
		for jobID, l := range m.lost[id] {
			t.lost[jobID] = l
		}
		delete(m.lost, id)
		m.targets[id] = t
		go t.run(m.ctx)
	}
	m.mu.Unlock()
	m.changed()
}

// Restart restarts every listener (e.g. after credentials change).
func (m *Manager) Restart() {
	m.mu.Lock()
	for id, t := range m.targets {
		delete(m.targets, id)
		m.keepLostLocked(id, t)
		go t.stop(false)
	}
	m.mu.Unlock()
	m.Sync()
}

// Shutdown stops listeners; busy runners keep running and are adopted on restart.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.targets {
		t.cancel()
	}
}

// RunnerContainer returns the container of a live runner.
func (m *Manager) RunnerContainer(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.targets {
		t.mu.Lock()
		r, ok := t.runners[name]
		t.mu.Unlock()
		if ok {
			return r.containerID, true
		}
	}
	return "", false
}

// LogFile returns the path of a finished runner's log.
func (m *Manager) LogFile(name string) string {
	return filepath.Join(m.logDir, filepath.Base(name)+".log")
}

// ----------------------------------------------------------------- snapshot

// RunnerState is shown in the UI.
type RunnerState struct {
	Name      string `json:"name"`
	Slot      int    `json:"slot"`
	Busy      bool   `json:"busy"`
	JobName   string `json:"jobName"`
	Repo      string `json:"repo"`
	StartedAt int64  `json:"startedAt"`
}

// TargetState is shown in the UI.
type TargetState struct {
	store.Target
	Status  string        `json:"status"`
	Error   string        `json:"error,omitempty"`
	Desired int           `json:"desired"`
	Waiting int           `json:"waiting"`
	Runners []RunnerState `json:"runners"`
}

// Snapshot returns the live state of every target.
func (m *Manager) Snapshot() []TargetState {
	targets, _ := m.store.Targets()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TargetState, 0, len(targets))
	for _, st := range targets {
		ts := TargetState{Target: st, Status: "disabled", Runners: []RunnerState{}}
		if t, ok := m.targets[st.ID]; ok {
			t.mu.Lock()
			ts.Status, ts.Error, ts.Desired, ts.Waiting = t.status, t.err, t.desired, t.waiting
			ts.ScaleSetID = t.scaleSetID
			for _, r := range t.runners {
				ts.Runners = append(ts.Runners, RunnerState{Name: r.name, Slot: r.lease.Slot, Busy: r.busy, JobName: r.jobName, Repo: r.repo, StartedAt: r.startedAt.Unix()})
			}
			t.mu.Unlock()
			sort.Slice(ts.Runners, func(i, j int) bool { return ts.Runners[i].Slot < ts.Runners[j].Slot })
		} else if st.Enabled && !m.creds().Valid() {
			ts.Status = "no-credentials"
		}
		out = append(out, ts)
	}
	return out
}

// ------------------------------------------------------------------ target

type runner struct {
	name        string
	containerID string
	runnerID    int64
	lease       pool.Lease
	busy        bool
	exited      bool
	killed      bool      // killed for exceeding the job time limit
	busySince   time.Time // when its job started
	jobID       string    // GitHub job it runs, cleared on JobCompleted
	jobName     string
	repo        string
	startedAt   time.Time
}

type target struct {
	m      *Manager
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	t           store.Target
	client      *scaleset.Client
	lst         *listener.Listener
	scaleSetID  int
	status      string
	err         string
	desired     int
	waiting     int
	waitSince   time.Time
	runners     map[string]*runner
	lost        map[string]lostJob // jobs bound to dead runners, by job ID
	starting    int                // runners being prepared/started in the background
	reconcileMu sync.Mutex
	orphanMu    sync.Mutex
	orphanAt    time.Time // last orphan cleanup
}

func newTarget(m *Manager, t store.Target) *target {
	ctx, cancel := context.WithCancel(context.Background())
	return &target{m: m, t: t, ctx: ctx, cancel: cancel, status: "starting", runners: map[string]*runner{}, lost: map[string]lostJob{},
		log: m.log.With("target", t.FullName())}
}

func (t *target) cfg() store.Target {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.t
}

func (t *target) update(n store.Target) {
	t.mu.Lock()
	t.t = n
	lst := t.lst
	t.mu.Unlock()
	if lst != nil {
		lst.SetMaxRunners(n.MaxRunners)
	}
	go t.reconcile()
}

func (t *target) waitingSince() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.waiting == 0 {
		return time.Now()
	}
	return t.waitSince
}

func (t *target) setStatus(status, errMsg string) {
	t.mu.Lock()
	t.status, t.err = status, errMsg
	t.mu.Unlock()
	t.m.changed()
}

func (t *target) run(parent context.Context) {
	go func() {
		select {
		case <-parent.Done():
			t.cancel()
		case <-t.ctx.Done():
		}
	}()
	backoff := 5 * time.Second
	for t.ctx.Err() == nil {
		start := time.Now()
		err := t.listen()
		if t.ctx.Err() != nil {
			break
		}
		if time.Since(start) > 5*time.Minute {
			backoff = 5 * time.Second
		}
		msg := "listener stopped"
		if err != nil {
			msg = err.Error()
		}
		t.log.Error("listener failed, retrying", "error", msg, "in", backoff)
		t.setStatus("error", msg)
		select {
		case <-t.ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (t *target) newClient() (*scaleset.Client, error) {
	creds := t.m.creds()
	cfg := t.cfg()
	info := scaleset.SystemInfo{System: "pitlane", Version: t.m.Version, Subsystem: "listener", ScaleSetID: cfg.ScaleSetID}
	if creds.AppClientID != "" && creds.AppPEM != "" && cfg.InstallationID != 0 {
		return scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.URL(),
			GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: creds.AppClientID, InstallationID: cfg.InstallationID, PrivateKey: creds.AppPEM},
			SystemInfo:      info,
		})
	}
	if creds.PAT != "" {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: cfg.URL(), PersonalAccessToken: creds.PAT, SystemInfo: info,
		})
	}
	return nil, errors.New("no GitHub credentials for this target (install the GitHub App on " + cfg.Owner + ")")
}

func (t *target) listen() error {
	client, err := t.newClient()
	if err != nil {
		return err
	}
	cfg := t.cfg()
	ss, err := client.GetRunnerScaleSet(t.ctx, 1, cfg.ScaleSetName)
	if err != nil {
		return fmt.Errorf("get scale set: %w", err)
	}
	if ss == nil {
		ss, err = client.CreateRunnerScaleSet(t.ctx, &scaleset.RunnerScaleSet{
			Name:          cfg.ScaleSetName,
			RunnerGroupID: 1,
			Labels:        []scaleset.Label{{Name: cfg.ScaleSetName}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
		t.log.Info("scale set created", "id", ss.ID, "label", cfg.ScaleSetName)
	}
	client.SetSystemInfo(scaleset.SystemInfo{System: "pitlane", Version: t.m.Version, Subsystem: "listener", ScaleSetID: ss.ID})
	if cfg.ScaleSetID != ss.ID {
		cfg.ScaleSetID = ss.ID
		if err := t.m.store.UpdateTarget(cfg); err != nil {
			t.log.Warn("save scale set id", "error", err)
		}
	}
	host, _ := os.Hostname()
	session, err := client.MessageSessionClient(t.ctx, ss.ID, fmt.Sprintf("pitlane-%s-%d", host, cfg.ID))
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer session.Close(context.Background())
	lst, err := listener.New(session, listener.Config{ScaleSetID: ss.ID, MaxRunners: cfg.MaxRunners, Logger: t.log.WithGroup("listener")})
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.client, t.lst, t.scaleSetID = client, lst, ss.ID
	t.t.ScaleSetID = ss.ID
	t.mu.Unlock()
	t.setStatus("listening", "")
	t.log.Info("listening for jobs", "scaleSet", ss.ID, "label", cfg.ScaleSetName)
	go t.cleanupOrphans()
	err = lst.Run(t.ctx, &scaler{t: t, session: session})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// stop cancels the listener. When remove is set the scale set is deleted on
// GitHub once its busy runners are done.
func (t *target) stop(remove bool) {
	t.cancel()
	t.mu.Lock()
	for _, r := range t.runners {
		if !r.busy {
			t.m.pool.Remove(context.Background(), r.containerID)
		}
	}
	t.mu.Unlock()
	if !remove {
		return
	}
	deadline := time.Now().Add(6 * time.Hour)
	for time.Now().Before(deadline) {
		t.mu.Lock()
		n := len(t.runners)
		client, id := t.client, t.scaleSetID
		t.mu.Unlock()
		if n == 0 {
			if client != nil && id != 0 {
				if err := client.DeleteRunnerScaleSet(context.Background(), id); err != nil {
					t.log.Warn("delete scale set", "error", err)
				} else {
					t.log.Info("scale set deleted", "id", id)
				}
			}
			return
		}
		time.Sleep(5 * time.Second)
	}
}

// reconcile starts runners until the target has what GitHub asked for (or
// no slot is free), and removes idle runners that are no longer needed.
func (t *target) reconcile() {
	if t.ctx.Err() != nil {
		return
	}
	t.reconcileMu.Lock()
	defer t.reconcileMu.Unlock()

	t.mu.Lock()
	client := t.client // replaced by listen(): read under the lock only
	if client == nil {
		t.mu.Unlock()
		return
	}
	cfg := t.t
	want := min(cfg.MaxRunners, cfg.MinRunners+effectiveDesired(t.desired, pruneLostJobs(t.lost, time.Now())))
	have := t.starting
	var idle []*runner
	for _, r := range t.runners {
		if r.exited {
			continue
		}
		have++
		if !r.busy && time.Since(r.startedAt) > 30*time.Second {
			idle = append(idle, r)
		}
	}
	t.mu.Unlock()

	// Runners are prepared and started in parallel, in the background: the
	// listener is never blocked and a slow start does not delay the others.
	started := 0
	for i := 0; i < want-have; i++ {
		lease, ok := t.m.pool.Acquire(t.ctx, cfg.Owner, cfg.ID, cfg.Strict)
		if !ok {
			break
		}
		started++
		t.mu.Lock()
		t.starting++
		t.mu.Unlock()
		go t.launch(lease)
	}
	missing := max(0, want-have-started)

	// Scale down: idle runners beyond what is needed are deregistered first
	// (GitHub refuses if a job was just assigned) and only then killed.
	excess := have + started - want
	for _, r := range idle {
		if excess <= 0 {
			break
		}
		if r.runnerID != 0 {
			if err := client.RemoveRunner(t.ctx, r.runnerID); err != nil {
				continue
			}
			t.m.forgetRunner(r.runnerID, nil)
		}
		t.log.Info("removing idle runner", "runner", r.name)
		t.m.pool.Remove(context.Background(), r.containerID)
		excess--
	}

	t.mu.Lock()
	if missing > 0 && t.waiting == 0 {
		t.waitSince = time.Now()
	}
	changed := t.waiting != missing || started > 0
	t.waiting = missing
	t.mu.Unlock()
	if changed {
		t.m.changed()
	}
}

func (t *target) launch(lease pool.Lease) {
	defer func() {
		t.mu.Lock()
		t.starting--
		t.mu.Unlock()
		t.m.changed()
	}()
	if err := t.m.pool.Prepare(t.ctx, lease); err != nil {
		t.log.Error("prepare slot", "slot", lease.Slot, "error", err)
		if !errors.Is(err, pool.ErrSlotRebuilding) {
			t.m.pool.Release(context.Background(), lease)
		}
		return
	}
	if err := t.startRunner(lease); err != nil {
		t.log.Error("start runner", "error", err)
		t.m.pool.Release(context.Background(), lease)
		t.setStatus("error", err.Error())
	}
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (t *target) startRunner(lease pool.Lease) error {
	cfg := t.cfg()
	t.mu.Lock()
	client, ssID := t.client, t.scaleSetID
	t.mu.Unlock()
	name := "pitlane-" + randomSuffix()
	lease.Runner = name
	jit, err := client.GenerateJitRunnerConfig(t.ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, ssID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	if jit.Runner != nil {
		// Recorded before the container exists: if Pitlane dies now, the
		// orphan cleanup still finds this registration.
		if err := t.m.store.AddRunner(store.RegisteredRunner{ID: int64(jit.Runner.ID), TargetID: cfg.ID, Name: name}); err != nil {
			// Not fatal, but the orphan cleanup will never see this runner.
			t.log.Error("record runner registration: it will not be cleaned up if it is orphaned", "runner", name, "id", jit.Runner.ID, "error", err)
		}
	}
	id, err := t.m.pool.StartRunner(t.ctx, pool.RunnerSpec{Name: name, JITConfig: jit.EncodedJITConfig, TargetID: cfg.ID, Owner: cfg.Owner, Lease: lease})
	if err != nil {
		if jit.Runner != nil {
			id := int64(jit.Runner.ID)
			t.m.forgetRunner(id, client.RemoveRunner(context.Background(), id))
		}
		return err
	}
	r := &runner{name: name, containerID: id, lease: lease, startedAt: time.Now()}
	if jit.Runner != nil {
		r.runnerID = int64(jit.Runner.ID)
	}
	t.mu.Lock()
	t.runners[name] = r
	t.mu.Unlock()
	t.log.Info("runner started", "runner", name, "slot", lease.Slot)
	go t.watch(r)
	return nil
}

func (t *target) watch(r *runner) {
	code := t.m.pool.Wait(context.Background(), r.containerID)
	t.log.Info("runner exited", "runner", r.name, "code", code)
	t.m.finishRunner(t, r)
}

// finishRunner archives logs, removes the container, frees the slot and
// makes sure the runner is gone from GitHub.
func (m *Manager) finishRunner(t *target, r *runner) {
	ctx := context.Background()
	if f, err := os.OpenFile(m.LogFile(r.name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		if err := archiveLog(f, maxArchivedLog, func(w io.Writer) error { return m.pool.Logs(ctx, r.containerID, false, w) }); err != nil {
			m.log.Warn("archive runner log", "runner", r.name, "error", err)
		}
		f.Close()
	} else {
		m.log.Warn("archive runner log", "runner", r.name, "error", err)
	}
	m.pool.Remove(ctx, r.containerID)
	if t != nil {
		t.runnerGone(r)
		t.mu.Lock()
		r.exited = true
		client := t.client
		t.mu.Unlock()
		if client != nil && r.runnerID != 0 {
			// Usually already gone (ephemeral): NotFound also clears the record.
			m.forgetRunner(r.runnerID, client.RemoveRunner(ctx, r.runnerID))
		}
	}
	m.pool.Release(ctx, r.lease)
	if t != nil {
		t.mu.Lock()
		delete(t.runners, r.name)
		t.mu.Unlock()
	}
	m.changed()
	m.pool.EnsureFreeDisk(ctx) // idle slots only; skipped while another prune runs
}

// maxArchivedLog bounds the archived log of one runner (its output is
// controlled by the job).
const maxArchivedLog = 64 << 20

var errLogFull = errors.New("log size limit reached")

// capWriter passes through at most n bytes, then fails with errLogFull.
type capWriter struct {
	w io.Writer
	n int64
}

func (c *capWriter) Write(b []byte) (int, error) {
	if c.n <= 0 {
		return 0, errLogFull
	}
	full := int64(len(b)) > c.n
	if full {
		b = b[:c.n]
	}
	n, err := c.w.Write(b)
	c.n -= int64(n)
	if err == nil && full {
		err = errLogFull
	}
	return n, err
}

// archiveLog copies a log through copyTo into w, keeping at most max bytes
// followed by a truncation notice.
func archiveLog(w io.Writer, max int64, copyTo func(io.Writer) error) error {
	err := copyTo(&capWriter{w: w, n: max})
	if errors.Is(err, errLogFull) {
		_, err = fmt.Fprintf(w, "\n[pitlane] log truncated at %d MB\n", max>>20)
	}
	return err
}

// timedOut reports whether a runner's job ran longer than limit (0 = no limit).
func timedOut(r *runner, limit time.Duration, now time.Time) bool {
	return limit > 0 && r.busy && !r.exited && !r.killed && !r.busySince.IsZero() && now.Sub(r.busySince) > limit
}

// timeoutStopGrace is how long a runner over its time limit has, after
// SIGTERM, to cancel its job and report it before it is killed.
const timeoutStopGrace = 30 * time.Second

// enforceTimeouts stops the runners whose job exceeded the target's
// max_job_minutes. The runner's watch then goes through finishRunner as for
// any other exit (logs archived, slot cleaned and released).
func (m *Manager) enforceTimeouts() {
	m.mu.Lock()
	ts := make([]*target, 0, len(m.targets))
	for _, t := range m.targets {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	now := time.Now()
	for _, t := range ts {
		t.mu.Lock()
		limit := time.Duration(t.t.MaxJobMinutes) * time.Minute
		var kill []*runner
		for _, r := range t.runners {
			if timedOut(r, limit, now) {
				r.killed = true
				kill = append(kill, r)
			}
		}
		t.mu.Unlock()
		for _, r := range kill {
			t.log.Warn("job exceeded the time limit, stopping its runner", "runner", r.name, "job", r.jobName, "limit", limit)
			// In the background: a graceful stop blocks up to its grace period.
			go func(t *target, r *runner) {
				ctx, cancel := context.WithTimeout(context.Background(), timeoutStopGrace+time.Minute)
				defer cancel()
				if err := m.pool.StopRunner(ctx, r.containerID, timeoutStopGrace); err != nil {
					t.log.Error("stop runner", "runner", r.name, "error", err)
					t.mu.Lock()
					r.killed = false // retried at the next check
					t.mu.Unlock()
				}
			}(t, r)
		}
	}
}

// ------------------------------------------------------------------ scaler

type scaler struct {
	t       *target
	session *scaleset.MessageSessionClient
}

func unix(ts time.Time) int64 {
	if ts.IsZero() || ts.Year() < 2000 {
		return 0
	}
	return ts.Unix()
}

func shortRef(ref string) string {
	// "owner/repo/.github/workflows/ci.yml@refs/heads/main" -> "ci.yml"
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	return filepath.Base(ref)
}

func (s *scaler) job(b scaleset.JobMessageBase) store.Job {
	cfg := s.t.cfg()
	return store.Job{TargetID: cfg.ID, JobID: b.JobID, RunID: b.WorkflowRunID, Owner: b.OwnerName, Repo: b.RepositoryName,
		WorkflowRef: shortRef(b.JobWorkflowRef), JobName: b.JobDisplayName, Event: b.EventName, Slot: -1, QueuedAt: unix(b.QueueTime)}
}

// upsertJob records a job in the history; a failure only affects the UI and
// the lost-job detection after a restart, so it is logged.
func (s *scaler) upsertJob(j store.Job) {
	if err := s.t.m.store.UpsertJob(j); err != nil {
		s.t.log.Warn("record job", "job", j.JobID, "error", err)
	}
}

func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		s.t.reconcile()
		return nil
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
			s.upsertJob(s.job(j.JobMessageBase))
		}
		if _, err := s.session.AcquireJobs(ctx, ids); err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
	}
	for _, j := range msg.JobStartedMessages {
		job := s.job(j.JobMessageBase)
		job.RunnerName = j.RunnerName
		job.StartedAt = unix(j.RunnerAssignTime)
		if job.StartedAt == 0 {
			job.StartedAt = time.Now().Unix()
		}
		s.t.mu.Lock()
		if r, ok := s.t.runners[j.RunnerName]; ok {
			if !r.busy || r.busySince.IsZero() {
				r.busySince = time.Now()
			}
			r.busy, r.jobName, r.repo, r.jobID = true, j.JobDisplayName, j.RepositoryName, j.JobID
			job.Slot = r.lease.Slot
		}
		if l, ok := s.t.lost[j.JobID]; ok && l.runner != j.RunnerName {
			delete(s.t.lost, j.JobID) // started again elsewhere: it needs a runner
		}
		s.t.mu.Unlock()
		s.upsertJob(job)
	}
	for _, j := range msg.JobCompletedMessages {
		job := s.job(j.JobMessageBase)
		job.RunnerName = j.RunnerName
		job.Result = j.Result
		job.FinishedAt = unix(j.FinishTime)
		if job.FinishedAt == 0 {
			job.FinishedAt = time.Now().Unix()
		}
		if job.Result == "" {
			job.Result = "canceled"
		}
		s.t.mu.Lock()
		delete(s.t.lost, j.JobID)
		if r, ok := s.t.runners[j.RunnerName]; ok && r.jobID == j.JobID {
			r.jobID = ""
		}
		s.t.mu.Unlock()
		s.upsertJob(job)
	}
	if msg.Statistics != nil {
		s.t.mu.Lock()
		s.t.desired = msg.Statistics.TotalAssignedJobs
		s.t.mu.Unlock()
	}
	s.t.m.changed()
	s.t.reconcile()
	return nil
}
