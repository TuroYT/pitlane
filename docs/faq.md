# FAQ

## Can I run a public repository on Pitlane?

You can, but read [Security](security.md) first. Every job controls a privileged
Docker daemon, so a job that wants to can become root on the Pitlane host, then
read the GitHub App key, other jobs' caches and whatever the host can reach on
your network.

Pitlane runs public repositories in **strict mode**, always: each job gets
throwaway caches and a wiped Docker daemon before and after it, and by default
no idle runner waits for a pull request. That keeps honest jobs apart. It does not stop
a malicious one, because strict mode is not a sandbox.

For a public repository:

- require approval for workflows from outside contributors (repository
  **Settings → Actions → General**);
- keep pull requests from forks on GitHub-hosted runners:

  ```yaml
  runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || 'pitlane' }}
  ```

- never check out a pull request's code in a `pull_request_target` job on
  Pitlane;
- run Pitlane on a host used only for CI, and
  [firewall the slots](security.md#firewall-the-slots) from your LAN.

Pitlane's own repository works this way: its CI runs on Pitlane, and pull
requests from forks run on `ubuntu-latest`.

## How much RAM and CPU does a job need?

Plan about **2 vCPU and 4 GB of RAM per parallel job**, plus 100–200 GB of disk
for the caches. A fast disk (SSD, NVMe) matters more than anything else, since
most of the time Pitlane saves is time not spent downloading and unpacking.

The default number of slots is half the CPU cores, at most 8. A lone job uses
every core of the host; CPU only gets shared when several jobs run at once.

Memory is usually what limits parallel jobs. After a few days of jobs, the
dashboard's **Memory per job** panel shows the p50, p90 and maximum peak memory
over the last 7 days, and suggests a slot count: usable memory divided by the
p90 peak. **Memory limit per job** in the settings caps a job that would take
the whole host. Details in [Resource usage per job](how-it-works.md#resource-usage-per-job).

## Does it work in a Proxmox LXC container?

Yes, it is the setup the benchmarks were measured on. The container can be
**unprivileged**, with `nesting=1,keyctl=1` so that Docker and the slots'
Docker-in-Docker daemons run inside it.

The one-liner creates such a container for you (8 cores, 16 GB of RAM and
150 GB of disk by default). It runs the same scripts as the
[community-scripts](https://community-scripts.org/) installers, so you get their
menu (default or advanced settings) and their overrides (`var_cpu`, `var_ram`,
`var_disk`, `var_hostname`, ...), and `update` inside the container updates
Pitlane:

```sh
bash -c "$(curl -fsSL https://github.com/TuroYT/pitlane/releases/latest/download/pitlane-lxc.sh)"
```

Things to know:

- multi-architecture builds (`docker/setup-qemu-action`) need `binfmt_misc`.
  Pitlane mounts it and registers the QEMU emulators at start when it runs with
  systemd in the container. The handlers are global to the Proxmox host kernel;
- per-job resource usage comes from cgroup v2, which Proxmox VE uses by default;
- a job can reach what the container can reach: put the container on a network
  that does not see your LAN, or apply the [slot firewall](security.md#firewall-the-slots).

## Does Pitlane need a public URL or an open port?

No. Pitlane opens a long-poll connection to GitHub through the runner scale set
API, and GitHub assigns jobs over it. It only needs outbound HTTPS to GitHub.
The URL you give Pitlane at setup is the one your browser uses, and it can be a
private LAN address.

## What happens when GitHub changes its API or the runner?

Pitlane uses GitHub's own [`actions/scaleset`](https://github.com/actions/scaleset)
Go library and the same scale set API as GitHub's Kubernetes controller, ARC. A
breaking change there would hit ARC users too.

The part that changes most often is the runner. GitHub refuses runners that are
too old, so each Pitlane release pins a recent runner image, and Renovate
proposes runner updates as GitHub publishes them. Keep Pitlane up to date
(**Settings → Updates**) and the runner follows.

If Pitlane breaks after a GitHub change before a fix is released, your workflows
only depend on the `runs-on:` label: set it back to `ubuntu-latest` and they run
on GitHub-hosted runners as before.

## Do I have to change my workflows?

Only `runs-on:`. `services:`, `container:` jobs, `docker build`,
`actions/cache` and the `setup-*` actions work as on GitHub-hosted runners. The
runner image is GitHub's official one with a few extra tools (build-essential,
zip, jq…), not the full `ubuntu-latest` image: a job that relies on a
preinstalled tool may need a setup step or a
[custom runner image](configuration.md#ui-settings).

## Are Windows, macOS or GitHub Enterprise Server supported?

No. Pitlane runs Linux runners (amd64 and arm64) for GitHub.com. Keep Windows
and macOS jobs on GitHub-hosted runners.

## What happens to running jobs when Pitlane restarts or updates?

They keep running. Stopping Pitlane stops the listeners but leaves busy runners
alone; at start, Pitlane adopts them, and slots keep their caches across
restarts.
