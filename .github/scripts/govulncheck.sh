#!/usr/bin/env bash
# Runs govulncheck on the module and fails when the code calls a vulnerable
# symbol (the exit status govulncheck itself gives), except for the advisories
# listed in govulncheck-ignore.txt, each with the reason it does not apply.
# Vulnerabilities only found in modules we require, in code we do not call,
# are not reported.
#
# Usage: govulncheck.sh <govulncheck command...>
#   govulncheck.sh go run golang.org/x/vuln/cmd/govulncheck@v1.10.0
set -euo pipefail

ignore_file=$(dirname "$0")/govulncheck-ignore.txt
report=$(mktemp)
trap 'rm -f "$report"' EXIT

# The JSON format always exits 0, so the verdict is computed below.
"$@" -format json ./... > "$report"

# One "ID<TAB>summary" line per advisory with a finding at symbol level (the
# first frame of its trace names a function).
called=$(jq -rs '
  (map(select(.osv) | .osv) | map({key: .id, value: .summary}) | from_entries) as $summary
  | map(select(.finding) | .finding | select(.trace[0].function != null) | .osv) | unique[]
  | "\(.)\t\($summary[.] // "")"' "$report")

ignored=$(sed -E 's/[[:space:]]*#.*//; /^[[:space:]]*$/d' "$ignore_file" | awk '{print $1}')

failed=0
while IFS=$'\t' read -r id summary; do
  [ -n "$id" ] || continue
  if grep -qxF "$id" <<<"$ignored"; then
    echo "ignored: $id $summary (see $(basename "$ignore_file"))"
  else
    echo "::error title=govulncheck::$id: $summary (https://pkg.go.dev/vuln/$id)"
    failed=1
  fi
done <<<"$called"

for id in $ignored; do
  grep -qF "$id" <<<"$called" || echo "::warning title=govulncheck::$id is in $(basename "$ignore_file") but is no longer reported: remove it"
done

if [ "$failed" -ne 0 ]; then
  echo "Vulnerable code is called: update the dependency, or run '$* ./...' for the call stacks." >&2
  exit 1
fi
echo "govulncheck: no vulnerability in the code we call, except the ignored ones."
