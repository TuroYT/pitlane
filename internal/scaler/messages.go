package scaler

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
	"github.com/actions/scaleset"
)

type scaler struct {
	t       *target
	session jobSession
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

// pendingJob is a job GitHub assigned to the scale set that no runner has
// started yet.
type pendingJob struct {
	key       jobKey
	predicted int64
	at        time.Time
}

// pendingTTL forgets a pending job whose start or completion was never seen.
const pendingTTL = 24 * time.Hour

func (s *scaler) job(b scaleset.JobMessageBase) store.Job {
	cfg := s.t.cfg()
	queued := unix(b.QueueTime)
	if queued == 0 {
		// Not every message carries queueTime; the scale set assignment is
		// the closest real date.
		queued = unix(b.ScaleSetAssignTime)
	}
	return store.Job{TargetID: cfg.ID, JobID: b.JobID, RunID: b.WorkflowRunID, Owner: b.OwnerName, Repo: b.RepositoryName,
		WorkflowRef: shortRef(b.JobWorkflowRef), WorkflowFullRef: b.JobWorkflowRef, JobName: b.JobDisplayName, Event: b.EventName, Slot: -1, QueuedAt: queued}
}

// upsertJob records a job in the history; a failure only affects the UI and
// the lost-job detection after a restart, so it is logged.
func (s *scaler) upsertJob(j store.Job) {
	if err := s.t.m.store.UpsertJob(j); err != nil {
		s.t.log.Warn("record job", "job", j.JobID, "error", err)
	}
}

// keyOf names a job for the memory prediction.
func keyOf(j store.Job) jobKey {
	return jobKey{owner: j.Owner, repo: j.Repo, workflow: j.WorkflowRef, job: j.JobName}
}

// pendingJob returns the pending entry of a job that just started, or a
// fresh prediction when it was never seen pending (assigned before a
// restart).
func (s *scaler) pendingJob(ctx context.Context, j store.Job) pendingJob {
	s.t.mu.Lock()
	pj, ok := s.t.pending[j.JobID]
	s.t.mu.Unlock()
	if ok {
		return pj
	}
	k := keyOf(j)
	pred, _ := s.t.m.predict.predict(ctx, k)
	return pendingJob{key: k, predicted: pred, at: time.Now()}
}

// pendingPredictions prunes the pending jobs older than pendingTTL and
// returns the predictions of the others, heaviest first. Callers hold t.mu.
func pendingPredictions(pending map[string]pendingJob, now time.Time) []int64 {
	out := make([]int64, 0, len(pending))
	for id, pj := range pending {
		if now.Sub(pj.at) > pendingTTL {
			delete(pending, id)
			continue
		}
		out = append(out, pj.predicted)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

// logTimestamps shows what GitHub sent for each job message, to see which
// timestamps are missing.
func (s *scaler) logTimestamps(msg *scaleset.RunnerScaleSetMessage) {
	log := func(kind string, b scaleset.JobMessageBase) {
		s.t.log.Debug("job message", "type", kind, "job", b.JobID, "queue", unix(b.QueueTime), "scaleSetAssign", unix(b.ScaleSetAssignTime),
			"runnerAssign", unix(b.RunnerAssignTime), "finish", unix(b.FinishTime))
	}
	for _, j := range msg.JobAvailableMessages {
		log("available", j.JobMessageBase)
	}
	for _, j := range msg.JobAssignedMessages {
		if j != nil {
			log("assigned", j.JobMessageBase)
		}
	}
	for _, j := range msg.JobStartedMessages {
		log("started", j.JobMessageBase)
	}
	for _, j := range msg.JobCompletedMessages {
		log("completed", j.JobMessageBase)
	}
}

// queueLookupRetry is how long to wait before asking GitHub again for a job
// it does not list with its runner yet.
var queueLookupRetry = 3 * time.Second

// fetchQueueTime records the queue time of a job that started without one.
// A warm runner can take a job that never produced a JobAvailable message,
// and GitHub does not always put queueTime in JobStarted; the REST API knows
// when the job was created. On failure the assignment time recorded earlier
// stays, which is only an upper bound.
func (s *scaler) fetchQueueTime(job store.Job) {
	lookup := s.t.m.JobQueuedAt
	if lookup == nil || job.RunID == 0 || job.RunnerName == "" {
		return
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(queueLookupRetry)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		var at time.Time
		at, err = lookup(ctx, s.t.cfg(), job.Owner, job.Repo, job.RunID, job.RunnerName)
		cancel()
		if err == nil {
			if q := unix(at); q != 0 {
				s.upsertJob(store.Job{TargetID: job.TargetID, JobID: job.JobID, Slot: -1, QueuedAt: q})
			}
			return
		}
	}
	s.t.log.Debug("queue time lookup failed", "job", job.JobID, "error", err)
}

func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		s.t.reconcile()
		return nil
	}
	s.logTimestamps(msg)
	for _, j := range msg.JobAssignedMessages {
		// Only a dated assignment is worth a row: an undated one would never
		// be pruned, and the job is recorded when it starts anyway.
		if j == nil {
			continue
		}
		if job := s.job(j.JobMessageBase); job.QueuedAt != 0 {
			s.upsertJob(job)
		}
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
			job := s.job(j.JobMessageBase)
			if job.QueuedAt == 0 {
				// GitHub did not say when the job was queued: it was just
				// now, near enough (the history and its pruning need a date).
				job.QueuedAt = time.Now().Unix()
			}
			s.upsertJob(job)
			key := keyOf(job)
			pred, source := s.t.m.predict.predict(ctx, key)
			s.t.log.Debug("job memory predicted", "job", job.JobName, "repo", job.Repo, "bytes", pred, "from", source)
			s.t.mu.Lock()
			s.t.pending[job.JobID] = pendingJob{key: key, predicted: pred, at: time.Now()}
			s.t.mu.Unlock()
		}
		if err := s.session.acquireJobs(ctx, ids); err != nil {
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
		// QueuedAt stays 0 when unknown: the store keeps the queue time
		// recorded earlier, and a made-up one would fake a wait of 0.
		pj := s.pendingJob(ctx, job)
		s.t.mu.Lock()
		delete(s.t.pending, j.JobID)
		refine := -1
		if r, ok := s.t.runners[j.RunnerName]; ok {
			if !r.busy && r.predicted != pj.predicted {
				// The job is known now: reserve what it needs, not what
				// the heaviest pending job needed.
				r.predicted, refine = pj.predicted, r.lease.Slot
			}
			if !r.busy || r.busySince.IsZero() {
				r.busySince = jobStart(j.RunnerAssignTime, r.startedAt, time.Now())
			}
			if !r.busy {
				r.meter.Begin() // usage counts from the job's start, not the warm runner's
			}
			r.busy, r.jobName, r.repo, r.jobID = true, j.JobDisplayName, j.RepositoryName, j.JobID
			job.Slot = r.lease.Slot
		}
		if l, ok := s.t.lost[j.JobID]; ok && l.runner != j.RunnerName {
			delete(s.t.lost, j.JobID) // started again elsewhere: it needs a runner
		}
		s.t.mu.Unlock()
		if refine >= 0 {
			s.t.m.pool.SetPrediction(refine, pj.predicted)
		}
		s.upsertJob(job)
		if j.QueueTime.IsZero() {
			go s.fetchQueueTime(job)
		}
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
		delete(s.t.pending, j.JobID)
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
