# Why Pitlane

Pitlane is for one situation: you run GitHub Actions for private repositories,
you have a Linux machine to spare (a VM, a Proxmox LXC container, a server), and
you want each job to get a fresh runner without starting from cold caches every
time, and without running Kubernetes to get there.

## What you get

On a real private monorepo (pnpm workspace, Nuxt + Prisma, Postgres and Redis
services, three Docker image builds), with Pitlane in an unprivileged Proxmox LXC
container with 12 vCPU:

| | GitHub-hosted (`ubuntu-latest`) | Pitlane |
| --- | --- | --- |
| Whole CI (longest job) | ~6 min 30 | ~3 min |
| Docker images & smoke test | 393 s | 127 s |
| `actions/cache` save, 200 MB | about 9 s | about 1 s |

The speed comes from what a slot keeps between jobs: its Docker daemon with the
images and build cache, the package manager caches and toolchains of the GitHub
owner, the `actions/cache` entries on local disk and a Docker Hub mirror. Steps
that only burn CPU (type-checking, unit tests) run at the speed of your hardware:
in the same benchmark, lint went from 40 s to 38 s and unit tests from 161 s to
159 s. The full table is in the [README](https://github.com/TuroYT/pitlane#faster-than-github-hosted-runners).

## Compared with the alternatives

Facts below come from each project's README or documentation, checked on
15 November 2026. If one of them is wrong or out of date, please
[open an issue](https://github.com/TuroYT/pitlane/issues).

| | Runs on | Inbound URL | Kept between jobs | Web UI | Runner OS | License |
| --- | --- | --- | --- | --- | --- | --- |
| **Pitlane** | one Docker host | not needed | Docker layers and images, package caches, toolchains, `actions/cache` | yes, with roles | Linux | MIT |
| actions-runner-controller | Kubernetes | not needed | what you mount yourself | no | Linux | Apache-2.0 |
| runscaler | one Docker host, or Tart VMs on Apple Silicon | not needed | opt-in `actions/cache` receiver, optional shared volume | no (CLI) | Linux, macOS | MIT |
| GARM | a controller and a provider | webhooks for pools, not needed for scale sets | depends on the provider and image | yes | depends on the provider | Apache-2.0 |
| terraform-aws-github-runner | your AWS account | GitHub webhook to API Gateway | nothing on ephemeral instances by default | no | Linux, Windows, macOS | MIT |
| Hosted runners | the provider's machines | not needed | provider features | the provider's | Linux, others by provider | commercial |

### actions-runner-controller (ARC)

[ARC](https://github.com/actions/actions-runner-controller) is GitHub's own
controller, installed with Helm on a Kubernetes cluster. Its runner scale sets
use the same API as Pitlane, so neither needs a webhook. Pods start fresh, and
anything you want to keep between jobs (a Docker layer cache, a package cache)
is a volume you set up yourself.

**Pick ARC** if you already run Kubernetes and want runners managed like your
other workloads, with Kubernetes doing the scheduling across many nodes.
**Pick Pitlane** if you would otherwise run a cluster only for CI.

### runscaler

[runscaler](https://github.com/ysya/runscaler) is the closest project to
Pitlane: a single Go binary on a Docker host, built on the same
`actions/scaleset` library, with a fresh container per job. It can also run
macOS and Linux VMs with Tart on Apple Silicon Macs. It is driven by a TOML file
and a CLI, and can opt into a built-in `actions/cache` receiver.

**Pick runscaler** if you need macOS runners, or prefer configuration files to a
web UI. **Pick Pitlane** for warm Docker-in-Docker daemons per slot (images,
layers and `services:` images stay between jobs), per-owner package caches, a
web UI with roles and per-job resource usage, and strict mode for public
repositories.

### GARM

[GARM](https://github.com/cloudbase/garm) manages pools of runners through
providers: LXD, Incus, Kubernetes, OpenStack, AWS, Azure, GCP, Oracle and
others, each a separate executable. It has a web UI, supports Gitea and GitHub
Enterprise Server, and can use webhooks or scale sets.

**Pick GARM** if you need runners on several clouds, VMs per job, Gitea, or
GitHub Enterprise Server. **Pick Pitlane** if one Docker host is enough and you
want caches that stay warm without building runner images per provider.

### terraform-aws-github-runner

[terraform-aws-github-runner](https://github.com/github-aws-runners/terraform-aws-github-runner)
(started by Philips Labs) deploys runners on EC2, often spot instances, with
Lambda functions that scale them on GitHub webhooks. It supports Linux, Windows
and macOS, and ephemeral instances are terminated after their job.

**Pick it** if your CI belongs in AWS and you want an EC2 instance per job.
**Pick Pitlane** if you have a machine of your own and want its disk to keep
the caches.

### Hosted runners and RunsOn

[Blacksmith](https://www.blacksmith.sh/), [Depot](https://depot.dev/),
[Namespace](https://namespace.so/), [Ubicloud](https://www.ubicloud.com/) and
[WarpBuild](https://www.warpbuild.com/) sell runners billed per minute, advertised
as cheaper or faster than GitHub-hosted ones, with their own caching (Blacksmith,
for one, keeps Docker layers on persistent disks billed per GB). You change
`runs-on:` and own no hardware. [RunsOn](https://runs-on.com/) runs in your own
AWS account, free for non-commercial use and licensed for commercial use.

**Pick a hosted service** if you do not want to own, patch and secure a machine,
need Windows or macOS, or run untrusted code from forks. **Pick Pitlane** if you
already have the hardware and would rather not pay per minute.

## When Pitlane is the wrong choice

- **Windows or macOS jobs.** Pitlane only runs Linux runners (amd64, arm64).
- **Untrusted code.** Every job controls a privileged Docker daemon and can take
  over the host. Strict mode keeps honest jobs apart; it does not contain a
  malicious one. See [Security](security.md).
- **GitHub Enterprise Server.** Pitlane talks to GitHub.com only.
- **A fleet of machines.** A Pitlane instance drives one Docker host. Scheduling
  jobs across many nodes is what ARC on Kubernetes or GARM are built for.
- **No time to run a host.** Self-hosting means updates, disk space and a
  machine that can fail. Pitlane updates itself and prunes its caches, but the
  host is still yours.

## What it costs

Pitlane is MIT-licensed and has no paid edition. You pay for the machine it runs
on. Plan about 2 vCPU and 4 GB of RAM per parallel job, and 100–200 GB of fast
disk for the caches ([Getting started](getting-started.md#requirements)).
