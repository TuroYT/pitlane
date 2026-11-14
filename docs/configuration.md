# Configuration

Most settings live in the web UI (**Settings**) and apply live, without a restart.

## UI settings

| Setting | Default | Notes |
| --- | --- | --- |
| Parallel jobs (slots) | half the CPU cores (max 8) | One warm Docker daemon per slot. Reducing it retires idle slots first. |
| Memory limit per job | 0 (unlimited) | Hard limit for the runner and its slot daemon. |
| CPU weight per job | 0 (Docker default) | `cpu-shares`. It only matters when the host is saturated; a lone job uses every core. |
| Cache sharing | per GitHub owner | `owner`: a slot that switches to another user/organization is wiped first. `shared`: every repository shares every cache. |
| Docker Hub mirror | on | Local pull-through cache (`registry:2`) used by every slot. |
| Build cache kept per slot | 20 GB | `docker builder prune --keep-storage` after each job. |
| Minimum free disk space | 20 GB | Checked after each job on the filesystem of `/var/lib/docker` (or the data directory). Below it, Pitlane prunes, until enough is free: the build cache of idle slots, then their images, then the largest per-owner cache volumes (`pitlane-cache-*`, `pitlane-toolcache-*`) no container uses. Busy slots are never touched; what was freed is logged. `0` disables it. |
| Runner image | `ghcr.io/turoyt/pitlane-runner:latest` | GitHub's official runner image plus build-essential, zip, jq… ([Dockerfile](../images/runner/Dockerfile)). Any image based on `ghcr.io/actions/actions-runner` works. |
| Docker-in-Docker image | `docker:28-dind` | |
| Allowed GitHub users | first admin | Comma-separated logins. |
| Job history | 30 days | Job records and runner logs. |

## Per repository / organization

| Setting | Default | Notes |
| --- | --- | --- |
| Label | `pitlane` | The value for `runs-on:`. |
| Warm runners | 1 for new repositories | Runners kept registered and idle, so jobs start in about a second. Each one holds a slot. Set `0` to free the slot when no job runs. Targets created before this default keep their value. |
| Max parallel | number of slots | Upper bound for this target. |
| Job limit (min) | 360 | A runner whose job runs longer is stopped (SIGTERM, so the job is reported as failed on GitHub at once, then SIGKILL after 30 s); its slot is cleaned and released as after any job. `0` = no limit. |
| Strict | on for public repositories (enforced, see below) | Fresh throwaway caches and a fully wiped slot daemon before and after every job. Slower (no warm cache), but nothing a job does (e.g. a pull request from a fork) can reach another job. |

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

## Automatic maintenance

- **Orphan runners.** A runner container that dies before deregistering (out of memory, host reboot) leaves an offline runner on GitHub. Pitlane records the ID of every runner it registers and, at start and every 10 minutes, removes those that GitHub reports offline and not busy, that have no Pitlane container and that were registered more than 15 minutes ago. Online, busy or unknown runners are never removed. It needs the REST runner list (`administration` permission of the GitHub App, or `repo` / `admin:org` for a token); runners registered by Pitlane versions before this feature are not tracked.
- **Lost jobs.** When a busy runner disappears before its job completes (killed by the job limit, out of memory, removed while Pitlane was stopped), GitHub keeps the job in progress and the runner offline and busy for several minutes, and still counts the job as assigned. Pitlane does not start a replacement runner for such a job: it is subtracted from the demand until GitHub reports it completed, it starts on another runner, the orphan cleanup sees its runner idle, or 30 minutes have passed.

## Health and metrics

- `GET /healthz` returns 200 as long as the process serves HTTP.
- `GET /readyz` returns 200 when Docker answers and at least one slot is up (ready, or busy/cleaning with a job), else 503 with the reason (`docker unreachable: …`, `no slot is ready`). No authentication.
- `GET /metrics` serves Prometheus text format:

| Metric | Type | Labels |
| --- | --- | --- |
| `pitlane_info` | gauge | `version` |
| `pitlane_docker_up` | gauge | |
| `pitlane_slots` | gauge | `status` (starting, ready, busy, cleaning, error, retiring) |
| `pitlane_runners` | gauge | `target`, `label`, `state` (busy, idle) |
| `pitlane_jobs_waiting` | gauge | `target`, `label`: jobs assigned by GitHub waiting for a free slot |
| `pitlane_jobs_completed` | gauge | `result`: finished jobs in the job history (shrinks when history is pruned) |
| `pitlane_job_duration_seconds` | summary | quantiles 0.5/0.9/0.99, `_sum`, `_count` of jobs finished in the last 24 h |
| `pitlane_job_wait_seconds` | summary | same, queue time of jobs started in the last 24 h |
| `pitlane_disk_free_bytes`, `pitlane_disk_total_bytes` | gauge | filesystem of `/var/lib/docker` (or the data directory) |

Access to `/metrics`: when a **metrics token** is set (Settings → Access), only `Authorization: Bearer <token>` is accepted. Without a token, signed-in users and clients connecting from the loopback address can read it. Requests carrying `X-Forwarded-For`, `X-Real-IP` or `Forwarded` never count as loopback, so a reverse proxy on the same host does not open it to everyone. With `--insecure-no-auth`, `/metrics` is open like the rest of the UI unless a token is set.

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
| `PITLANE_BASE_URL` | `--base-url` | request host | URL you open in the browser. |
| `PITLANE_LOG_LEVEL` | `--log-level` | `info` | |
| `PITLANE_GITHUB_TOKEN` | | | Use a personal access token instead of a GitHub App (advanced, see below). |
| `PITLANE_INSECURE_NO_AUTH` | `--insecure-no-auth` | `false` | Disables login. Pitlane then refuses to start unless `PITLANE_LISTEN` is a loopback address (`127.0.0.1`, `::1`, `localhost`). |
| `PITLANE_INSECURE_NO_AUTH_ANY_ADDRESS` | `--insecure-no-auth-any-address` | `false` | Allows `--insecure-no-auth` on any listen address. Only behind your own authentication (an authenticating reverse proxy that is the only thing able to reach Pitlane). |
| `DOCKER_HOST` | | local socket | Standard Docker client variables are honored. |

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
