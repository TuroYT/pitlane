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
| `setup-node` / `setup-python` / … | download toolchain | shared `/opt/hostedtoolcache` |
| Docker Hub pulls | rate-limited | local pull-through mirror |
| CPU | 4 vCPU | the whole host when it's the only job |

Slots are chosen by **affinity**: a free slot that last ran the same repository,
then the same owner, then any free slot.

## Caches

| Cache | Scope | Location |
| --- | --- | --- |
| Docker images, layers, buildx cache | per slot | `pitlane-slot-N-docker` volume |
| Toolchains (`setup-*` actions) | global | `pitlane-toolcache` volume → `/opt/hostedtoolcache` |
| npm, pnpm, yarn, bun, pip, Go modules | per owner (or global) | `pitlane-cache-<owner>` volume → `/home/runner/.cache` |
| Docker Hub mirror | global | `pitlane-mirror-data` volume |
| Downloaded actions | per slot | `_work/_actions` in `pitlane-slot-N-work` |

## Lifecycle and restarts

Stopping Pitlane stops the listeners but leaves busy runners running. On start,
Pitlane adopts them, and slots keep their cache across restarts. Disabling a
repository deletes its scale set on GitHub once its running jobs finish.
