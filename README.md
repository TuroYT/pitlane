<p align="center">
  <img src="docs/logo.svg" width="64" height="64" alt="">
</p>

<h1 align="center">Pitlane</h1>

<p align="center">
  <b>Fast, ephemeral, autoscaled self-hosted GitHub Actions runners. One binary, a web UI, no Kubernetes.</b>
</p>

---

Pitlane turns any Docker host (a VM, a Proxmox LXC, a spare server) into a pool of
self-hosted GitHub Actions runners that **start on demand, run one job, and
disappear**, while **keeping every cache warm** so your CI is faster than on
GitHub-hosted runners.

- **Ephemeral runners, warm caches.** Every job gets a brand-new runner. Docker images, build layers, `npm`/`pnpm`/`yarn`/`pip`/Go caches and toolchains stay on disk between jobs.
- **Docker that just works.** `services:`, `container:` jobs and `docker build` work out of the box. Each slot has its own Docker-in-Docker daemon, and jobs never touch the host's Docker.
- **Autoscaling with GitHub's official scale set API.** No webhook and no public URL: Pitlane long-polls GitHub, and jobs start within seconds.
- **Web UI.** Live slots and runners, job history with wait time and duration, runner logs, one-click repository enablement, settings applied live.
- **Two-minute setup.** Pitlane creates its own GitHub App in one click and uses it to sign you in with GitHub.
- **Single static binary.** Install it with systemd, Docker Compose, or a one-liner on Proxmox.

## Quick start

On a Linux host with systemd (Docker is installed if missing):

```sh
curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/install.sh | sudo sh
```

Or with Docker Compose:

```sh
curl -O https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/compose.yaml
PITLANE_BASE_URL=http://my-server:8080 docker compose up -d
```

Or on Proxmox VE (creates a ready-to-use LXC container):

```sh
bash -c "$(curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/pitlane-lxc.sh)"
```

Then:

1. Open the printed URL and enter the setup token (`pitlane setup-token`).
2. Click **Create GitHub App**, then install the App on your account or organization.
3. Sign in with GitHub, go to **Repositories** and click **Enable**.
4. In your workflows:

   ```yaml
   jobs:
     build:
       runs-on: pitlane
   ```

That's it. See [Getting started](docs/getting-started.md) for details.

## Faster than GitHub-hosted runners

Measured on a real-world private monorepo CI (pnpm workspace, Nuxt + Prisma, Postgres/Redis
service containers, three Docker image builds), all jobs running in parallel, warm caches.
Pitlane ran in an unprivileged Proxmox LXC container with 12 vCPU and a modest RAID disk.

| | GitHub-hosted (`ubuntu-latest`) | Pitlane |
| --- | --- | --- |
| **Whole CI (longest job)** | **~6 min 30** | **~3 min** |
| Job pickup | queue + VM boot | 6 s |
| Docker images & smoke test | 393 s | **127 s** (3.1×) |
| Dependency install (`setup` step) | 30–40 s | **21–26 s** |
| Prisma migrations | 77 s | **59 s** |
| Lint & format | 40 s | **38 s** |
| Unit tests | 161 s | **159 s** |

The biggest wins come from what stays warm between jobs: Docker layers and `services:`
images in each slot, one hardlinked pnpm store per owner, the tool cache and a local Docker
Hub mirror. CPU-bound steps (type-checking, bundling) depend on your hardware: a job on a
saturated host is not faster than on a 4-vCPU GitHub runner.

On a project that previously used fixed self-hosted runners, the same jobs went from
705 s → 250 s (lint), 417 s → 210 s (tests) and 376 s → 165 s (build).

The **Jobs** page of the UI compares, for each of your jobs, the median duration on Pitlane
with the same job on GitHub-hosted runners, so you can check on your own workloads.

## How it works

```
GitHub ──(scale set long-poll)──▶ pitlane ──▶ slot #0  [dind daemon, warm cache] ◀── runner (one job, then deleted)
                                          ├─▶ slot #1  [dind daemon, warm cache] ◀── runner
                                          └─▶ …        shared: toolcache, package caches, Docker Hub mirror
```

A **slot** is a long-lived Docker-in-Docker daemon. When GitHub assigns a job to
your `pitlane` label, Pitlane picks a free slot (preferring one that already ran
this repository, so its cache is hottest), starts a fresh runner container in the
slot's network namespace, and lets it run exactly one job. Afterwards, the job's
containers, networks, volumes and workspace are wiped, while images and build cache
stay. Read more in [How it works](docs/how-it-works.md).

## Documentation

- [Getting started](docs/getting-started.md)
- [Configuration](docs/configuration.md)
- [How it works & performance](docs/how-it-works.md)
- [Security](docs/security.md)
- [Troubleshooting](docs/troubleshooting.md)

## Status

Pitlane is young (v0.1). It is used in production by its author, but expect rough
edges. Issues and pull requests are welcome.

## License

[MIT](LICENSE)
