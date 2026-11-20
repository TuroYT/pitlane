package scaler

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/actions/scaleset"

	"github.com/TuroYT/pitlane/internal/store"
)

// scaler is the listener's handler of a target's job messages
// (listener.Scaler).
type scaler struct {
	t       *target
	session jobSession
}

// plausible reports whether a timestamp from GitHub or Docker is set: they
// report unknown times as the zero time or the Unix epoch.
func plausible(ts time.Time) bool { return ts.Year() >= 2000 }

// unix converts a message timestamp to Unix seconds, 0 when GitHub left it
// unset.
func unix(ts time.Time) int64 {
	if !plausible(ts) {
		return 0
	}
	return ts.Unix()
}

// unixOrNow is unix with now in place of an unset time.
func unixOrNow(ts time.Time) int64 {
	if u := unix(ts); u != 0 {
		return u
	}
	return time.Now().Unix()
}

// shortRef is the workflow file name of a job's workflow ref.
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

// job is the history row of a job message, without what only some message
// kinds carry.
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

// pendingFor returns the pending entry of a job that just started, or a
// fresh prediction when it was never seen pending (assigned before a
// restart).
func (s *scaler) pendingFor(ctx context.Context, j store.Job) pendingJob {
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

// defaultQueueLookupRetry is how long to wait before asking GitHub again for a job
// it does not list with its runner yet.
const defaultQueueLookupRetry = 3 * time.Second

const (
	queueLookupAttempts = 3
	queueLookupTimeout  = 15 * time.Second
)

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
	for attempt := range queueLookupAttempts {
		if attempt > 0 {
			<-s.t.m.wait(s.t.m.queueLookupRetry)
		}
		ctx, cancel := context.WithTimeout(context.Background(), queueLookupTimeout)
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

// Scale handles a batch of job messages from the listener, then
// reconciles (a nil message only reconciles).
func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	if msg == nil {
		s.t.reconcile()
		return nil
	}
	s.logTimestamps(msg)
	s.jobsAssigned(msg.JobAssignedMessages)
	if err := s.acquireAvailable(ctx, msg.JobAvailableMessages); err != nil {
		return err
	}
	for _, j := range msg.JobStartedMessages {
		s.jobStarted(ctx, j)
	}
	for _, j := range msg.JobCompletedMessages {
		s.jobCompleted(j)
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

// jobsAssigned records the assigned jobs whose queue time is known. Only a
// dated assignment is worth a row: an undated one would never be pruned, and
// the job is recorded when it starts anyway.
func (s *scaler) jobsAssigned(msgs []*scaleset.JobAssigned) {
	for _, j := range msgs {
		if j == nil {
			continue
		}
		if job := s.job(j.JobMessageBase); job.QueuedAt != 0 {
			s.upsertJob(job)
		}
	}
}

// acquireAvailable records the available jobs as pending, with their
// predicted memory, and acquires them for the scale set.
func (s *scaler) acquireAvailable(ctx context.Context, msgs []*scaleset.JobAvailable) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(msgs))
	for _, j := range msgs {
		ids = append(ids, j.RunnerRequestID)
		s.addPending(ctx, j)
	}
	if err := s.session.acquireJobs(ctx, ids); err != nil {
		return fmt.Errorf("acquire jobs: %w", err)
	}
	return nil
}

// addPending records an available job and what it is expected to need.
func (s *scaler) addPending(ctx context.Context, j *scaleset.JobAvailable) {
	job := s.job(j.JobMessageBase)
	if job.QueuedAt == 0 {
		// GitHub did not say when the job was queued: it was just now, near
		// enough (the history and its pruning need a date).
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

// jobStarted binds a started job to its runner and records it.
func (s *scaler) jobStarted(ctx context.Context, j *scaleset.JobStarted) {
	job := s.job(j.JobMessageBase)
	job.RunnerName = j.RunnerName
	job.StartedAt = unixOrNow(j.RunnerAssignTime)
	// QueuedAt stays 0 when unknown: the store keeps the queue time recorded
	// earlier, and a made-up one would fake a wait of 0.
	pj := s.pendingFor(ctx, job)
	refine := -1 // slot whose reservation changes
	s.t.mu.Lock()
	delete(s.t.pending, j.JobID)
	if r, ok := s.t.runners[j.RunnerName]; ok {
		if r.beginJob(j, pj.predicted, time.Now()) {
			refine = r.lease.Slot
		}
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

// jobCompleted records a finished job and forgets it everywhere it waited.
func (s *scaler) jobCompleted(j *scaleset.JobCompleted) {
	job := s.job(j.JobMessageBase)
	job.RunnerName = j.RunnerName
	job.Result = cmp.Or(j.Result, "canceled")
	job.FinishedAt = unixOrNow(j.FinishTime)
	s.t.mu.Lock()
	delete(s.t.pending, j.JobID)
	delete(s.t.lost, j.JobID)
	if r, ok := s.t.runners[j.RunnerName]; ok && r.jobID == j.JobID {
		r.jobID = ""
	}
	s.t.mu.Unlock()
	s.upsertJob(job)
}
