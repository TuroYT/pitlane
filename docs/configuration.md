# Configuration

Most settings live in the web UI (**Settings**, admins only) and apply live, without a restart.
Who can sign in is set in **Users** (see [Users and roles](#users-and-roles)).

## UI settings

| Setting | Default | Notes |
| --- | --- | --- |
| Parallel jobs (slots) | half the CPU cores (max 8) | One warm Docker daemon per slot. Reducing it retires idle slots first. |
| Memory limit per job | 0 (unlimited) | Hard limit for the runner and its slot daemon. |
| Start a job only when its memory fits | on | Memory admission: a job waits while the predicted memory of the running jobs plus its own exceeds the host's memory minus the reserve; a job bigger than that runs alone. See [Scheduling](how-it-works.md#scheduling). |
| Memory reserve (MB) | 2048 | Kept for the system, Docker, Pitlane and the page cache; at most half the host's memory. |
| Reserved slots for strict jobs | automatic: 1 while a strict target is enabled, else 0 | Slots only strict jobs use (0 to slots − 1). Strict jobs overflow to empty slots, then to a warm slot whose owner keeps another one: they never wipe an owner's last warm cache, and queue instead. 0 restores the previous behaviour. See [Slots reserved for strict jobs](how-it-works.md#slots-reserved-for-strict-jobs). |
| CPU weight per job | 0 (Docker default) | `cpu-shares`. It only matters when the host is saturated; a lone job uses every core. |
| Cache sharing | per GitHub owner | `owner`: a slot that switches to another user/organization is wiped first. `shared`: every repository shares every cache. |
| Docker Hub mirror | on | Local pull-through cache (`registry:2`, pinned by digest) used by every slot. |
| Actions cache | on | Serves `actions/cache` (and `setup-*` actions with `cache:`) from the Pitlane host, for non-strict jobs; strict jobs keep GitHub's cache. Entries are kept per repository and branch, never across owners. Turning it off applies to the next jobs and keeps the stored entries. See [How it works](how-it-works.md#actions-cache). |
| Actions cache size | 20 GB | Total size of the stored entries; beyond it the least recently restored go first. Settings shows the current size and the hit/miss counters since start. |
| Actions cache share per owner | 50 % | The most one user or organization may store, as a share of the cache size (`actions_cache_owner_pct`, 1 to 100). Uploads in progress count against it, and a larger upload is refused. When an owner goes over it, its own least recently restored entries go first (its newest entry always stays); the size limit then applies to the whole cache. `100` turns it off. |
| Build cache kept per slot | 20 GB | `docker builder prune --keep-storage` after each job. |
| Minimum free disk space | 20 GB | Checked after each job on the filesystem of `/var/lib/docker` (or the data directory). Below it, Pitlane prunes, until enough is free: the build cache of idle slots, then their images, then the least recently used actions cache entries, then the largest per-owner cache volumes (`pitlane-cache-*`, `pitlane-toolcache-*`) no container uses. Busy slots are never touched; what was freed is logged. `0` disables it. |
| Runner image | `ghcr.io/turoyt/pitlane-runner:v<version>` | The runner image released with this Pitlane version: GitHub's official runner image plus build-essential, zip, jq… ([Dockerfile](../images/runner/Dockerfile)). It follows Pitlane upgrades, including when the settings page saved it; to stay on one image, give a digest (`name:tag@sha256:…`). Any image based on `ghcr.io/actions/actions-runner` works. |
| Docker-in-Docker image | `docker:28-dind@sha256:…` | Pinned by digest ([images.go](../internal/pool/images.go)). |
| Job history | 30 days | Job records and runner logs. |

## Per repository / organization

| Setting | Default | Notes |
| --- | --- | --- |
| Label | `pitlane` | The value for `runs-on:`. A repository and its organization cannot both be enabled with the same label (GitHub would send each job to only one of their scale sets, the other holding slots for nothing): give one of them another label. |
| Warm runners | 1 for new targets, 0 for new strict targets | Runners kept registered and idle, so jobs start in about a second. Each one holds a slot. Set `0` to free the slot when no job runs. A strict (public) target gets none, so no idle runner sits registered waiting for a pull request from a fork; you can still set one. Targets created before this default keep their value. |
| Max parallel | number of slots | Upper bound for this target. |
| Job limit (min) | 360 | A runner whose job runs longer is stopped (SIGTERM, so the job is reported as failed on GitHub at once, then SIGKILL after 30 s); its slot is cleaned and released as after any job. `0` = no limit. |
| Strict | on for public repositories (enforced, see below) | Fresh throwaway caches and a fully wiped slot daemon before and after every job. Slower (no warm cache). It keeps caches and slot state from leaking between jobs, but it is **not a sandbox**: a malicious job (e.g. a pull request from a fork) can still take over the host. See [Security](security.md#strict-mode). |

### Strict mode enforcement

Pitlane asks GitHub for a repository's visibility (with the App installation token,
or the personal access token) when the target is created, every hour, at start and
whenever the repository list is shown:

- A **public repository** always runs strict. Creating it with `strict: false`, or
  turning strict off, is refused (HTTP 400). A repository that becomes public is
  switched to strict on its own: Pitlane logs a warning, the UI shows why, and its
  idle warm runners (started without strict isolation) are replaced.
- When GitHub cannot be asked, a **new** target is created strict (fail closed) and an
  **existing** target keeps its mode (the error is logged); turning strict off is
  refused until GitHub answers.
- A repository that becomes private stays strict until you turn strict off.
- An **organization** target serves every repository its runner group lets in.
  Pitlane registers its scale sets in the organization's **Default** runner group
  and reads that group's *Allow public repositories* setting (Organization settings →
  Actions → Runner groups → Default). While it is on, the organization target is
  enforced strict exactly like a public repository. **Keep it off** (GitHub's
  default): public repositories of the organization then cannot use the runners at
  all, and fork pull requests never reach them. To run a public repository on
  Pitlane, enable that repository as its own target (always strict) rather than
  opening the runner group.

## Users and roles

Sign-in is GitHub OAuth through Pitlane's GitHub App. Who may sign in, and what
they may do, is set by admins in **Users**:

| Role | Can |
| --- | --- |
| `viewer` | Read the dashboard, jobs, runner and job logs, job links, the comparison with GitHub-hosted runners and `/metrics`. Changes nothing, sees no settings, users, audit log or secret. |
| `operator` | Everything a viewer can, plus enable or disable a repository or organization, see the update status, check for updates and pull the runner image. No other target setting, no settings, users, update install or update preferences. |
| `admin` | Everything: targets, settings, users and rules, updates, the audit log, recreating the GitHub App. |

The server checks the role on every API call (a viewer gets HTTP 403, not just a
hidden button), reading it again each time: demoting someone takes effect on
their next request, and removing a user ends their sessions.

- **Users**: a GitHub login and a role. A user listed here gets that role,
  whatever the rules below say. There is always at least one admin: removing or
  demoting the last one is refused (HTTP 409).
- **Organization and team rules**: "members of organization `acme` are viewers",
  "members of team `acme/platform` are admins" (the team's slug). Someone who
  is not a listed user signs in with the highest role of the rules they match.
  Pitlane checks the memberships at sign-in only, with the user's own token
  (GitHub App user access tokens have no OAuth scopes: they get the App's
  permissions, limited to what the user can see):
  - organization membership: `GET /user/memberships/orgs/{org}`, which needs the
    App's **Members** organization permission (read-only) and the App installed
    in that organization; otherwise Pitlane falls back to the **public**
    membership (`GET /orgs/{org}/public_members/{login}`, no permission needed).
    A member who keeps their membership private is then not recognized;
  - team membership: `GET /orgs/{org}/teams/{team}/memberships/{login}`, which
    always needs the **Members** permission and the App installed there.

  The App Pitlane creates does not ask for **Members**. To use private
  memberships or team rules, add it in the App's settings on GitHub
  (**Permissions & events → Organization permissions → Members: Read-only**),
  then accept the new permission on the organization's installation. A check
  GitHub refuses or cannot answer grants nothing (Pitlane logs a warning), so
  sign-in falls back to the other rules.

  A session granted by a rule lasts **12 hours** (a listed user's lasts 30
  days): someone who leaves the organization or team keeps access at most that
  long, then signs in again and is checked again. Removing or demoting a rule
  applies to its sessions at once.

**Upgrading** from a version with the *Allowed GitHub users* setting: every
login of that list becomes an admin, so nobody loses access. Pitlane keeps
writing the admins' logins to that setting, so going back to an older version
lets the admins in, and nobody else.

**Audit log** (admins): who changed what and when, latest first: targets
(created, changed fields with old and new values, removed), settings (changed
values; the metrics token only as "changed" or "removed"), users and rules,
update checks, installs, pulls and preferences, the GitHub App creation. Kept as
long as the job history (**Job history** setting). Admin actions are also
written to Pitlane's log.

## Automatic maintenance

- **Orphan runners.** A runner container that dies before deregistering (out of memory, host reboot) leaves an offline runner on GitHub. Pitlane records the ID of every runner it registers and, at start and every 10 minutes, removes those that GitHub reports offline and not busy, that have no Pitlane container and that were registered more than 15 minutes ago. Online, busy or unknown runners are never removed. It needs the REST runner list (`administration` permission of the GitHub App, or `repo` / `admin:org` for a token); runners registered by Pitlane versions before this feature are not tracked.
- **Lost jobs.** When a busy runner disappears before its job completes (killed by the job limit, out of memory, removed while Pitlane was stopped), GitHub keeps the job in progress and the runner offline and busy for several minutes, and still counts the job as assigned. Pitlane does not start a replacement runner for such a job: it is subtracted from the demand until GitHub reports it completed, it starts on another runner, the orphan cleanup sees its runner idle, or 30 minutes have passed.

## Updates

**Settings → Updates** (admins; operators see the status and can check and pull the runner image; the actions are CSRF-protected like every other change). The same data is served by `GET /api/updates`; actions are `POST /api/updates/check`, `POST /api/updates/apply`, `POST /api/updates/pull-runner` and `PUT /api/updates/settings`.

**Pitlane.** Every 6 hours (and on **Check now**) Pitlane asks the GitHub API, without a token, for the releases of `TuroYT/pitlane`, and shows the latest stable release next to the running version, with a link to its release notes. If the repository is private or unreachable, the check fails with a message and nothing else changes.

**Update now** is offered when Pitlane runs as a plain binary (systemd, the Proxmox LXC script). It downloads `pitlane_linux_<arch>` and `checksums.txt` from the **same** release, over HTTPS from `github.com` and GitHub's release storage hosts only, with a 200 MB limit, and refuses to install when the checksum does not match or the file has no entry, or when its build attestation does not verify (see below). It then runs `<new binary> version`, writes the binary next to the current one, fsyncs it, keeps the current one as `pitlane.prev` and renames the new one into place. Pitlane then shuts down like on `systemctl stop` (busy runners keep running and are adopted) and re-executes itself; if that fails it exits and systemd's `Restart=always` starts it again. Every step is logged (`journalctl -u pitlane`) and errors show in the UI; on any failure the current binary is left untouched.

Rollback: `sudo mv /usr/local/bin/pitlane.prev /usr/local/bin/pitlane && sudo systemctl restart pitlane`.

Self-update is not offered, and the exact command is shown instead, when Pitlane runs in a container (`docker compose pull && docker compose up -d`), when its binary's directory is not writable (re-run `install.sh` with `PITLANE_VERSION`), for a development build, or when `PITLANE_SELF_UPDATE=false` is set.

**Install updates automatically** (off by default) installs, inside a daily window (local time, default 03:00-05:00), the highest patch or minor release of the running major version (while Pitlane is in 0.x, where a minor release may break things, only patch releases of the running minor version), never a pre-release, and only while no job runs. A failed automatic attempt is retried the next day. New major versions (and, in 0.x, new minor versions) are always installed manually, with **Update now**.

**Build attestation.** The checksum only proves that the binary is the one listed in its release. After it, and before anything is written in place, Pitlane checks the binary's GitHub build provenance attestation (published by `actions/attest-build-provenance` in the release workflow):

- it asks `GET https://api.github.com/repos/TuroYT/pitlane/attestations/sha256:<digest of the downloaded binary>` (anonymous);
- it verifies the Sigstore bundle with [sigstore-go](https://github.com/sigstore/sigstore-go): signature, certificate chain to the Sigstore public-good Fulcio roots, signed certificate timestamp, and inclusion in the Rekor transparency log. The trusted root comes from Sigstore's TUF repository (`https://tuf-repo-cdn.sigstore.dev`, verified against the TUF root embedded in Pitlane, cached in `<data>/sigstore-tuf`);
- the certificate must be issued by GitHub Actions (`https://token.actions.githubusercontent.com`) to the workflow `TuroYT/pitlane/.github/workflows/release.yml` at a `refs/tags/v*` ref, for the repository `https://github.com/TuroYT/pitlane`, on a GitHub-hosted runner, and for the tag of the release being installed;
- the statement must be a SLSA provenance whose subject `pitlane_linux_<arch>` has the downloaded binary's SHA-256.

Fail closed: when GitHub has no attestation for the digest, when the API, the TUF repository or any check fails, nothing is installed and the error shows in **Settings → Updates** (and in the journal). An automatic update that fails this way is retried the next day. Manual installs with `install.sh` are not affected.

The expected repository and workflow are fixed at build time and cannot be changed from the UI or the environment. A fork that publishes its own attested releases builds with `-ldflags "-X github.com/TuroYT/pitlane/internal/update.AttestationRepo=<owner>/<repo> -X github.com/TuroYT/pitlane/internal/update.AttestationWorkflow=.github/workflows/release.yml"`. A fork without attestations can set `PITLANE_UPDATE_SKIP_ATTESTATION=true`: **unsafe**, updates then rely on the checksum only (a compromised release would be installed); Pitlane logs a warning at start and at every update, and the Updates panel says so.

**Runner image.** Pitlane reads the GitHub runner version inside the configured runner image (from `/home/runner/bin/Runner.Listener.deps.json` of a created, never started, container) and compares it with the latest `actions/runner` release. GitHub stops accepting a runner that does not update itself about 30 days after a newer version is released: the UI warns a week before that date and after it. **Pull latest** pulls the configured tag again; with **Pull the runner image automatically** (on by default) this happens every day. Runners already started (idle warm runners included) keep their image; runners started after the pull use the new one. The pull runs in the background, so no job waits for it. The image digest in use and the time of the last pull are shown.

## Health and metrics

- `GET /healthz` returns 200 as long as the process serves HTTP.
- `GET /readyz` returns 200 when Docker answers and at least one slot is up (ready, or busy/cleaning with a job), else 503 with the reason (`docker unreachable: …`, `no slot is ready`). No authentication.
- `GET /metrics` serves Prometheus text format:

| Metric | Type | Labels |
| --- | --- | --- |
| `pitlane_info` | gauge | `version` |
| `pitlane_docker_up` | gauge | |
| `pitlane_slots` | gauge | `status` (starting, ready, busy, cleaning, error, retiring) |
| `pitlane_slot_wipes_total` | counter | `reason` (`owner_change`, `strict`, `unknown_owner`): slots rebuilt empty before a job since start |
| `pitlane_runners` | gauge | `target`, `label`, `state` (busy, idle) |
| `pitlane_jobs_waiting` | gauge | `target`, `label`, `reason` (`slot`, `memory`, `strict_slot`): jobs assigned by GitHub that wait, by why (a target waits for one reason at a time, the others are 0) |
| `pitlane_memory_budget_bytes` | gauge | memory jobs may use: host memory minus the reserve (0 when unknown) |
| `pitlane_memory_predicted_bytes` | gauge | memory reserved for the running jobs by prediction |
| `pitlane_memory_committed_bytes` | gauge | what the admission counts: per running job, the larger of its prediction and its current use |
| `pitlane_memory_admission_enabled` | gauge | 1 when memory admission is on |
| `pitlane_jobs_completed` | gauge | `result`: finished jobs in the job history (shrinks when history is pruned) |
| `pitlane_job_duration_seconds` | summary | quantiles 0.5/0.9/0.99, `_sum`, `_count` of jobs finished in the last 24 h |
| `pitlane_job_wait_seconds` | summary | same, queue time of jobs started in the last 24 h |
| `pitlane_job_memory_peak_bytes` | summary | quantiles 0.5/0.9/0.99, `_sum`, `_count` of the peak memory (runner + slot) of the measured jobs finished in the last 7 days (see [resource usage per job](how-it-works.md#resource-usage-per-job)) |
| `pitlane_disk_free_bytes`, `pitlane_disk_total_bytes` | gauge | filesystem of `/var/lib/docker` (or the data directory) |
| `pitlane_actions_cache_bytes`, `pitlane_actions_cache_entries` | gauge | what the actions cache stores |
| `pitlane_actions_cache_requests_total` | counter | `result` (hit, miss, save), since start |
| `pitlane_actions_cache_evictions_total` | counter | entries evicted (size limit, low disk) since start |

Access to `/metrics`: when a **metrics token** is set (Settings → Access), only `Authorization: Bearer <token>` is accepted. Without a token, signed-in users (any role) and clients connecting from the loopback address can read it. Requests carrying `X-Forwarded-For`, `X-Real-IP` or `Forwarded` never count as loopback, so a reverse proxy on the same host does not open it to everyone. With `--insecure-no-auth`, `/metrics` is open like the rest of the UI unless a token is set.

```yaml
scrape_configs:
  - job_name: pitlane
    authorization: { credentials: <metrics token> }
    static_configs: [{ targets: ["pitlane.example:8080"] }]
```

## Environment variables / flags

| Variable | Flag | Default | |
| --- | --- | --- | --- |
| `PITLANE_LISTEN` | `--listen` | `:8080` | HTTP address. |
| `PITLANE_DATA` | `--data` | `/var/lib/pitlane` | Database, master key, logs. **Back it up.** |
| `PITLANE_BASE_URL` | `--base-url` | saved by the setup | URL you open in the browser (see [Base URL](#base-url)). |
| `PITLANE_TRUSTED_PROXIES` | `--trusted-proxies` | | Comma-separated IPs or CIDRs of reverse proxies allowed to set the `Host` and `X-Forwarded-Proto` headers the base URL is derived from (loopback always is). |
| `PITLANE_CACHE_LISTEN` | `--cache-listen` | `:8091` | Address of the actions cache. Runners reach it on the slot network's gateway (the host). When Pitlane runs in a container, it starts `pitlane-cache-forward` (its own image, `nobody`, no capabilities, read-only) on the slot networks, which relays only this port to Pitlane. `off` disables it; if it cannot listen, Pitlane logs an error and jobs use GitHub's cache. Each request needs a per-job token. |
| `PITLANE_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn` or `error`; anything else stops Pitlane at start. |
| `PITLANE_GITHUB_TOKEN` | | | Use a personal access token instead of a GitHub App (advanced, see below). |
| `PITLANE_INSECURE_NO_AUTH` | `--insecure-no-auth` | `false` | Disables login. Pitlane then refuses to start unless `PITLANE_LISTEN` is a loopback address (`127.0.0.1`, `::1`, `localhost`). |
| `PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS` | `--insecure-no-auth-any-address` | `false` | Allows `--insecure-no-auth` on any listen address. Only behind your own authentication (an authenticating reverse proxy that is the only thing able to reach Pitlane). |
| `PITLANE_SELF_UPDATE` | | | `false` disables self-update (for packaged installs). |
| `PITLANE_UPDATE_SKIP_ATTESTATION` | | | `true` skips the [build attestation](#updates) check of self-updates. **Unsafe**, for forks that publish no attestation. |
| `DOCKER_HOST` | | local socket | Standard Docker client variables are honored. |

### Base URL

The base URL is the GitHub App's callback address and decides whether cookies are
`Secure`. Pitlane uses, in order: `PITLANE_BASE_URL`; the URL saved by the first-run
setup (the setup page proposes the address your browser used, and you can edit it);
for a request coming from loopback or from a `PITLANE_TRUSTED_PROXIES` address, the
request's `Host` and `X-Forwarded-Proto` headers; otherwise a URL built from
`PITLANE_LISTEN` (the machine's host name when it listens on every interface).
Other clients cannot choose it through request headers. First-run setup keeps
working without any of these set: open the UI at the address you want, check the
proposed URL and create the App. Behind a reverse proxy on another machine, set
`PITLANE_BASE_URL` or list the proxy in `PITLANE_TRUSTED_PROXIES`.

### Personal access token mode

If you cannot create a GitHub App, set `PITLANE_GITHUB_TOKEN` to a token with the
`repo` scope (repositories) or `admin:org` scope (organizations). GitHub sign-in is
then unavailable, so put Pitlane behind your own authentication and start it with
`--insecure-no-auth`. Keep it on a loopback address (`PITLANE_LISTEN=127.0.0.1:8080`)
with the proxy on the same host. When the proxy reaches Pitlane over a network
(another container, another machine), also set `PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS=true`
and make sure nothing else can reach that port. Add targets manually in **Repositories**.

## Files

```
/var/lib/pitlane/
  pitlane.db      SQLite database (settings, targets, job history)
  master.key      encrypts the GitHub App private key and secrets: keep it with the db
  logs/           runner logs
  actions-cache/  actions/cache entries (entries/<id>.bin + .json) and the key signing per-job tokens
  setup-token     only until the first admin signs in
```

Pitlane restricts the data directory to its owner (`0700`) and creates every file
in it `0600`. Both the Docker image and the systemd unit run Pitlane as root, which
owns the directory (a new Compose volume is created by Docker for root). If you
point `PITLANE_DATA` at a directory owned by another user (a bind mount prepared
by someone else, Pitlane running as a non-root user with group access), Pitlane
cannot change its mode: it logs a warning and starts anyway, its files staying
`0600`. Restrict that directory yourself. A directory Pitlane owns but cannot
`chmod` is an error.
