# CI and checks

Every pull request runs the [`CI` workflow](../.github/workflows/ci.yml). Its
jobs run in parallel and skip what a change cannot break: a pull request that
only touches `docs/` runs the docs build and nothing else. Pitlane's own CI
runs on Pitlane (the `PITLANE_RUNNER` repository variable); pull requests from
forks run on GitHub-hosted runners.

## The checks

| Check | Runs when the pull request changes | What it does | Where to look when it fails |
| --- | --- | --- | --- |
| `Changes` | always | Lists the changed files and decides which jobs run. | The job summary lists the jobs that run and those skipped. |
| `Go / lint` | Go code, `go.mod`, `.golangci.yml`, `Makefile`, `web/` | `gofmt`, `go vet`, `go mod tidy -diff`, golangci-lint. | Annotations on the offending lines; the summary lists the golangci-lint issues. |
| `Go / test (race)` | same as `Go / lint` | `go test -race` with coverage, through gotestsum. | Annotations on the failing assertion; the summary has one row per package with its tests, time and coverage. The log has one group per package. |
| `Go / build` | same as `Go / lint` | Builds the UI, then the `pitlane` binary with the UI embedded (`make build`). | Compiler errors are annotated. |
| `Web / build, lint, test` | `web/` | `vue-tsc` and `vite build`, ESLint with no warnings allowed, Vitest. | ESLint errors and failing tests are annotated; Vitest writes its own summary. |
| `Docs / build` | `docs/`, `site/` | Builds the VitePress site, which fails on a dead link. | The log names the page and the link. |
| `all green` | always | Fails if any job above failed or was cancelled. Skipped jobs count as passed. | The summary lists the result of every job. |

A change to the CI itself (`.github/workflows/ci.yml`, `.github/actions/`,
`.github/scripts/`) runs every job. Pushes to `main` run every job except the
docs build: the [`docs` workflow](../.github/workflows/docs.yml) builds and
publishes the site from `main`.

A new push to a pull request cancels the run it replaces. Every job has a
timeout, and the workflow token can only read the repository.

## Which check to require

Require **`all green`** only (the GitHub UI shows it as "CI / all green").
It reports on every pull request, including those where most jobs are skipped,
so a docs-only pull request is not blocked waiting for a Go check that never
runs. Requiring the individual jobs instead would block those pull requests.

## Run the checks locally

```sh
make lint        # Go / lint
make test-race   # Go / test (race)
make web build   # Go / build
cd web && pnpm build && pnpm lint && pnpm test   # Web / build, lint, test
cd site && pnpm install && pnpm build            # Docs / build
```

The versions of golangci-lint and gotestsum are pinned in the `Makefile`;
Renovate proposes new ones.

## Dependency updates

Renovate ([`renovate.json`](../renovate.json)) opens its pull requests on
Monday mornings (Europe/Paris), grouped:

- merged automatically once `all green` passes: minor and patch updates of
  GitHub Actions and of the build tooling (`devDependencies` of `web/`, the
  docs site), and image digest updates (except `tonistiigi/binfmt`, used only
  by the release workflow);
- left for a maintainer: Go modules, the UI's runtime dependencies, the Go
  tools of the `Makefile` and every major update.

Security fixes skip the schedule and are opened as soon as they are known.
Everything waits 3 days after its release (14 days for the docs site), except
security fixes and Pitlane's own images.

## End-to-end tests

The [`e2e` workflow](../.github/workflows/e2e.yml) runs the [end-to-end
suite](e2e.md) from the Actions tab (manual only). It needs a repository secret
`E2E_GITHUB_TOKEN`: a fine-grained token restricted to the test repository,
with **Contents**, **Actions** and **Workflows** read and write (the suite
pushes its `e2e-*.yml` files to the test repository's `.github/workflows`).
Without it the workflow stops with an error that says so.
