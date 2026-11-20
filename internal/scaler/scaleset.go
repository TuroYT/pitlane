package scaler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// This file is the only one calling github.com/actions/scaleset (a public
// preview library) and the pool: the rest of the package goes through the
// small interfaces below, so API changes stay here and tests use fakes.
// Message payloads (scaleset.RunnerScaleSetMessage and the job messages) are
// plain data and are used as is by the listener handler.

// Pool is what the scaler uses of the Docker slot pool (*pool.Pool).
type Pool interface {
	RunnerContainers(ctx context.Context) ([]pool.Orphan, error)
	RunnerNames(ctx context.Context) (map[string]bool, error)
	Released() <-chan struct{}
	// Admit reserves a slot for a job, or returns why it must wait.
	Admit(ctx context.Context, r pool.Request) (pool.Lease, string)
	// SetPrediction refines the memory reserved for a leased slot's job.
	SetPrediction(slot int, bytes int64)
	// SetStrictDemand says whether a strict target is enabled.
	SetStrictDemand(on bool)
	Prepare(ctx context.Context, l pool.Lease) error
	Release(ctx context.Context, l pool.Lease)
	StartRunner(ctx context.Context, spec pool.RunnerSpec) (string, error)
	Wait(ctx context.Context, id string) int64
	Logs(ctx context.Context, id string, follow bool, w io.Writer) error
	Remove(ctx context.Context, id string)
	StopRunner(ctx context.Context, id string, grace time.Duration) error
	EnsureFreeDisk(ctx context.Context)
	// MeasureJob starts measuring a runner's job (nil: not measurable).
	MeasureJob(ctx context.Context, runnerID string, slot int) *pool.Meter
}

var _ Pool = (*pool.Pool)(nil)

// scaleSetAPI is what a target uses of the GitHub scale set API.
type scaleSetAPI interface {
	// ensureScaleSet returns the ID of the scale set named name in
	// RunnerGroupID, creating it (labelled with its name) when missing.
	ensureScaleSet(ctx context.Context, name string) (id int, created bool, err error)
	// openSession opens the message session of a scale set.
	openSession(ctx context.Context, scaleSetID int, owner string) (jobSession, error)
	// generateJIT registers a runner and returns its JIT configuration.
	generateJIT(ctx context.Context, name string, scaleSetID int) (jitConfig, error)
	removeRunner(ctx context.Context, id int64) error
	// getRunner reports whether GitHub still knows a runner (an error
	// matching isNotFound when it does not).
	getRunner(ctx context.Context, id int64) error
	deleteScaleSet(ctx context.Context, id int) error
}

// jobSession is a message session: it acquires jobs and runs the listener.
type jobSession interface {
	acquireJobs(ctx context.Context, requestIDs []int64) error
	// run delivers job messages to s until ctx ends or the session fails.
	run(ctx context.Context, scaleSetID, maxRunners int, log *slog.Logger, s listener.Scaler) error
	// setMaxRunners updates the capacity announced by a running listener.
	setMaxRunners(n int)
	close(ctx context.Context) error
}

// jitConfig is a registered runner's configuration.
type jitConfig struct {
	encoded  string
	runnerID int64 // 0 when GitHub did not return the registration
}

// newScaleSetAPI connects to the scale set API of a target.
func newScaleSetAPI(creds Credentials, cfg store.Target, version string) (scaleSetAPI, error) {
	info := scaleset.SystemInfo{System: "pitlane", Version: version, Subsystem: "listener", ScaleSetID: cfg.ScaleSetID}
	var (
		c   *scaleset.Client
		err error
	)
	switch {
	case creds.AppClientID != "" && creds.AppPEM != "" && cfg.InstallationID != 0:
		c, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.URL(),
			GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: creds.AppClientID, InstallationID: cfg.InstallationID, PrivateKey: creds.AppPEM},
			SystemInfo:      info,
		})
	case creds.PAT != "":
		c, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL: cfg.URL(), PersonalAccessToken: creds.PAT, SystemInfo: info,
		})
	default:
		return nil, errors.New("no GitHub credentials for this target (install the GitHub App on " + cfg.Owner + ")")
	}
	if err != nil {
		return nil, err
	}
	return &ghScaleSet{c: c, version: version}, nil
}

// ghScaleSet is the real scaleSetAPI.
type ghScaleSet struct {
	c       *scaleset.Client
	version string
}

func (g *ghScaleSet) ensureScaleSet(ctx context.Context, name string) (int, bool, error) {
	ss, err := g.c.GetRunnerScaleSet(ctx, RunnerGroupID, name)
	if err != nil {
		return 0, false, fmt.Errorf("get scale set: %w", err)
	}
	created := false
	if ss == nil {
		ss, err = g.c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: RunnerGroupID,
			Labels:        []scaleset.Label{{Name: name}},
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return 0, false, fmt.Errorf("create scale set: %w", err)
		}
		created = true
	}
	g.c.SetSystemInfo(scaleset.SystemInfo{System: "pitlane", Version: g.version, Subsystem: "listener", ScaleSetID: ss.ID})
	return ss.ID, created, nil
}

func (g *ghScaleSet) openSession(ctx context.Context, scaleSetID int, owner string) (jobSession, error) {
	s, err := g.c.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	return &ghSession{s: s}, nil
}

func (g *ghScaleSet) generateJIT(ctx context.Context, name string, scaleSetID int) (jitConfig, error) {
	jit, err := g.c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, scaleSetID)
	if err != nil {
		return jitConfig{}, err
	}
	out := jitConfig{encoded: jit.EncodedJITConfig}
	if jit.Runner != nil {
		out.runnerID = int64(jit.Runner.ID)
	}
	return out, nil
}

func (g *ghScaleSet) removeRunner(ctx context.Context, id int64) error {
	return g.c.RemoveRunner(ctx, id)
}

func (g *ghScaleSet) getRunner(ctx context.Context, id int64) error {
	_, err := g.c.GetRunner(ctx, int(id))
	return err
}

func (g *ghScaleSet) deleteScaleSet(ctx context.Context, id int) error {
	return g.c.DeleteRunnerScaleSet(ctx, id)
}

// ghSession is the real jobSession.
type ghSession struct {
	s   *scaleset.MessageSessionClient
	mu  sync.Mutex
	lst *listener.Listener
}

func (g *ghSession) acquireJobs(ctx context.Context, ids []int64) error {
	_, err := g.s.AcquireJobs(ctx, ids)
	return err
}

func (g *ghSession) run(ctx context.Context, scaleSetID, maxRunners int, log *slog.Logger, s listener.Scaler) error {
	lst, err := listener.New(g.s, listener.Config{ScaleSetID: scaleSetID, MaxRunners: maxRunners, Logger: log})
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.lst = lst
	g.mu.Unlock()
	return lst.Run(ctx, s)
}

func (g *ghSession) setMaxRunners(n int) {
	g.mu.Lock()
	lst := g.lst
	g.mu.Unlock()
	if lst != nil {
		lst.SetMaxRunners(n)
	}
}

func (g *ghSession) close(ctx context.Context) error { return g.s.Close(ctx) }

func isNotFound(err error) bool {
	return errors.Is(err, scaleset.NotFoundError) || errors.Is(err, scaleset.RunnerNotFoundError)
}

// Credentials used to talk to GitHub. Either the App fields or PAT is set.
type Credentials struct {
	AppClientID string
	AppPEM      string
	PAT         string
}

// RunnerGroupID is the runner group of every scale set Pitlane creates: the
// default group (for an organisation, its "Allow public repositories"
// setting decides whether public repositories can use the runners).
const RunnerGroupID = 1

// Valid reports whether credentials are usable.
func (c Credentials) Valid() bool { return c.PAT != "" || (c.AppClientID != "" && c.AppPEM != "") }
