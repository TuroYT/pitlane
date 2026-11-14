package cache

// NodeShim replaces each `externals/node*/bin/node` of the runner (the real
// binary is kept next to it as `node.pitlane`). Runner.Worker starts every
// JavaScript action with that path and sets ACTIONS_CACHE_SERVICE_V2 and the
// cache URLs itself, after the container's environment, so this is the only
// place where they can be changed without patching the runner.
//
// It only acts when Pitlane gave the runner container a cache URL
// (PITLANE_CACHE_URL, absent for strict jobs, when the cache is off, and
// inside job containers, which do not inherit it): @actions/cache then uses
// its v1 protocol against Pitlane, artifacts keep using ACTIONS_RESULTS_URL
// (GitHub). In every other case it runs Node unchanged, so jobs fall back to
// GitHub's cache. The runner already runs `sh -c` in job containers before
// any JavaScript action (OS detection), so requiring /bin/sh adds nothing.
const NodeShim = `#!/bin/sh
# Pitlane: serve actions/cache from the Pitlane host (docs/design/actions-cache.md).
if [ -n "$PITLANE_CACHE_URL" ] && [ -n "$GITHUB_REPOSITORY" ]; then
  export ACTIONS_CACHE_URL="$PITLANE_CACHE_URL$GITHUB_REPOSITORY/"
  unset ACTIONS_CACHE_SERVICE_V2
fi
self=$(readlink -f "$0" 2>/dev/null) || self=$0
exec "$self.pitlane" "$@"
`

// ShimVersion changes whenever NodeShim does, so that slots refresh their
// copy of the runner externals.
const ShimVersion = "1"
