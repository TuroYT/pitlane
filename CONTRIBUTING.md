# Contributing to Pitlane

Thanks for taking the time. Pitlane has one maintainer, so small, focused pull
requests get reviewed fastest. For anything bigger than a bug fix, open an
issue or a discussion first so we can agree on the approach before you write
the code.

Questions go to [Discussions](https://github.com/TuroYT/pitlane/discussions),
bugs and feature requests to [issues](https://github.com/TuroYT/pitlane/issues/new/choose),
and vulnerabilities to a private advisory (see [SECURITY.md](SECURITY.md)).

## Development setup

You need:

- Go, at the version in [`go.mod`](go.mod) (currently 1.27.3). With a recent Go
  and the default `GOTOOLCHAIN=auto`, the right toolchain is downloaded for you.
- Node.js 22 and pnpm 10 for the web UI (`web/`) and the docs site (`site/`).
- Docker, if you want to run Pitlane for real. Its slots are privileged
  Docker-in-Docker containers, so a VM or a throwaway host is a better place
  than your laptop's daily Docker.

The repository layout:

| Path | What it is |
| --- | --- |
| `cmd/pitlane` | The binary: flags, wiring, the embedded UI |
| `internal/pool` | Slots (Docker-in-Docker), caches, runner containers, cgroup usage |
| `internal/scaler` | GitHub scale sets, job lifecycle, reconciliation |
| `internal/server` | HTTP API, auth, roles, metrics |
| `internal/cache` | The `actions/cache` server |
| `internal/store` | SQLite storage and migrations |
| `internal/update` | Self-update and provenance checks |
| `web/` | Vue 3 + Vite UI, embedded into the binary from `web/dist` |
| `docs/`, `site/` | Markdown docs and the VitePress site that renders them |
| `deploy/` | `install.sh`, the systemd unit, Compose file, Proxmox LXC script |
| `images/runner` | The runner image released with each version |
| `e2e/` | End-to-end workflows run against a real instance |

## Make targets

| Target | What it does |
| --- | --- |
| `make web` | `pnpm install --frozen-lockfile && pnpm build` in `web/`, output in `web/dist` |
| `make build` | Builds `bin/pitlane` with the UI embedded (run `make web` first) |
| `make run` | `web` + `build`, then runs Pitlane against your local Docker daemon on `127.0.0.1:8080`, data in `./.data`, **login disabled** (`--insecure-no-auth`) |
| `make dev` | Vite dev server for the UI, proxying `/api` and `/auth` to a Pitlane on `:8080` (or `PITLANE_URL`) |
| `make test` | `go vet ./...` and `go test ./...` |
| `make check` | Everything CI runs on code, stopping at the first failure (see [Tests and linters](#tests-and-linters)) |
| `make help` | Lists the targets |
| `make clean` | Removes `bin/`, `.data/`, `web/node_modules` and the built UI |

`make run` turns authentication off and starts real privileged containers.
Keep it on loopback and on a machine where that is fine.

## Working on the UI without a Pitlane

`docs/assets/src/screenshots/mock.mjs` serves a deterministic fake API (jobs,
targets, users, settings), so you can work on the UI without GitHub or Docker:

```sh
node docs/assets/src/screenshots/mock.mjs 18080 &
cd web && pnpm install && PITLANE_URL=http://localhost:18080 pnpm dev
```

Then open the URL Vite prints. The mock needs no `npm install` of its own;
only the screenshot scripts next to it do (see
[their README](docs/assets/src/screenshots/README.md)). If your change adds an
API field the UI depends on, add it to the mock too.

## Tests and linters

CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs the following.
Run them before you push. `make check` runs all of them in this order and
stops at the first failure:

```sh
make check
```

The individual steps are:

```sh
make web                              # builds web/dist, embedded in the binary
(cd web && pnpm test && pnpm lint)    # vitest, eslint with --max-warnings 0
test -z "$(gofmt -l ./cmd ./internal ./web)"
go vet ./...
go mod tidy -diff
make golangci-lint
go test -race ./...
make build
```

`go test -race` needs cgo (a C compiler). golangci-lint is pinned in the
`Makefile` (`GOLANGCI_LINT_VERSION`), which CI also uses; running it through `go run` builds it with your Go toolchain,
so it understands the Go version in `go.mod`. Its configuration is in
[`.golangci.yml`](.golangci.yml).

`pnpm build` in `web/` also type-checks with `vue-tsc`. If you change the docs
site, `cd site && pnpm install && pnpm build` catches broken pages.

## End-to-end tests

Unit tests use fakes for Docker and GitHub. The e2e suite runs real GitHub
Actions jobs on a real Pitlane instance: it pushes `e2e/workflows/` to a test
repository, dispatches every scenario and checks the results. You need your
own Pitlane serving a throwaway private repository, plus `bash`, `git`, `jq`
and an authenticated `gh`. See [docs/e2e.md](docs/e2e.md).

You do not have to run it for every pull request. Mention in the pull request
whether you did, and on what host. For changes to slots, caches, the runner
image or job handling, a run is very welcome; the maintainer runs it before a
release anyway.

## Security note for testing

Every Pitlane job controls a privileged Docker daemon and can take over the
host. Never point a shared Pitlane (your company's, a friend's homelab) at a
branch, fork or workflow you have not read. Test on a host you own and can
rebuild. [docs/security.md](docs/security.md) explains why.

## Commits

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/),
as in `git log`:

```
feat(metrics): add pitlane_slot_wipes_total
fix(gh): an org billing manager is not an org member
perf(pool): pick the slot to wipe by expected cache loss
docs: reproducible UI screenshot scripts
style(web): polish the dashboard
chore(deps): update alpine docker tag to v3.24
```

Types in use: `feat`, `fix`, `perf`, `refactor`, `test`, `docs`, `style`,
`build`, `ci` and `chore`. The scope is the package or area (`pool`, `scaler`, `server`,
`cache`, `web`, `deps`…). The subject is lower case, in the present tense, with
no trailing period. Say why in the body when the diff does not make it obvious.

## Pull requests

- Keep each pull request to one change. A refactor and a feature are two pull
  requests.
- Add or update tests for the behaviour you change. Go tests sit next to the
  code (`*_test.go`), UI tests next to the module (`*.test.ts`).
- Update the docs in `docs/` in the same pull request when you change a flag,
  a setting, a default or anything a user would notice.
- Describe behaviour changes plainly, especially anything that affects running
  jobs, caches, upgrades or security.
- For UI changes, attach before and after screenshots, light and dark if the
  change touches colors.
- Do not bump dependencies by hand unless you need to: Renovate does it.
- CI must be green. Pull requests from forks run on GitHub-hosted runners and
  need the maintainer's approval before their workflows start.

## Releases

Releases are cut by the maintainer. Only repository admins can push `v*` tags
(a tag ruleset enforces it).

1. A tag `vX.Y.Z` (or `vX.Y.Z-rc.N` for a pre-release) is pushed from `main`.
2. [`release.yml`](.github/workflows/release.yml) runs on GitHub-hosted runners,
   never on Pitlane itself. It builds the `amd64` and `arm64` binaries,
   `checksums.txt` and the installers, attests their build provenance, builds
   and pushes `ghcr.io/turoyt/pitlane` and `ghcr.io/turoyt/pitlane-runner`
   (with SBOM and attestations), then creates the GitHub release with generated
   notes. Final releases also move the `vX.Y` and `latest` image tags.
3. Users can check a download with `gh attestation verify`. The in-app updater
   refuses a binary whose provenance it cannot verify.

Release notes come from merged pull request titles, so give your pull request a
title that reads well in a changelog.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). By
taking part, you agree to it.
