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
curl -fsSL https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/install.sh | sudo sh
```

Or with Docker Compose:

```sh
curl -O https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/compose.yaml
PITLANE_BASE_URL=http://my-server:8080 docker compose up -d
```

Or on Proxmox VE (creates a ready-to-use LXC container):

```sh
bash -c "$(curl -fsSL https://raw.githubusercontent.com/TuroYT/pitlane/main/deploy/proxmox/pitlane-lxc.sh)"
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
