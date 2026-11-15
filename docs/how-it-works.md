# How it works

## Components

- **Listener.** For every enabled repository or organization, Pitlane registers a
  [runner scale set](https://github.com/actions/scaleset) and keeps a long-poll
  session open with GitHub. GitHub pushes "N jobs are assigned to you", and Pitlane
  acquires the jobs and starts N runners. This is the same API that GitHub's
  Kubernetes controller (ARC) uses, so no webhook or public endpoint is needed.
- **Slots.** A slot is a long-lived `docker:dind` container with its own Docker
  data volume. It stays up between jobs, so images, layers and the buildx cache
  are already there when the next job starts. There are as many slots as
  parallel jobs.
- **Runners.** For each job, Pitlane asks GitHub for a just-in-time runner
  configuration and starts a fresh `actions-runner` container that shares the
  slot's network namespace (like a Kubernetes pod), with `DOCKER_HOST` pointing
  at the slot daemon. The runner takes one job and exits.
- **Cleanup.** When the runner exits, Pitlane archives its log, removes it, and
  wipes the job's leftovers in the slot (containers, networks, volumes,
  workspace). Images and build cache are kept, and the slot is ready again.

## Where the speed comes from

| | GitHub-hosted | Pitlane |
| --- | --- | --- |
| Job pickup | queue + VM boot | seconds (instant with warm runners) |
| `services:` images (postgres, redis…) | pulled every job | already in the slot |
| `docker build` | cold, unless you wire a remote cache | incremental, local buildx cache |
| `npm ci` / `pnpm i` / `pip` / Go modules | download, or restore `actions/cache` from blob storage | local disk cache (`~/.cache` volume) |
| `actions/cache`, `setup-*` with `cache:` | Azure blob storage | the Pitlane host's disk |
| `setup-node` / `setup-python` / … | download toolchain | shared `/opt/hostedtoolcache` |
| Docker Hub pulls | rate-limited | local pull-through mirror |
| CPU | 4 vCPU | the whole host when it's the only job |

Slots are chosen by **affinity**: a free slot that last ran the same repository,
then the same owner, then an empty one. With the `owner` cache scope, a slot that
served another owner is rebuilt empty first (its Docker layers and build cache
are lost), so when every free slot needs a rebuild Pitlane takes the one that
costs least:

1. a slot whose cache no one can use (unknown owner after a restart without
   state, left by a strict job);
2. a slot whose owner keeps at least one other warm slot (ready or busy): an
   owner's last warm slot goes last;
3. the least recently used (the time of each slot's last job is kept in
   `slots.json`; a slot without one counts as the oldest), then the lowest
   index.

Among the free slots of its own repository, a job predicted at 1.5 GB or more
(see [Scheduling](#scheduling)) takes the most recently used one, whose cache is
the freshest, and a lighter job the least recently used one, leaving the warm
ones to the heavy jobs. A job without prediction takes the lowest index.

Strict jobs (public repositories) use the [slots reserved for
them](#slots-reserved-for-strict-jobs) when there are any. Without reserved
slots, they take an empty slot first, then follow the same order. When the
only choice is to wipe a warm slot while another slot is being cleaned after a
strict job, the strict job waits for that slot, for at most 20 seconds: the
cleanup is the same rebuild the warm slot would need, and it is already under
way, so the job does not start later than it would have.
Each rebuild before a job is logged (`rebuilding slot before its job`, with the
previous owner and why) and counted in `pitlane_slot_wipes_total{reason}`
(`owner_change`, `strict`, `unknown_owner`).

## Scheduling

A job starts when a slot is free and, with **memory admission** on (the
default), when its predicted memory fits next to the running jobs. A 12-slot
host with 23 GB of memory went down when a burst of builds ran at once:
admission lets you keep as many slots as CPU allows while memory decides how
many jobs run together.

### Memory prediction

When GitHub assigns a job, Pitlane predicts its memory from the jobs it already
measured (see [resource usage per job](#resource-usage-per-job)). The memory of
a past job is its runner's peak plus its slot's peak **anonymous** memory
(`memory.stat` `anon`, sampled every 2 seconds): a slot's `memory.peak` also
counts the page cache filled by image layers and build contexts, which the
kernel gives back under pressure. Jobs measured before anonymous memory was
recorded count a quarter of their slot's `memory.peak` instead. Cancelled jobs
are left out.

1. The same job (repository, workflow file and job name): the 90th percentile
   of its last 20 runs, or the highest run while it has fewer than 5.
2. A job never measured: the same over the last 50 jobs of its repository.
3. A new repository: the last 200 jobs of every repository (at least 5).
4. Nothing measured yet: 2 GB.

Predictions are at least 256 MB and are cached for a minute.

### Admission

The **budget** is the host's memory (`MemTotal` of `/proc/meminfo`, which is the
LXC container's limit under lxcfs, lowered to the cgroup's `memory.max` when
Pitlane does not run in a Docker container) minus the **memory reserve** (2 GB by
default, at most half the memory) kept for the system, Docker, Pitlane and the
page cache.

Each slot handed out reserves the prediction of the job it starts for. GitHub
decides which idle runner gets which job, so new runners reserve the heaviest
pending jobs first; a warm runner reserves the prediction of its repository (or
of every job, for an organization). When the job starts, the reservation
becomes that job's prediction. A running job counts for the larger of its
reservation and what it uses now (runner `memory.current` plus the slot's
anonymous memory). A new job starts when this total plus its prediction fits
the budget.

- **No deadlock.** A job is always admitted when no other job runs, even when
  its prediction alone is above the budget.
- **No starvation.** The oldest job refused for memory keeps its place: a job of
  another target starts only if it leaves room for it. A target that gets a
  runner and still has jobs waiting goes behind the other waiting targets, so
  targets take turns.
- **Failure modes.** A job that suddenly needs more than its history says is
  not stopped: the next jobs see its real use and wait. A first run, or a new
  repository, is predicted from the others, then 2 GB. When the host's memory
  cannot be read, only the slot count limits jobs. After a Pitlane restart,
  adopted runners reserve their repository's prediction.

Waiting jobs show their reason on the dashboard and the Jobs page (`waiting for
memory`, `waiting for a slot`, `waiting for a strict slot`) and in
`pitlane_jobs_waiting{reason}`. The dashboard's **memory budget** meter shows
what is committed, predicted and used against the budget, and each busy slot
shows its job's current use against its prediction.

### Slots reserved for strict jobs

A strict job (public repository) rebuilds its slot before and after it runs.
On a busy host, the only free slot is often another repository's warm one, and
the strict job wipes it. With **reserved slots for strict jobs** (one by default
while a strict target is enabled, none otherwise; set 0 to N−1 in Settings):

- non-strict jobs never use the reserved slots (the highest slot numbers,
  marked `strict` on the dashboard);
- strict jobs use them first, then overflow to a slot with nothing to lose
  (empty, or last used by a strict job or an unknown owner), then to a warm
  slot whose owner keeps another warm one (least recently used first);
- they never wipe an owner's last warm slot: then they wait (`waiting for a
  strict slot`) until a reserved slot frees up. They also wait, at most 20 s,
  while a slot is being rebuilt after a strict job, since it comes back clean.

The trade-off: private repositories have one slot less, and when several
strict jobs run at once (a CI with parallel jobs), the extra ones each cost
another owner one warm slot. Set 0 to get the previous behaviour back (strict
jobs take any slot, cheapest wipe first).

## Caches

| Cache | Scope | Location |
| --- | --- | --- |
| Docker images, layers, buildx cache | per slot | `pitlane-slot-N-docker` volume |
| Toolchains (`setup-*` actions) | per owner (or global) | `pitlane-toolcache-<owner>` volume → `/opt/hostedtoolcache` |
| npm, yarn, bun, pip, Go modules | per owner (or global) | `pitlane-cache-<owner>` volume → `/home/runner/.cache` |
| pnpm store | per owner (or global), hardlinked into each slot | `pitlane-cache-<owner>.pnpm` volume (never mounted) ↔ `_work/.pnpm-store` in `pitlane-slot-N-work` |
| `actions/cache` entries | per repository and branch, within one owner (strict jobs: GitHub's cache) | `<data dir>/actions-cache` |
| Docker Hub mirror | global | `pitlane-mirror-data` volume |
| Downloaded actions | per slot (wiped when the slot changes owner) | `_work/_actions` in `pitlane-slot-N-work` |

pnpm only hardlinks packages into `node_modules` from a store on the same
mount as the workspace, so each slot keeps its store in its work volume. After
each job Pitlane merges it with the owner's canonical store (new packages go
up, missing ones come down, duplicate copies become hardlinks of the canonical
file). All slots of an owner then share the same inodes: the store is cached in
RAM once instead of once per slot, which is what keeps `pnpm install` fast when
several jobs install at the same time on a slow disk. Strict jobs never touch
it, and a slot changing owner drops its store before its first job.

## Actions cache

`actions/cache` and every action built on its toolkit (`setup-node`,
`setup-python`, `setup-go`, `setup-java` with `cache:`, `ruby/setup-ruby`…)
restore and save from the Pitlane host instead of GitHub's cache, without any
workflow change. Pitlane copies each runner image's Node runtimes into the slot
and puts a small shim in front of them: for jobs Pitlane gave a cache URL, it
points the toolkit's `ACTIONS_CACHE_URL` at Pitlane and makes it use its v1
protocol; artifacts, OIDC and logs still go to GitHub. Lookups follow GitHub's
rules (exact key, then prefixes and `restore-keys` in order, newest first; the
job's branch, then the base and default branches), entries are immutable, and
the least recently restored are evicted beyond the size limit. One owner may
use at most a share of that size (half by default): beyond it, its own entries
are evicted first.

- Each runner gets its own token, valid until its job ends; it only opens the
  caches of the target's owner (one repository for a repository target).
- Strict jobs, jobs in a `container:`, and every job when the setting is off or
  the listener is down use GitHub's cache as before. If Pitlane's cache fails
  during a job, the toolkit only logs a warning and the job goes on.
- The design and its trade-offs are in [design/actions-cache.md](design/actions-cache.md).

## Tips for the fastest CI

- **`cache:` in `setup-*` actions is not needed on Pitlane**: the package
  managers' own caches (`~/.cache`, the pnpm store, the tool cache) are already
  warm on local disk, which is faster than archiving and extracting them. If you
  keep it (workflows shared with GitHub-hosted runners), it is served from the
  Pitlane host, so it costs seconds instead of the 30–60 s of an upload to GitHub.
- Prefer `docker buildx build` without `cache-from: type=gha`: the slot's local
  build cache is faster.
- With pnpm, keep the default store location (do not set `store-dir`) and do
  not cache it with `actions/cache` / `cache: pnpm`: Pitlane points
  `npm_config_store_dir` at the shared, hardlinked store.
- New repositories get one **warm runner** (Repositories → Warm runners), so
  their jobs start in about a second. Set it to 0 for repositories that rarely
  run jobs, so their slot is free for the others.

## Resource usage per job

Pitlane measures each job from cgroup v2, in both containers it uses: the
runner and its slot (the slot daemon runs `services:` and `docker build`).
Jobs table columns: **Peak RAM** (runner peak + slot peak), **CPU time** and
**Pressure** (share of the job's time stalled waiting for a CPU, I/O or memory,
from Linux PSI; the worse of runner and slot, both in the tooltip). The jobs API
returns the same data in `usage` (`null` when not measured).

- **Window.** From the job's start (GitHub's `JobStarted`; the runner's start
  when it was missed) to the runner's exit. The slot cleanup that follows is
  not counted.
- **Peak memory** is `memory.peak`, page cache included. The runner's covers
  its whole life (a warm runner waits with little memory). A slot lives for
  many jobs, so its `memory.peak` is reset at the job's start by writing to it
  (Linux 6.12 and later, the reset applies to Pitlane's open file); on older
  kernels, or when the file is not writable, the slot's peak is the highest
  `memory.current` sampled every 2 seconds, which can miss short spikes. Both
  peaks need not coincide: their sum is an upper bound.
- **CPU time** (`cpu.stat` `usage_usec`) and **pressure** (`total=` of
  `cpu.pressure` "some", `io.pressure` "full", `memory.pressure` "full") are
  deltas over the window. The runner's cgroup disappears when it exits, so its
  last reading is at most 2 seconds old.
- **When nothing is recorded.** Pitlane finds a container's cgroup from its
  main process (`/proc/<pid>/cgroup`) or the standard layouts
  (`/sys/fs/cgroup/system.slice/docker-<id>.scope`, `/sys/fs/cgroup/docker/<id>`).
  On cgroup v1, Docker Desktop, rootless Docker, or Pitlane in a container
  without the host's `/sys/fs/cgroup`, jobs simply have no usage; a job never
  fails because of it. Runners adopted after a Pitlane restart are not measured.

The dashboard's **Memory per job** panel shows the p50, p90 and max peak over
the last 7 days and suggests `floor(usable memory / p90)` slots, usable memory
being the admission's budget (the host's memory minus the memory reserve),
next to the configured slot count and memory limit. Memory, not CPU, is usually what limits parallel jobs:
a job whose memory is near the limit, or whose memory pressure is high, needs
fewer slots or more RAM. `GET /api/jobs/memory` serves the same data.

## Lifecycle and restarts

Stopping Pitlane stops the listeners but leaves busy runners running. On start,
Pitlane adopts them, and slots keep their cache across restarts. A runner whose
repository Pitlane cannot tell (container without a valid target label, or a
repository deleted or disabled meanwhile) is stopped at start, like a job over
its time limit, and its slot is rebuilt before any reuse. Disabling a
repository deletes its scale set on GitHub once its running jobs finish.
