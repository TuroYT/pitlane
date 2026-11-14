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
then the same owner, then any free slot.

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
the least recently restored are evicted beyond the size limit.

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

## Lifecycle and restarts

Stopping Pitlane stops the listeners but leaves busy runners running. On start,
Pitlane adopts them, and slots keep their cache across restarts. A runner whose
repository Pitlane cannot tell (container without a valid target label, or a
repository deleted or disabled meanwhile) is stopped at start, like a job over
its time limit, and its slot is rebuilt before any reuse. Disabling a
repository deletes its scale set on GitHub once its running jobs finish.
