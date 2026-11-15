package scaler

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Memory prediction for the admission (pool.Admit): what a job will need,
// from the memory of the last jobs with the same name (runner peak plus the
// slot's anonymous peak, see store.MemoryHistory).
const (
	// DefaultJobMemory is the prediction of a job with no history at all
	// (cold start), which is about what a Node.js build needs.
	DefaultJobMemory = int64(2) << 30
	minJobMemory     = int64(256) << 20
	jobSamples       = 20  // last runs of the same job
	repoSamples      = 50  // last jobs of the same repository
	globalSamples    = 200 // last jobs of any repository
	minGlobalSamples = 5   // below that, the default is safer than a few jobs
	predictTTL       = time.Minute
)

// jobKey names a job across runs.
type jobKey struct{ owner, repo, workflow, job string }

// history returns the memory of the last measured jobs matching the key's
// set fields, newest first (store.MemoryHistory).
type history func(ctx context.Context, owner, repo, workflow, job string, limit int) ([]int64, error)

type prediction struct {
	bytes  int64
	source string // job, repo, global or default
	at     time.Time
}

// predictor caches predictions for predictTTL: they only change when jobs
// finish.
type predictor struct {
	hist history
	now  func() time.Time

	mu    sync.Mutex
	cache map[jobKey]prediction
}

func newPredictor(h history) *predictor {
	return &predictor{hist: h, now: time.Now, cache: map[jobKey]prediction{}}
}

// estimate turns the memory of past runs into a prediction: the 90th
// percentile (nearest rank) once there are 5 runs or more, the highest run
// before that (a cold job is not trusted to stay small).
func estimate(samples []int64) (int64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	v := slices.Clone(samples)
	slices.Sort(v)
	if len(v) < 5 {
		return v[len(v)-1], true
	}
	rank := (len(v)*9 + 9) / 10 // ceil(0.9 n)
	return v[rank-1], true
}

// predict returns the memory a job is expected to need and where the
// prediction comes from: the same job's runs, else its repository's, else
// every job's, else DefaultJobMemory. A history error falls through to the
// next level.
func (p *predictor) predict(ctx context.Context, k jobKey) (int64, string) {
	if p == nil {
		return DefaultJobMemory, "default"
	}
	now := p.now()
	p.mu.Lock()
	if c, ok := p.cache[k]; ok && now.Sub(c.at) < predictTTL {
		p.mu.Unlock()
		return c.bytes, c.source
	}
	p.mu.Unlock()
	bytes, source := p.lookup(ctx, k)
	bytes = max(bytes, minJobMemory)
	p.mu.Lock()
	if len(p.cache) > 10_000 {
		clear(p.cache) // bounded: job names come from workflows
	}
	p.cache[k] = prediction{bytes: bytes, source: source, at: now}
	p.mu.Unlock()
	return bytes, source
}

func (p *predictor) lookup(ctx context.Context, k jobKey) (int64, string) {
	try := func(owner, repo, workflow, job string, limit, least int) (int64, bool) {
		s, err := p.hist(ctx, owner, repo, workflow, job, limit)
		if err != nil || len(s) < least {
			return 0, false
		}
		return estimate(s)
	}
	if k.repo != "" && (k.workflow != "" || k.job != "") {
		if v, ok := try(k.owner, k.repo, k.workflow, k.job, jobSamples, 1); ok {
			return v, "job"
		}
	}
	if k.repo != "" {
		if v, ok := try(k.owner, k.repo, "", "", repoSamples, 1); ok {
			return v, "repo"
		}
	}
	if v, ok := try("", "", "", "", globalSamples, minGlobalSamples); ok {
		return v, "global"
	}
	return DefaultJobMemory, "default"
}
