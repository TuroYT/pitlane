package scaler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

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
	meter       *pool.Meter // resources used by its job (nil: not measured)
	predicted   int64       // memory reserved for its job (bytes)
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
	if cfg := t.cfg(); cfg.Strict && !lease.Strict {
		// Switched to strict while the slot was being prepared.
		t.m.pool.Release(context.Background(), lease)
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
	jit, err := client.generateJIT(t.ctx, name, ssID)
	if err != nil {
		return fmt.Errorf("generate JIT config: %w", err)
	}
	if jit.runnerID != 0 {
		// Recorded before the container exists: if Pitlane dies now, the
		// orphan cleanup still finds this registration.
		if err := t.m.store.AddRunner(store.RegisteredRunner{ID: jit.runnerID, TargetID: cfg.ID, Name: name}); err != nil {
			// Not fatal, but the orphan cleanup will never see this runner.
			t.log.Error("record runner registration: it will not be cleaned up if it is orphaned", "runner", name, "id", jit.runnerID, "error", err)
		}
	}
	id, err := t.m.pool.StartRunner(t.ctx, pool.RunnerSpec{Name: name, JITConfig: jit.encoded, TargetID: cfg.ID, Owner: cfg.Owner, Repo: cfg.Repo, Lease: lease})
	if err != nil {
		if jit.runnerID != 0 {
			t.m.forgetRunner(jit.runnerID, client.removeRunner(context.Background(), jit.runnerID))
		}
		return err
	}
	r := &runner{name: name, containerID: id, lease: lease, runnerID: jit.runnerID, startedAt: time.Now(),
		meter: t.m.pool.MeasureJob(t.ctx, id, lease.Slot), predicted: lease.Predicted}
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
	// Before the slot cleanup, which is not part of the job.
	m.recordUsage(r.name, r.meter.Stop())
	if f, err := os.OpenFile(m.LogFile(r.name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		if err := archiveLog(f, maxArchivedLog, func(w io.Writer) error { return m.pool.Logs(ctx, r.containerID, false, w) }); err != nil {
			m.log.Warn("archive runner log", "runner", r.name, "error", err)
		}
		if err := f.Close(); err != nil {
			m.log.Warn("archive runner log", "runner", r.name, "error", err)
		}
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
			m.forgetRunner(r.runnerID, client.removeRunner(ctx, r.runnerID))
		}
	}
	if r.lease.Slot >= 0 { // -1: the container had no valid slot label
		m.pool.Release(ctx, r.lease)
	}
	if t != nil {
		t.mu.Lock()
		delete(t.runners, r.name)
		t.mu.Unlock()
	}
	m.changed()
	m.pool.EnsureFreeDisk(ctx) // idle slots only; skipped while another prune runs
}

// beginJob marks a runner busy with the job GitHub just started on it. It
// reports whether the runner's memory reservation changed: a runner started
// before its job was known reserved what the heaviest pending job needed,
// and now reserves what its own job needs. Callers hold t.mu.
func (r *runner) beginJob(j *scaleset.JobStarted, predicted int64, now time.Time) (refined bool) {
	if !r.busy && r.predicted != predicted {
		r.predicted, refined = predicted, true
	}
	if !r.busy || r.busySince.IsZero() {
		r.busySince = jobStart(j.RunnerAssignTime, r.startedAt, now)
	}
	if !r.busy {
		r.meter.Begin() // usage counts from the job's start, not the warm runner's
	}
	r.busy, r.jobName, r.repo, r.jobID = true, j.JobDisplayName, j.RepositoryName, j.JobID
	return refined
}
