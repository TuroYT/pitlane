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
| Strict | on for public repositories | Fresh throwaway caches and a fully wiped slot daemon before and after every job. Slower (no warm cache), but nothing a job does (e.g. a pull request from a fork) can reach another job. |

## Automatic maintenance

- **Orphan runners.** A runner container that dies before deregistering (out of memory, host reboot) leaves an offline runner on GitHub. Pitlane records the ID of every runner it registers and, at start and every 10 minutes, removes those that GitHub reports offline and not busy, that have no Pitlane container and that were registered more than 15 minutes ago. Online, busy or unknown runners are never removed. It needs the REST runner list (`administration` permission of the GitHub App, or `repo` / `admin:org` for a token); runners registered by Pitlane versions before this feature are not tracked.
- **Lost jobs.** When a busy runner disappears before its job completes (killed by the job limit, out of memory, removed while Pitlane was stopped), GitHub keeps the job in progress and the runner offline and busy for several minutes, and still counts the job as assigned. Pitlane does not start a replacement runner for such a job: it is subtracted from the demand until GitHub reports it completed, it starts on another runner, the orphan cleanup sees its runner idle, or 30 minutes have passed.

## Updates

**Settings → Updates** (signed-in users only; the actions are CSRF-protected like every other change). The same data is served by `GET /api/updates`; actions are `POST /api/updates/check`, `POST /api/updates/apply`, `POST /api/updates/pull-runner` and `PUT /api/updates/settings`.

**Pitlane.** Every 6 hours (and on **Check now**) Pitlane asks the GitHub API, without a token, for the releases of `TuroYT/pitlane`, and shows the latest stable release next to the running version, with a link to its release notes. If the repository is private or unreachable, the check fails with a message and nothing else changes.

**Update now** is offered when Pitlane runs as a plain binary (systemd, the Proxmox LXC script). It downloads `pitlane_linux_<arch>` and `checksums.txt` from the **same** release, over HTTPS from `github.com` and GitHub's release storage hosts only, with a 200 MB limit, and refuses to install when the checksum does not match or the file has no entry. It then runs `<new binary> version`, writes the binary next to the current one, fsyncs it, keeps the current one as `pitlane.prev` and renames the new one into place. Pitlane then shuts down like on `systemctl stop` (busy runners keep running and are adopted) and re-executes itself; if that fails it exits and systemd's `Restart=always` starts it again. Every step is logged (`journalctl -u pitlane`) and errors show in the UI; on any failure the current binary is left untouched.

Rollback: `sudo mv /usr/local/bin/pitlane.prev /usr/local/bin/pitlane && sudo systemctl restart pitlane`.

Self-update is not offered, and the exact command is shown instead, when Pitlane runs in a container (`docker compose pull && docker compose up -d`), when its binary's directory is not writable (re-run `install.sh` with `PITLANE_VERSION`), for a development build, or when `PITLANE_SELF_UPDATE=false` is set.

**Install updates automatically** (off by default) installs, inside a daily window (local time, default 03:00-05:00), the highest patch or minor release of the running major version, never a pre-release, and only while no job runs. A failed automatic attempt is retried the next day. New major versions are always manual.

The checksum only proves that the binary is the one listed in its release. A future step should also verify a signature or a GitHub build attestation of the release (see the release workflow): the `update.Verifier` hook in `internal/update` runs after the checksum check, before anything is installed.

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
| `PITLANE_INSECURE_NO_AUTH` | `--insecure-no-auth` | `false` | Disables login. Only behind your own authentication. |
| `PITLANE_SELF_UPDATE` | | | `false` disables self-update (for packaged installs). |
| `DOCKER_HOST` | | local socket | Standard Docker client variables are honored. |

### Personal access token mode

If you cannot create a GitHub App, set `PITLANE_GITHUB_TOKEN` to a token with the
`repo` scope (repositories) or `admin:org` scope (organizations). GitHub sign-in is
then unavailable, so put Pitlane behind your own authentication and start it with
`--insecure-no-auth`. Add targets manually in **Repositories**.

## Files

```
/var/lib/pitlane/
  pitlane.db      SQLite database (settings, targets, job history)
  master.key      encrypts the GitHub App private key and secrets: keep it with the db
  logs/           runner logs
  setup-token     only until the first admin signs in
```
