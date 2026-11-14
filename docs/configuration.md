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
| Runner image | `ghcr.io/turoyt/pitlane-runner:latest` | GitHub's official runner image plus build-essential, zip, jq… ([Dockerfile](../images/runner/Dockerfile)). Any image based on `ghcr.io/actions/actions-runner` works. |
| Docker-in-Docker image | `docker:28-dind` | |
| Allowed GitHub users | first admin | Comma-separated logins. |
| Job history | 30 days | Job records and runner logs. |

## Per repository / organization

| Setting | Default | Notes |
| --- | --- | --- |
| Label | `pitlane` | The value for `runs-on:`. |
| Warm runners | 0 | Runners kept registered and idle, so jobs start instantly. Each one holds a slot. |
| Max parallel | number of slots | Upper bound for this target. |
| Strict | on for public repositories | Fresh throwaway caches and a fully wiped slot daemon before and after every job. Slower (no warm cache), but nothing a job does (e.g. a pull request from a fork) can reach another job. |

## Environment variables / flags

| Variable | Flag | Default | |
| --- | --- | --- | --- |
| `PITLANE_LISTEN` | `--listen` | `:8080` | HTTP address. |
| `PITLANE_DATA` | `--data` | `/var/lib/pitlane` | Database, master key, logs. **Back it up.** |
| `PITLANE_BASE_URL` | `--base-url` | request host | URL you open in the browser. |
| `PITLANE_LOG_LEVEL` | `--log-level` | `info` | |
| `PITLANE_GITHUB_TOKEN` | | | Use a personal access token instead of a GitHub App (advanced, see below). |
| `PITLANE_INSECURE_NO_AUTH` | `--insecure-no-auth` | `false` | Disables login. Only behind your own authentication. |
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
