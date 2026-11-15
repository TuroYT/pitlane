#!/usr/bin/env bash
# Turns the `go test -json` events of a CI run into GitHub annotations (one per
# failing test, on the line that failed when its output names one) and a
# Markdown job summary: one row per package with its result, test counts, time
# and statement coverage.
#
# Usage: go-test-report.sh <go-test.json> [coverage.out]
set -euo pipefail

events=$1
profile=${2:-}
module=$(go list -m)
summary=${GITHUB_STEP_SUMMARY:-/dev/stdout}

[ -s "$events" ] || { echo "::error title=Go tests::no test events recorded (the tests did not start)"; exit 0; }

# --- annotations ---------------------------------------------------------------
# Failing tests: "    foo_test.go:42: message" lines of the test's output point
# at the assertion. Without one (a panic, a timeout), annotate the package.
jq -rs --arg module "$module" '
  def dir: ltrimstr($module) | ltrimstr("/");
  def esc: gsub("%"; "%25") | gsub("\r"; "%0D") | gsub("\n"; "%0A");
  def prop: esc | gsub(":"; "%3A") | gsub(","; "%2C");
  (map(select(.Action == "fail" and .Test != null)) | map({Package, Test})) as $failed
  # A parent test fails when one of its subtests does: report the subtest only.
  | ($failed | map(. as $f | select($failed | any(.Package == $f.Package and (.Test | startswith($f.Test + "/"))) | not))) as $leaves
  | group_by(.Package + "\u0000" + (.Test // ""))
  | map({Package: .[0].Package, Test: .[0].Test, out: (map(select(.Action == "output") | .Output) | join(""))})
  | map(. as $t | select($leaves | any(.Package == $t.Package and .Test == $t.Test)))
  | .[]
  | (.out | [scan("(?m)^\\s+([A-Za-z0-9_./-]+_test\\.go):([0-9]+): (.*)$")]) as $hits
  | if ($hits | length) > 0 then
      $hits[] as $h
      | "::error file=\(.Package | dir | if . == "" then "" else . + "/" end)\($h[0]),line=\($h[1]),title=\(.Test | prop) failed::\($h[2] | esc)"
    else
      "::error title=\("\(.Package | dir): \(.Test) failed" | prop)::\(.out | .[-3000:] | esc)"
    end
' "$events" 2>/dev/null || true

# Build and vet errors are annotated by setup-go's problem matcher.

# --- summary -------------------------------------------------------------------
{
  echo "### Go tests (race detector)"
  echo
  jq -rs --arg module "$module" '
    def dir: ltrimstr($module) | ltrimstr("/") | if . == "" then "." else . end;
    def icon: if . == "pass" then "✅" elif . == "fail" then "❌" elif . == "skip" then "⏭️" else "❔" end;
    (map(select(.Action == "fail" and .Test != null)) | length) as $failed
    | (map(select(.Action == "pass" and .Test != null)) | length) as $passed
    | (map(select(.Action == "skip" and .Test != null)) | length) as $skipped
    | "**\($passed) passed, \($failed) failed, \($skipped) skipped**\n",
      "| | Package | Tests | Failed | Time | Coverage |",
      "| --- | --- | ---: | ---: | ---: | ---: |",
      ( group_by(.Package)[]
        | select(.[0].Package != null)
        | (map(select(.Test == null and (.Action == "pass" or .Action == "fail" or .Action == "skip"))) | last) as $end
        | select($end != null)
        | ([.[] | select(.Action == "output" and .Test == null) | .Output
            | capture("coverage: (?<c>[0-9.]+)% of statements")?.c] | last) as $cov
        | "| \($end.Action | icon) | `\(.[0].Package | dir)` | \([.[] | select(.Test != null and (.Action == "pass" or .Action == "fail"))] | length) | \([.[] | select(.Test != null and .Action == "fail")] | length) | \($end.Elapsed // 0 | . * 10 | round / 10)s | \(if $cov then $cov + "%" else "–" end) |"
      )
  ' "$events"
  if [ -n "$profile" ] && [ -s "$profile" ]; then
    echo
    echo "Total statement coverage: **$(go tool cover -func="$profile" | awk '/^total:/ {print $NF}')**"
  fi
} >>"$summary"
