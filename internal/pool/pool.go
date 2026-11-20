// Package pool manages the Docker side of Pitlane: a fixed number of warm
// "slots" (one long-lived Docker-in-Docker daemon each, keeping images and
// build cache between jobs), the shared cache volumes and the ephemeral
// runner containers attached to those slots.
package pool

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
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
