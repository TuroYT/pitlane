# Security

## Threat model

Every job controls a **privileged Docker daemon** (its slot's Docker-in-Docker).
Privileged containers are not a security boundary: a job that wants to can
become root on the Pitlane host. From there it can read Pitlane's data
directory (the GitHub App private key and `master.key`), drive the host's
Docker daemon, tamper with other slots and their caches, and reach your LAN.

So the rule is simple: **only run code you trust on Pitlane, and treat the host
as dedicated to CI**, ideally a VM or an LXC container used only for Pitlane,
which you can rebuild at any time.

## Isolation model

What Pitlane does to keep honest jobs from stepping on each other:

- Jobs **never** get the host's Docker socket. Each slot runs its own Docker
  daemon, and a job only controls that daemon. The slot daemon's API listens on
  loopback only and is reachable only by the runner that shares the slot's
  network namespace. Slots never share a network with each other.
- The actions cache (`actions/cache` served by Pitlane) never crosses GitHub
  owners, whatever the cache scope: each runner gets a token, signed by
  Pitlane and valid until its job ends, that opens only its target owner's
  entries (one repository for a repository target). Within an owner, the
  repository and branch come from the job (`GITHUB_REPOSITORY` and the scopes
  of GitHub's job token, which Pitlane cannot verify), so a malicious job could
  read or poison another repository's or branch's entries of the same owner, as
  it already can with the owner's other caches. Strict jobs get no Pitlane
  cache (they use GitHub's). The token travels in the cache URL: it can show
  in a job's debug log, and is useless once the job has ended.
- With the default cache scope (`owner`), caches never cross GitHub owners: a slot
  that switches to another user or organization is wiped first (images, build
  cache, downloaded actions), and package/toolchain caches are separate volumes
  per owner. If a wipe fails, the slot is destroyed and rebuilt empty.

These measures stop accidental leaks between jobs. They do not contain a job
that attacks the host on purpose (see above).

## Strict mode

Strict mode (per repository or organization, on by default for public
repositories) gives each job throwaway caches and resets the slot's Docker
daemon before and after the job.

- **It guarantees** that a strict job starts without other jobs' caches, images
  or slot state, and leaves nothing behind for the next job, as long as no job
  breaks out of its slot.
- **It does not** protect the host: a strict job still controls a privileged
  Docker daemon and can take over the host, then every other job, cache and
  secret on it. **Strict mode is not a sandbox.**

## Public repositories and pull requests from forks

Anyone can open a pull request on a public repository. If its workflows run on
Pitlane, a stranger runs code with root-equivalent access to your host. The UI
flags public repositories. If you enable one anyway:

- In the repository (or organization) settings, under **Actions → General →
  Approval for running fork pull request workflows from contributors**, choose
  **Require approval for all external contributors**, and read a pull request's
  workflow changes before approving it.
- Never route untrusted pull requests to Pitlane unless the host is disposable
  (nothing else on it, no access to your LAN, rebuilt regularly). Keep
  `pull_request` jobs from forks on GitHub-hosted runners, for example:

  ```yaml
  runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || 'pitlane' }}
  ```

- Never use `pull_request_target` (or `workflow_run` on fork events) with a
  checkout of the pull request's code on Pitlane.
- Firewall the slots (below).

Stronger isolation (Sysbox, rootless Docker-in-Docker or microVMs per job)
would make untrusted jobs safer to run; Pitlane does not support it yet.

## Firewall the slots

A job can reach whatever the host can reach. Each slot uses its own Docker
bridge network `pitlane-slot-N` (label `dev.pitlane.managed=true`), whose subnet
Docker picks. These rules drop traffic from the slot subnets to the host
itself and to private addresses (LAN, link-local, cloud metadata), while
keeping the Docker Hub mirror (on the slot's own network) and the internet
reachable:

```sh
#!/bin/sh
# Run as root on the Pitlane host (or LXC container) once Pitlane has started,
# again after changing the number of slots, and at boot (e.g. a oneshot systemd
# unit with After=pitlane.service).
set -eu
DNS_SERVER=""   # set it if your DNS resolver is on the LAN, e.g. 192.168.1.1

iptables -N PITLANE-SLOTS 2>/dev/null || iptables -F PITLANE-SLOTS
iptables -N PITLANE-SLOTS-IN 2>/dev/null || iptables -F PITLANE-SLOTS-IN
for net in $(docker network ls -q --filter label=dev.pitlane.managed=true --filter name=pitlane-slot-); do
  for s in $(docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' "$net"); do
    case "$s" in *:*) continue ;; esac            # IPv4 here; use ip6tables for IPv6 subnets
    iptables -A PITLANE-SLOTS -s "$s" -d "$s" -j RETURN   # same slot network (mirror)
    if [ -n "$DNS_SERVER" ]; then
      iptables -A PITLANE-SLOTS -s "$s" -d "$DNS_SERVER" -p udp --dport 53 -j RETURN
      iptables -A PITLANE-SLOTS -s "$s" -d "$DNS_SERVER" -p tcp --dport 53 -j RETURN
    fi
    for d in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 100.64.0.0/10 169.254.0.0/16; do
      iptables -A PITLANE-SLOTS -s "$s" -d "$d" -j DROP
    done
    iptables -A PITLANE-SLOTS-IN -s "$s" -p tcp --dport 8091 -j RETURN  # Pitlane's actions cache (--cache-listen)
    iptables -A PITLANE-SLOTS-IN -s "$s" -j DROP     # the host's other ports (SSH, Pitlane UI…)
  done
done
iptables -C DOCKER-USER -j PITLANE-SLOTS 2>/dev/null || iptables -I DOCKER-USER -j PITLANE-SLOTS
iptables -C INPUT -j PITLANE-SLOTS-IN 2>/dev/null || iptables -I INPUT -j PITLANE-SLOTS-IN
```

The actions cache port is the one exception on the host: every request needs a
per-job token. When Pitlane runs in a container, runners reach the cache
through `pitlane-cache-forward`, a container on the slot networks that only
relays that port (covered by the first rule), and the exception is not needed.
Pitlane's own container never joins the slot networks.

`DOCKER-USER` only sees forwarded traffic (to the LAN and to other containers'
published ports); connections to the host's own addresses go through `INPUT`,
hence the second chain. Run a job afterwards to check that it can still pull
images and reach GitHub. Remember that these rules only bind honest jobs: a job
that has taken over the host can remove them.

## Secrets

- The GitHub App private key, client secret and webhook secret are encrypted with
  AES-256-GCM using `master.key` from the data directory.
- Just-in-time runner configs are passed to the runner container's environment and
  are single-use.
- UI sessions use random 256-bit tokens in `HttpOnly`, `SameSite=Lax` cookies
  (`Secure` when the base URL is HTTPS). State-changing API calls require a custom
  header, which blocks cross-site requests.

## Access

- The first setup is protected by a one-time token, available only on the host.
- Only GitHub logins listed in **Allowed GitHub users** can sign in.
- Put Pitlane behind HTTPS (a reverse proxy) if it is reachable outside your LAN.
- `--insecure-no-auth` (`PITLANE_INSECURE_NO_AUTH=true`) turns login off: anyone
  who reaches the UI controls Pitlane. Use it only with `--listen 127.0.0.1:8080`
  behind a reverse proxy on the same host that authenticates every request. The
  default `--listen :8080` would expose it to the whole network.

## Supply chain

- Third-party images Pitlane runs (`docker:dind`, `tonistiigi/binfmt`,
  `registry`, the official runner) and the Dockerfile base images are pinned by
  digest; Renovate proposes updates.
- The default runner image is the one released with your Pitlane version
  (`ghcr.io/turoyt/pitlane-runner:vX.Y.Z`).
- Releases are built on GitHub-hosted runners. `install.sh` verifies the binary
  against the release's `checksums.txt`, and binaries and images carry build
  provenance (`gh attestation verify`, see [Getting started](getting-started.md#updating)).
  The in-app self-update verifies that provenance itself (Sigstore bundle,
  release workflow identity, binary digest) and refuses to install without it
  (see [Updates](configuration.md#updates)).

## Reporting a vulnerability

Please open a private security advisory on GitHub
(**Security → Report a vulnerability**) rather than a public issue.
