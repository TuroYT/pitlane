#!/usr/bin/env bash
# Pitlane end-to-end suite: runs real GitHub Actions jobs on a Pitlane instance.
#
# Syncs e2e/workflows/*.yml (as .github/workflows/e2e-*.yml) and e2e/fixtures
# into a test repository served by the Pitlane instance, pushes them to its
# default branch if they changed, dispatches each scenario, waits, and prints
# one line per scenario. Exits non-zero if any scenario did not succeed.
#
# Usage: e2e/run.sh [options] [owner/repo] [scenario...]
#
#   owner/repo       test repository (default: SheepHost/pitlane-org-test)
#   scenario         basic services container docker cache artifacts matrix
#                    logs multiarch (default: all)
#   -l, --label L    runs-on label passed to every workflow (default: pitlane)
#   -2, --twice      run the whole selection twice, to show warm-cache effects
#   -n, --no-sync    do not push the workflows, dispatch what is already there
#   -c, --min-concurrency N
#                    legs of the matrix that must overlap (default: 4)
#   -t, --timeout S  give up waiting for a run after S seconds (default: 1800)
#   -h, --help
#
# Needs bash, git, jq and an authenticated gh CLI (`gh auth login`) with push
# access to the test repository. No token is read, printed or stored: git and
# the API calls go through gh's own credential handling.
set -euo pipefail

ALL_SCENARIOS=(basic services container docker cache artifacts matrix logs multiarch)
# Scenarios that need the instance to themselves (they measure concurrency).
EXCLUSIVE=(matrix)

repo=SheepHost/pitlane-org-test
label=pitlane
passes=1
sync=1
min_concurrency=4
timeout=1800
scenarios=()

die() { echo "e2e: $*" >&2; exit 2; }
log() { echo "e2e: $*" >&2; }
usage() { sed -n '2,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//;p}' "$0"; }

while [ $# -gt 0 ]; do
  case "$1" in
    -l|--label) label=${2:?}; shift 2 ;;
    -2|--twice) passes=2; shift ;;
    -n|--no-sync) sync=0; shift ;;
    -c|--min-concurrency) min_concurrency=${2:?}; shift 2 ;;
    -t|--timeout) timeout=${2:?}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    -*) die "unknown option $1 (see --help)" ;;
    */*) repo=$1; shift ;;
    *) scenarios+=("$1"); shift ;;
  esac
done
[ ${#scenarios[@]} -gt 0 ] || scenarios=("${ALL_SCENARIOS[@]}")

here=$(cd "$(dirname "$0")" && pwd)
for s in "${scenarios[@]}"; do
  [ -f "$here/workflows/e2e-$s.yml" ] || die "unknown scenario '$s' (have: ${ALL_SCENARIOS[*]})"
done
for tool in gh git jq; do command -v "$tool" >/dev/null || die "$tool is required"; done
gh auth status >/dev/null 2>&1 || die "gh is not authenticated (run gh auth login)"

branch=$(gh repo view "$repo" --json defaultBranchRef --jq .defaultBranchRef.name)
[ -n "$branch" ] || die "cannot read $repo"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# --- sync -------------------------------------------------------------------
if [ "$sync" = 1 ]; then
  log "syncing workflows into $repo@$branch"
  gh repo clone "$repo" "$work/repo" -- --quiet --depth 1 --branch "$branch"
  dst=$work/repo
  mkdir -p "$dst/.github/workflows"
  rm -f "$dst"/.github/workflows/e2e-*.yml
  cp "$here"/workflows/e2e-*.yml "$dst/.github/workflows/"
  rm -rf "$dst/e2e/fixtures"
  mkdir -p "$dst/e2e"
  cp -R "$here/fixtures" "$dst/e2e/fixtures"
  git -C "$dst" add -A .github/workflows e2e
  if git -C "$dst" diff --cached --quiet; then
    log "workflows already up to date"
  else
    src=$(git -C "$here" rev-parse --short HEAD 2>/dev/null || echo unknown)
    git -C "$dst" commit --quiet -m "e2e: sync the Pitlane e2e suite (pitlane $src)"
    # Push through gh's credential helper, whatever git's own config says.
    git -C "$dst" -c credential.helper= -c 'credential.helper=!gh auth git-credential' \
      push --quiet origin "HEAD:$branch"
    log "pushed $(git -C "$dst" rev-parse --short HEAD)"
  fi
fi

# --- dispatch & wait ----------------------------------------------------------
# GitHub registers pushed workflows asynchronously: retry the first dispatch.
dispatch() { # scenario -> run id on stdout
  local wf="e2e-$1.yml" id="" since tries=0
  local args=(-X POST "repos/$repo/actions/workflows/$wf/dispatches"
              -f "ref=$branch" -f "inputs[label]=$label" -F return_run_details=true)
  [ "$1" = matrix ] && args+=(-f "inputs[min-concurrency]=$min_concurrency")
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  until out=$(gh api "${args[@]}" 2>&1); do
    tries=$((tries + 1))
    [ $tries -lt 12 ] || { echo "$out" >&2; die "cannot dispatch $wf (GitHub rejects workflows with invalid YAML this way: check the run list of the repository)"; }
    sleep 5
  done
  id=$(jq -r '.workflow_run_id // empty' <<<"$out" 2>/dev/null || true)
  # Older API versions return 204 with no body: find the run we just created.
  while [ -z "$id" ]; do
    sleep 3
    id=$(gh api "repos/$repo/actions/workflows/$wf/runs?event=workflow_dispatch&branch=$branch&created=>=$since&per_page=1" \
           --jq '.workflow_runs[0].id // empty')
  done
  echo "$id"
}

wait_runs() { # run ids...
  local deadline=$(( $(date +%s) + timeout )) pending=("$@") next id st
  while [ ${#pending[@]} -gt 0 ]; do
    next=()
    for id in "${pending[@]}"; do
      st=$(gh api "repos/$repo/actions/runs/$id" --jq .status)
      [ "$st" = completed ] || next+=("$id")
    done
    pending=("${next[@]+"${next[@]}"}")
    [ ${#pending[@]} -gt 0 ] || break
    if [ "$(date +%s)" -ge "$deadline" ]; then
      log "timed out waiting for runs: ${pending[*]}"; return 1
    fi
    sleep 10
  done
}

declare -A run_of
run_pass() { # pass number
  local p=$1 s group=() exclusive=() ids=()
  for s in "${scenarios[@]}"; do
    if [[ " ${EXCLUSIVE[*]} " == *" $s "* ]]; then exclusive+=("$s"); else group+=("$s"); fi
  done
  if [ ${#group[@]} -gt 0 ]; then
    for s in "${group[@]}"; do run_of[$p:$s]=$(dispatch "$s"); ids+=("${run_of[$p:$s]}"); log "pass $p: $s -> run ${run_of[$p:$s]}"; done
    wait_runs "${ids[@]}" || true
  fi
  for s in "${exclusive[@]+"${exclusive[@]}"}"; do
    run_of[$p:$s]=$(dispatch "$s"); log "pass $p: $s -> run ${run_of[$p:$s]} (alone)"
    wait_runs "${run_of[$p:$s]}" || true
  done
}

for p in $(seq 1 "$passes"); do run_pass "$p"; done

# --- report -------------------------------------------------------------------
failed=0
rows=()
for p in $(seq 1 "$passes"); do
  for s in "${scenarios[@]}"; do
    id=${run_of[$p:$s]}
    run=$(gh api "repos/$repo/actions/runs/$id")
    jobs=$(gh api --paginate "repos/$repo/actions/runs/$id/jobs?per_page=100" --jq '.jobs[]' | jq -s .)
    conclusion=$(jq -r '.conclusion // .status' <<<"$run")
    url=$(jq -r .html_url <<<"$run")
    # Wall time of the run, and the longest a job waited for a runner
    # (queued -> started), which includes waiting for a free slot.
    duration=$(jq -r '((.updated_at | fromdateiso8601) - (.run_started_at | fromdateiso8601))' <<<"$run")
    pickup=$(jq -r '[.[] | select(.started_at and .created_at)
                     | ((.started_at | fromdateiso8601) - (.created_at | fromdateiso8601))] | max // 0' <<<"$jobs")
    timings=""
    for jid in $(jq -r '.[].id' <<<"$jobs"); do
      t=$(gh api "repos/$repo/check-runs/$jid/annotations" \
            --jq '[.[] | select(.title == "e2e-timing") | .message] | sort | join("; ")' 2>/dev/null || true)
      [ -z "$t" ] || timings="${timings:+$timings; }$t"
    done
    [ "$conclusion" = success ] || failed=$((failed + 1))
    rows+=("$p|$s|$conclusion|${duration}s|${pickup}s|${timings:--}|$url")
  done
done

echo
echo "Pitlane e2e on $repo (label: $label)"
{
  echo "pass|scenario|conclusion|duration|max queue|timings|run"
  printf '%s\n' "${rows[@]}"
} | if command -v column >/dev/null; then column -t -s '|'; else tr '|' '\t'; fi
echo
if [ "$failed" -gt 0 ]; then
  echo "e2e: $failed scenario run(s) did not succeed" >&2
  exit 1
fi
echo "e2e: all scenario runs succeeded"
