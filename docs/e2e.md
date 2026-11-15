# End-to-end tests

Pitlane is tested the way it is used: by running real GitHub Actions jobs on a
real Pitlane instance. The suite in [`e2e/`](../e2e/) is a set of
`workflow_dispatch` workflows, one per scenario, that each assert their own
success, and a script that pushes them to a test repository, runs them and
reports the results.

## Run it

You need a Pitlane instance that serves a test repository, and on your machine
`bash` 4+, `git`, `jq` and the [GitHub CLI](https://cli.github.com/)
authenticated with push access to that repository (`gh auth login`).

```sh
e2e/run.sh                              # every scenario on SheepHost/pitlane-org-test
e2e/run.sh my-org/pitlane-e2e           # another test repository
e2e/run.sh my-org/pitlane-e2e basic cache   # a selection
e2e/run.sh --twice                      # everything twice, to see warm caches
e2e/run.sh --label pitlane-staging      # another runs-on label
e2e/run.sh --help
```

The script:

1. clones the test repository, copies `e2e/workflows/*.yml` to
   `.github/workflows/` and `e2e/fixtures/` to `e2e/fixtures/`, removes
   `e2e-*.yml` workflows that no longer exist here, and pushes to the default
   branch only if something changed (`--no-sync` skips this step);
2. dispatches the scenarios at the same time, except `matrix`, which runs
   alone afterwards because it measures how many jobs run in parallel;
3. waits for every run, then prints one line per scenario run: conclusion,
   wall time, the longest time a job waited for a runner (which includes
   waiting for a free slot), the timings the jobs reported, and a link to the
   run;
4. exits with status 1 if any run did not succeed.

Use a throwaway private repository: the suite pushes to its default branch and
uploads short-lived artifacts and caches. No token is read or printed: git and
API calls go through `gh`'s own credential handling.

## Scenarios

| Scenario | What it checks |
| --- | --- |
| `basic` | A plain job: env precedence (workflow, job, step), `GITHUB_ENV`, `GITHUB_PATH`, step and job outputs (multiline too), and real JavaScript actions with **hyphenated inputs**: `actions/checkout` `fetch-depth: 2`, `actions/setup-node` `node-version: 22.11.0`, `actions/upload-artifact` `include-hidden-files: true`, checked by a dependent job. A runner that loses `INPUT_*` variables with a dash in their name falls back to each input's default, and these assertions fail. |
| `services` | `services:` Postgres and Redis with health checks: both healthy, a SQL and a Redis round trip on `localhost`, and a step container on the slot's host network. |
| `container` | A `container:` job on `node:22-bookworm-slim`: steps run inside it, `actions/checkout` and `actions/github-script` run in the container, a service is reached by its name on the job network. |
| `docker` | `docker buildx build` twice: the second build must reuse every layer (under 8 s, `CACHED` on the slow layer); the first one is reported as warm or cold depending on what the slot built before. `docker compose up --wait`, requests to the stack, `down -v`. |
| `cache` | `actions/cache/save` in one job, then in a dependent job a restore by exact key, a restore by `restore-keys` prefix (`cache-hit: false`, matched key checked), and a clean miss. `actions/setup-node` with `cache: npm` and `npm ci` on a fixture lockfile. |
| `artifacts` | Two jobs upload artifacts (50 small files in nested directories, and 100 MiB); a third downloads one by name and all by pattern, and checks every SHA-256. |
| `matrix` | 4 matrix legs that each hold a runner for 30 s: every leg must run on a distinct runner, and at least 4 (`--min-concurrency`) must overlap. |
| `logs` | 200,000 log lines (about 20 MB), log groups, `add-mask`, a failing step with `continue-on-error`, `failure()` and `always()` conditions; the job must succeed. |
| `multiarch` | `docker/setup-qemu-action` and `docker/setup-buildx-action`, then a `linux/arm64` build on an amd64 runner, and the image run under emulation. |

Job timeouts are not covered: they depend on the instance's settings.

## Expected results

Every scenario concludes `success`. A typical run against a 4-slot instance:

| Scenario | Wall time | Notes |
| --- | --- | --- |
| `basic`, `services`, `container`, `docker`, `cache`, `artifacts`, `logs`, `multiarch` | 2–3 min each, in parallel | Dispatched together, so most of the time is jobs queuing for a free slot. |
| `matrix` | about 1 min | Runs alone: peak concurrency 4/4, legs start within a second of each other. |

Timings to look at in the report:

- **docker build 2** is 0–1 s; **docker build 1** is cold (about 30 s, mostly
  the 8 s sleep and pulling `alpine`) the first time a slot builds the image,
  then warm.
- **npm ci** takes a second or two: Pitlane keeps the npm cache on disk, even
  when setup-node reports `cache-hit=false` on a first run.
- **arm64 build under QEMU** is around 15–20 s for the fixture image.

`multiarch` registers QEMU `binfmt_misc` handlers, which are global to the
host kernel. On a host where that is not allowed (some unprivileged containers),
it fails at `setup-qemu-action`: exclude it with a scenario selection.

## Add a scenario

1. Add `e2e/workflows/e2e-<name>.yml` with `on: workflow_dispatch`, a `label`
   input and `runs-on: ${{ inputs.label || 'pitlane' }}` on every job.
2. Make the workflow fail on its own when the behaviour breaks: assert with
   `test`, not by eye.
3. Report a timing with `echo "::notice title=e2e-timing::<what>: <n>s"`; the
   script collects these annotations.
4. Add `<name>` to `ALL_SCENARIOS` in `e2e/run.sh` and a row to the table above.
   Pin every action by full commit SHA, as in the other scenarios.
