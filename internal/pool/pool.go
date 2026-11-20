// Package pool manages the Docker side of Pitlane: a fixed number of warm
// "slots" (one long-lived Docker-in-Docker daemon each, keeping images and
// build cache between jobs), the shared cache volumes and the ephemeral
// runner containers attached to those slots.
package pool

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/docker/docker/client"
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
	mirrorVolume  = "pitlane-mirror-data"
	mirrorNetwork = "pitlane-mirror"
	runnerUID     = "1001:1001"
	workDir       = "/home/runner/_work"
	// The slot daemon only listens on loopback: the runner shares the slot's
	// network namespace, nothing else can reach the Docker API.
	dindAddr = "tcp://127.0.0.1:2375"
	// Bumped whenever the slot container definition changes: older slots are
	// recreated (their volumes, hence their cache, are kept).
	slotSpecVersion = "5"
	LabelSpec       = "dev.pitlane.spec"
)

// Slot statuses (SlotState.Status).
const (
	statusStarting = "starting" // its daemon is being created or started
	statusReady    = "ready"    // free: can be handed out
	statusBusy     = "busy"     // leased to a job
	statusCleaning = "cleaning" // cleaned after a job, or pruned for disk space
	statusError    = "error"    // failed: the retry loop starts (or rebuilds) it again
	statusRetiring = "retiring" // shown for a slot beyond the slot count, until removed
)

// SlotState is a read-only snapshot of a slot.
type SlotState struct {
	Index    int    `json:"index"`
	Status   string `json:"status"` // one of the status constants
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

// Pool owns the slots.
//
// Locking: mu guards the slot table and everything marked "guarded by mu".
// It is never held across a Docker call (only the small state file is
// written under it): the slow paths take a snapshot under mu, work without
// it, then publish the result under mu again, re-checking what may have
// changed meanwhile.
type Pool struct {
	cli *client.Client
	log *slog.Logger

	released chan struct{} // signalled (without blocking) when a slot frees up

	mu        sync.Mutex
	cfg       Config                // guarded by mu
	slots     map[int]*slot         // guarded by mu
	chowned   map[string]bool       // guarded by mu: volumes whose root is owned by the runner user
	externals map[int]string        // guarded by mu: slot -> runner image ID copied into its externals volume
	restored  map[int]persistedSlot // guarded by mu: persisted state, including slots not live
	binfmt    map[string]string     // guarded by mu: binfmt_misc entries registered by Pitlane (name -> content)
	qemuWant  bool                  // guarded by mu: QEMU should be (re)registered as soon as no job runs
	qemuBusy  bool                  // guarded by mu: registration in progress, no slot is handed out
	// runnerFallback is the runner image Start used because the wanted one
	// (from) could not be pulled: Configure keeps it instead of the default.
	runnerFallback struct{ from, to string } // guarded by mu

	strictWaitSince time.Time        // guarded by mu: a strict job has been waiting for a clean slot since then
	strictDemand    bool             // guarded by mu: a strict target is enabled (AutoStrictSlots)
	reserved        map[int]int64    // guarded by mu: slot -> memory reserved for its job (bytes)
	meters          map[int]*Meter   // guarded by mu: slot -> measurement of its running job
	memHead         *memWaiter       // guarded by mu: oldest request refused for memory
	wipes           map[string]int64 // guarded by mu: slot wipes before a job, by reason
	cache           CacheBroker      // guarded by mu: actions cache server, nil when not serving
	cachePort       int              // guarded by mu

	pruneMu       sync.Mutex   // one disk reclaim at a time
	volMu         sync.RWMutex // cache volume removal (write) vs runner creation (read)
	cacheLimitsMu sync.Mutex   // serializes applyCacheLimits
	fwdMu         sync.Mutex   // cache forwarder creation

	stores storeSync // slot pnpm stores merged with their owner's canonical store

	selfOnce   sync.Once
	self       *selfInfo // set once by selfContainer: the container Pitlane runs in (nil on the host)
	cgroupWarn sync.Once // "cgroups not readable" is logged once

	// Seams: the zero value is the real thing, tests set fakes.
	sops     slotOps          // nil: Docker
	env      *hostEnv         // nil: realHost
	cgroups  cgroupFS         // zero: the host's cgroups
	memTotal func() int64     // nil: hostMemory
	now      func() time.Time // nil: time.Now
	shimExe  string           // binary installed as the Node shim; "": the running Pitlane
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
	p.loadState()
	return p, nil
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

func ptr[T any](v T) *T { return &v }
