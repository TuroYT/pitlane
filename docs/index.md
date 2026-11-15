---
# Home page of the documentation site (https://turoyt.github.io/pitlane/).
# On GitHub, start from the README or getting-started.md instead.
layout: home

hero:
  name: Pitlane
  text: Ephemeral GitHub Actions runners with caches that stay warm
  tagline: One binary, a web UI, no Kubernetes. Each job gets a fresh runner on your own Docker host, and Docker layers, package caches and toolchains stay on disk between jobs.
  image:
    src: /logo.svg
    alt: Pitlane logo
  actions:
    - theme: brand
      text: Get started
      link: /getting-started
    - theme: alt
      text: How it works
      link: /how-it-works
    - theme: alt
      text: GitHub
      link: https://github.com/TuroYT/pitlane

features:
  - title: A fresh runner for every job
    details: Each job gets a brand-new runner. Docker images, build layers, npm/pnpm/yarn/pip/Go caches and toolchains stay on disk between jobs, and actions/cache restores and saves from the Pitlane host.
  - title: Docker works as on GitHub
    details: services, container jobs and docker build work out of the box. Each slot has its own Docker-in-Docker daemon, and jobs never touch the host's Docker.
  - title: Jobs start within seconds
    details: Pitlane long-polls GitHub through the official scale set API, the one GitHub's Kubernetes controller uses. There is no webhook and no public URL to expose.
  - title: A web UI to see what runs where
    details: Live slots and runners, every job traced back to its repository, workflow, branch or pull request, wait and duration trends, runner logs and one-click repository enablement.
  - title: Two-minute setup
    details: Pitlane creates its own GitHub App in one click and uses it to sign you in with GitHub.
  - title: One static binary
    details: Install it with systemd, Docker Compose, or a one-liner on Proxmox.
---

## Quick start

On a Linux host with systemd (Docker is installed if missing):

```sh
curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/install.sh | sudo sh
```

Open the printed URL, enter the setup token (`pitlane setup-token`), click
**Create GitHub App**, install it, enable a repository, and use the label:

```yaml
jobs:
  build:
    runs-on: pitlane
```

Docker Compose and Proxmox installs are in [Getting started](getting-started.md).

## Faster than GitHub-hosted runners

Measured on a real-world private monorepo CI (pnpm workspace, Nuxt + Prisma,
Postgres/Redis service containers, three Docker image builds), all jobs running
in parallel, warm caches. Pitlane ran in an unprivileged Proxmox LXC container
with 12 vCPU and a modest RAID disk.

| | GitHub-hosted (`ubuntu-latest`) | Pitlane |
| --- | --- | --- |
| **Whole CI (longest job)** | **~6 min 30** | **~3 min** |
| Job pickup | queue + VM boot | 6 s |
| Docker images & smoke test | 393 s | **127 s** (3.1×) |
| Dependency install (`setup` step) | 30–40 s | **21–26 s** |
| Prisma migrations | 77 s | **59 s** |
| Lint & format | 40 s | **38 s** |
| Unit tests | 161 s | **159 s** |

CPU-bound steps depend on your hardware: a job on a saturated host is not
faster than on a 4-vCPU GitHub runner. The dashboard compares, for each of your
jobs, the median duration on Pitlane with the same job on GitHub-hosted runners.

## The dashboard

<img class="pl-visual screenshot light-only" src="./assets/dashboard-light.png" alt="Pitlane dashboard: running jobs with their repository, workflow, branch or pull request and links to GitHub, the warm slots, and charts of jobs, slot utilization, queue wait and job duration">
<img class="pl-visual screenshot dark-only" src="./assets/dashboard-dark.png" alt="Pitlane dashboard: running jobs with their repository, workflow, branch or pull request and links to GitHub, the warm slots, and charts of jobs, slot utilization, queue wait and job duration">

<sub>Screenshot with demo data.</sub>

## How it works

<img class="pl-visual light-only" src="./assets/how-it-works-light.svg" alt="GitHub Actions assigns jobs to pitlane over a long-poll; pitlane picks a warm Docker-in-Docker slot and starts a fresh runner that runs one job and is deleted; images, build cache, toolcache, package caches and the Docker Hub mirror are kept between jobs">
<img class="pl-visual dark-only" src="./assets/how-it-works-dark.svg" alt="GitHub Actions assigns jobs to pitlane over a long-poll; pitlane picks a warm Docker-in-Docker slot and starts a fresh runner that runs one job and is deleted; images, build cache, toolcache, package caches and the Docker Hub mirror are kept between jobs">

A **slot** is a long-lived Docker-in-Docker daemon. When GitHub assigns a job to
your `pitlane` label, Pitlane picks a free slot (preferring one that already ran
this repository, so its cache is hottest), starts a fresh runner container in
the slot's network namespace, and lets it run exactly one job. Afterwards, the
job's containers, networks, volumes and workspace are wiped, while images and
build cache stay. Read more in [How it works](how-it-works.md).

Pitlane is young: it provides Linux runners only, and expects rough edges.
Issues and pull requests are welcome.
