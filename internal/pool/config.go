package pool

import "github.com/TuroYT/pitlane/internal/cache"

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

// Cache scopes (Config.CacheScope).
const (
	scopeOwner  = "owner"  // caches never cross GitHub owners
	scopeShared = "shared" // every owner shares the caches
)

// Defaults of the Config fields left unset.
const (
	defaultSlots          = 2
	defaultCacheScope     = scopeOwner
	defaultKeepBuildGB    = 20
	defaultImageOS        = "ubuntu24"
	defaultActionsCacheGB = 20
)

// defaults fills the fields left unset (or out of range) with their default.
// Settings saved by older versions may hold former default images: those
// follow the current defaults too.
func (c *Config) defaults() {
	c.defaultSlots()
	c.defaultImages()
	c.defaultCaches()
	c.defaultMemory()
}

func (c *Config) defaultSlots() {
	if c.Slots <= 0 {
		c.Slots = defaultSlots
	}
	if c.StrictSlots < AutoStrictSlots {
		c.StrictSlots = AutoStrictSlots
	}
}

func (c *Config) defaultImages() {
	if c.RunnerImage == "" || isDefaultRunnerImage(c.RunnerImage) {
		c.RunnerImage = DefaultRunnerImage
	}
	if c.DindImage == "" || c.DindImage == legacyDindImage {
		c.DindImage = DefaultDindImage
	}
	if c.ImageOS == "" {
		c.ImageOS = defaultImageOS
	}
}

func (c *Config) defaultCaches() {
	if c.CacheScope == "" {
		c.CacheScope = defaultCacheScope
	}
	if c.KeepBuildGB <= 0 {
		c.KeepBuildGB = defaultKeepBuildGB
	}
	if c.ActionsCacheGB <= 0 {
		c.ActionsCacheGB = defaultActionsCacheGB
	}
	if c.ActionsCacheOwnerPct <= 0 || c.ActionsCacheOwnerPct > 100 {
		c.ActionsCacheOwnerPct = cache.DefaultOwnerPercent
	}
}

func (c *Config) defaultMemory() {
	if c.MemoryReserveMB < 0 {
		c.MemoryReserveMB = DefaultMemoryReserveMB
	}
}

// WithDefaults returns c with the defaults Configure applies to the settings
// left empty or out of range.
func (c Config) WithDefaults() Config {
	c.defaults()
	return c
}
