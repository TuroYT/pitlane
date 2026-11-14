# Design: serving `actions/cache` from the Pitlane host

Status: implemented in v0.3 (`internal/cache`, `internal/pool/actionscache.go`). Sources checked on 2026-11-15
against `actions/runner` v2.338.2 (the version pinned in
`images/runner/Dockerfile`) and `@actions/cache` 6.3.0 (`actions/toolkit` main).

## Goal

`actions/cache`, and every action built on `@actions/cache` (`setup-node`,
`setup-python`, `setup-go`, `setup-java` with `cache:`, `pnpm/action-setup`,
`ruby/setup-ruby`, …), should restore and save at LAN/disk speed from the
Pitlane host instead of GitHub's Azure-backed cache, **without any workflow
change**, and **without ever breaking a job**: when anything is off, jobs use
GitHub's cache as before.

## How the cache protocol works today

### Who tells the action where the cache is

GitHub sends the runner a job message whose `SystemVssConnection` endpoint
carries the service URLs (`CacheServerUrl`, `ResultsServiceUrl`, …) and a job
token. For every JavaScript action step, `Runner.Worker`
(`src/Runner.Worker/Handlers/NodeScriptActionHandler.cs`) builds the step
environment and, **after** copying the job's `env:` and the runner's own
environment, sets:

| Variable | Value |
| --- | --- |
| `ACTIONS_RUNTIME_TOKEN` | job-scoped JWT, sent as `Authorization: Bearer` |
| `ACTIONS_CACHE_URL` | legacy cache service (v1), only when `CacheServerUrl` is in the message |
| `ACTIONS_RESULTS_URL` | results service (`results-receiver.actions.githubusercontent.com`): cache v2 **and artifacts v4** |
| `ACTIONS_CACHE_SERVICE_V2` | `True` when the job has the `actions_uses_cache_service_v2` feature flag (all of github.com since 2025) |
| `ACTIONS_CACHE_MODE` | new: `read`/`write`/`write-only`/`none` policy, enforced by the client |

It then runs `externals/<node20|node24>/bin/node <action script>`. Because the
runner assigns these variables last, setting `ACTIONS_CACHE_URL` or
`ACTIONS_RESULTS_URL` on the runner container (or in a workflow's `env:`) has
no effect: the handler overwrites them.

### What the client does (`@actions/cache`)

`internal/config.ts`:

```ts
getCacheServiceVersion(): isGhes() ? 'v1' : (ACTIONS_CACHE_SERVICE_V2 ? 'v2' : 'v1')
getCacheServiceURL():     v1 → ACTIONS_CACHE_URL || ACTIONS_RESULTS_URL ; v2 → ACTIONS_RESULTS_URL
```

- **v2** (github.com default): Twirp/JSON on
  `ACTIONS_RESULTS_URL` + `/twirp/github.actions.results.api.v1.CacheService/`
  (`GetCacheEntryDownloadURL`, `CreateCacheEntry`, `FinalizeCacheEntryUpload`).
  The service returns signed Azure Blob URLs; the client uploads/downloads with
  the Azure SDK (block blobs) directly. The Twirp URL is built with an absolute
  path (`new URL('/twirp/…', base)`), so a path prefix in the base URL is lost.
- **v1** (legacy, still used by GHES and still shipped in the client):
  REST on `ACTIONS_CACHE_URL` + `_apis/artifactcache/`:
  - `GET cache?keys=k1,k2&version=V` → `200 {cacheKey, scope, creationTime, archiveLocation}` or `204`
  - `POST caches {key, version, cacheSize}` → `{cacheId}` (`409` when it exists or is being written)
  - `PATCH caches/{id}` with `Content-Range: bytes a-b/*`, parallel chunks
  - `POST caches/{id} {size}` commits
  - `archiveLocation` is fetched with a plain `GET` (no auth header): it must be a self-authenticating URL.
  The base URL is concatenated as a string, so **a path prefix survives**.

`version` is a hash of the paths, compression method and OS flag, so it
already separates incompatible archives. GitHub resolves a lookup like this
(documented in "Caching dependencies", "Matching a cache key"): first in the
current branch scope, exact `key` match, then prefix matches of `key` and of
each `restore-keys` entry in order (newest first); then the same in the base
branch (pull requests) and the default branch. Entries are immutable; a save
of an existing key fails with "another job may be creating this cache" (a
warning). Cache errors in `restoreCache`/`saveCache` are logged as warnings
and never fail the step (only input validation errors do).

The job token carries the branch scopes in its `ac` claim, a JSON string like
`[{"Scope":"refs/heads/feature","Permission":3},{"Scope":"refs/heads/main","Permission":1}]`
(1 = read, 2 = write). BuildKit's `type=gha` exporter and Forgejo's server
rely on the same claim.

## What others do

| Project (license) | How jobs reach it | Notes |
| --- | --- | --- |
| [falcondev-oss/github-actions-cache-server](https://github.com/falcondev-oss/github-actions-cache-server) (MIT) | forked runner image reading `CUSTOM_ACTIONS_RESULTS_URL`, or `sed` on `Runner.Worker.dll` renaming the UTF-16 literal `ACTIONS_RESULTS_URL` to a dummy name, then `ACTIONS_RESULTS_URL` set on the runner | v2 API. The patch also redirects artifacts v4 (same variable). Runner self-update silently restores the stock binary; needs `--disableupdate`. |
| RunsOn "Magic Cache" (commercial) | a `runs-on/action` step writes `ACTIONS_CACHE_URL`/`ACTIONS_RESULTS_URL` to `$GITHUB_ENV`, pointing at a sidecar on the VM | needs a workflow change; the sidecar forwards artifact calls to GitHub. |
| nektos/act, Gitea/Forgejo act_runner (MIT) | they *are* the runner, so they set `ACTIONS_CACHE_URL` themselves | v1 API server embedded in the runner (`pkg/artifactcache`). Not applicable to the official runner. |
| Blacksmith, WarpBuild, Ubicloud, Namespace (commercial) | own VM images and network; some intercept the results/cache hosts transparently | not reusable. |

## Options for Pitlane

1. **Binary patch of `Runner.Worker.dll`** (falcondev). Works, but it is tied
   to the runner build (string literal layout), must be redone for every
   runner image (users can pick their own image), and renaming
   `ACTIONS_RESULTS_URL` also captures artifacts v4, so Pitlane would have to
   proxy the artifact Twirp calls to GitHub, where a proxy bug breaks
   `upload-artifact` (a hard failure, not a warning). Inside job containers
   (`container:`), only the handler's variables are passed, so the dummy name
   leaks in and the cache silently breaks there.
2. **Environment overrides only.** Impossible: the handler overwrites the
   variables (above).
3. **Transparent proxy / MITM** of `results-receiver.actions.githubusercontent.com`.
   Needs DNS or iptables tricks in every slot plus a CA trusted by Node
   (`NODE_EXTRA_CA_CERTS`) and by BuildKit; a job controls its slot's Docker
   daemon and network; artifacts and logs go through the same host. Far too
   invasive and fragile.
4. **Node shim in the externals + v1 protocol (chosen).** Pitlane already
   copies the runner image's `externals/` into a per-slot volume (job
   containers need it at `/__e`). While copying, Pitlane replaces every
   `externals/node*/bin/node` with a 6-line `sh` script that, *only when*
   Pitlane injected `PITLANE_CACHE_URL` into the runner container,
   sets `ACTIONS_CACHE_URL` to Pitlane's server and unsets
   `ACTIONS_CACHE_SERVICE_V2`, then `exec`s the real Node binary. The client
   then speaks v1 to Pitlane. `ACTIONS_RESULTS_URL` is untouched: artifacts,
   OIDC, logs and job summaries keep going to GitHub.

### Why option 4

- **Fails safe at every layer.** No `PITLANE_CACHE_URL` (feature off, strict
  job, cache listener down, job container) → the shim is a plain `exec`, jobs
  use GitHub's cache exactly as today. Pitlane's server down or failing → the
  client logs a warning and the job goes on (cache miss / save skipped). A
  future `@actions/cache` without v1 would ignore `ACTIONS_CACHE_URL` and
  fall back to v2, i.e. GitHub's cache.
- **No binary patch, any runner image.** Nothing in `Runner.Worker` changes;
  it works with the official image or a custom one, and survives runner
  upgrades (the externals copy is redone for each image, the shim with it).
- **Artifacts never go through Pitlane.**
- **Job containers keep working.** The runner already runs `sh -c` inside job
  containers before starting JavaScript actions (OS detection in
  `StepHost.cs`), so the shim adds no requirement. The runner does not pass
  `PITLANE_CACHE_URL` into job containers, so their actions use GitHub's
  cache (Pitlane's address is not reachable from the slot daemon's networks
  anyway).
- **v1 is simple** (4 endpoints, ranges written straight to a file) and a path
  prefix survives in the base URL, which carries Pitlane's per-job credential
  and the repository.

Risks: GitHub removing v1 from the client (fallback to GitHub, see above);
`ACTIONS_CACHE_MODE` keeps being enforced by the client, which reads it from
the environment. BuildKit's `type=gha` cache (docker/build-push-action)
receives the same variables and speaks v1 too; it is served by Pitlane only
when BuildKit can reach the host (usually not: it runs in the slot daemon's
own network); otherwise it fails like any unreachable gha cache. Pitlane
already recommends the slot's local build cache instead.

## Pitlane design

### Server (`internal/cache`)

- Built into the Pitlane process, on its own listener (`--cache-listen`,
  default `:8091`), separate from the UI (no session auth, no reverse proxy).
- The runner reaches it at the slot network's gateway (the host) on that port:
  `PITLANE_CACHE_URL=http://<gateway>:8091/<token>/`. When Pitlane itself runs
  in a container (Compose), a published port is not reachable from another
  bridge network (Docker's inter-network isolation), so Pitlane joins each
  slot network, like the mirror, and advertises its address there. The shim
  appends `$GITHUB_REPOSITORY/`.
- Storage: `<data dir>/actions-cache/`, one `<id>.json` (metadata) and one
  `<id>.tgz` (archive) per entry, uploads in `tmp/`. The index is rebuilt by
  scanning at start; no database table, no migration.
- Reservations must declare their size; unfinished uploads together stay
  within the size limit (32 per runner at most) and are dropped when their
  runner is revoked or after 24 hours.
- Lookup semantics as GitHub (scopes from the `ac` claim, then exact key,
  key prefix, each restore key prefix, newest first). Entries are immutable,
  a reserve on an existing or pending key returns 409.
- Size limit (setting, default 20 GB) with LRU eviction (last restore time),
  plus a hook in the low-disk pruning (`internal/pool/disk.go`): after idle
  slots' build caches and images, before per-owner cache volumes.

### Isolation and authentication

- **Per-job credential.** For each non-strict runner, the pool registers the
  runner with the cache server and gets a token
  `<runner name>.<HMAC-SHA256(key, runner name, owner, repository restriction)>`;
  the key is random, kept in the data directory (jobs cannot read it), and the
  runner name is random and used for one job only. The token is in the runner
  container's environment only. It is valid while the runner is registered:
  `Release` revokes it, and a runner adopted after a restart is re-registered
  from its container labels (owner, target), so tokens survive restarts.
- **Owner isolation (hard).** The token maps to the target's GitHub owner
  (from Pitlane's own state, never from the job). The repository in the URL
  must belong to that owner, and for a repository target it must be that
  repository. Entries are stored per `owner/repo`; a job can never read or
  write another owner's entries.
- **Repository and branch scoping (best effort, like GitHub).** For an
  organization target, the repository comes from `GITHUB_REPOSITORY` (set by
  the runner); the branch scopes come from the job token's `ac` claim, which
  Pitlane decodes but cannot verify (GitHub does not publish the signing
  key). A malicious job could therefore read or write the cache of another
  repository or branch **of the same owner**. That is the same trust level as
  the rest of Pitlane's caches, which are shared per owner (toolcache,
  `~/.cache`, Docker layers, pnpm store) with no branch scoping at all.
- **Strict jobs get no Pitlane cache.** No `PITLANE_CACHE_URL` is injected;
  their actions use GitHub's cache, which GitHub isolates per repository and
  branch (including fork pull requests).
- **Cache scope `shared`** does not merge actions caches across owners: unlike
  the volumes, entries stay per repository, as on GitHub.

### Settings and UI

- `actions_cache` (default on) and `actions_cache_gb` (default 20) in the
  settings table; Settings → Caches shows them with the current size, entry
  count and hit/miss counters since start.
- Turning it off stops injecting `PITLANE_CACHE_URL` for new runners (the shim
  stays, inert) and keeps the stored entries until they are evicted or the
  directory is removed.

## Tests

- `internal/cache`: protocol handlers, matching rules, branch scopes, owner
  and repository isolation, tokens (revocation, identity binding, restarts),
  upload limits, LRU eviction, index reload; `TestNodeShim` runs the shim.
- `PITLANE_E2E=1 go test ./internal/cache -run Toolkit` drives the real
  `@actions/cache` client (6.3.0 by default; `PITLANE_E2E_TOOLKIT=4.1.0` or
  `5.3.0` for the versions bundled in `actions/cache@v4`/`v5`) through the
  shim, with the variables the runner sets on github.com
  (`ACTIONS_CACHE_SERVICE_V2=True`, GitHub URLs): 40 MB save in two chunks,
  restore by `restore-keys`, miss.
- `PITLANE_E2E_DOCKER=1 go test ./internal/pool -run PinnedRunner` installs
  the shim into the externals of the runner image pinned in
  `images/runner/Dockerfile` and runs each Node runtime through it.
