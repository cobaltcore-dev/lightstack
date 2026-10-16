#!/bin/bash
# coverage.sh — the CLI conformance coverage gate (SPEC-028).
#
# Cross-references the command manifests (test/cli/manifest/*.json) with the
# recorded results (test/cli/results/results.json) and prints the per-service
# matrix. This is the machine-checkable definition of "all cases".
#
# Usage:
#   coverage.sh [--results <file>] [--require-tiers A,B] [--strict] [--quiet]
#
#   --results <file>     results.json to read (default test/cli/results/results.json)
#   --require-tiers      tiers that must be covered and passing (default: A,B)
#   --strict             additionally require tier C (workflows) — used by the
#                        nightly full run
#   --quiet              matrix only, no header/footer chatter
#
# Status per command×tier:
#   pass           a matching case recorded with status pass
#   fail           a matching case recorded as fail               → gate fails
#   known-failing  a matching case recorded as known-failing      → allowed, counted
#   skip           a matching case recorded as skip               → gate fails
#   untested       no matching case at all                        → gate fails
#
# Tier→case-id mapping: tier A uses the manifest id as the case id; tier B uses
# "<id>.neg". Tier C entries are workflow scenario ids used verbatim.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
MANIFEST_DIR="$CLI_ROOT/manifest"
DEFAULT_RESULTS="$CLI_ROOT/results/results.json"

RESULTS="$DEFAULT_RESULTS"
REQUIRE_TIERS="A,B"
QUIET=0

# A value-taking option must have its value: without this, a trailing bare
# `--results` under `set -u` dies with an unbound-variable error naming $2
# instead of naming the option (run.sh guards the same way with _need_arg).
_need_arg() {
    if (( $# < 2 )); then
        echo "ERROR: option $1 requires a value" >&2
        exit 2
    fi
}

while (( $# > 0 )); do
    case "$1" in
        --results)       _need_arg "$@"; RESULTS="$2"; shift 2 ;;
        --require-tiers) _need_arg "$@"; REQUIRE_TIERS="$2"; shift 2 ;;
        --strict)        REQUIRE_TIERS="A,B,C"; shift ;;
        --quiet)         QUIET=1; shift ;;
        *)               echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

if ! ls "$MANIFEST_DIR"/*.json >/dev/null 2>&1; then
    echo "ERROR: no manifests found in $MANIFEST_DIR" >&2
    exit 2
fi
if [[ ! -f "$RESULTS" ]]; then
    echo "ERROR: no results at $RESULTS — run ./test/cli/bin/run.sh first" >&2
    exit 2
fi

# Fail closed: a truncated, empty, or corrupt results file (e.g. a partial
# write from a killed runner) or an unparseable manifest must FAIL the gate,
# not iterate over nothing and print a vacuous pass. Every jq that feeds the
# gate is exit-checked, and a zero-row result is itself an error.
MANIFESTS_JSON=$(jq -s '.' "$MANIFEST_DIR"/*.json) || {
    echo "ERROR: manifests in $MANIFEST_DIR are not parseable JSON" >&2
    exit 2
}
CASES_JSON=$(jq '.cases // []' "$RESULTS") || {
    echo "ERROR: $RESULTS is not parseable JSON — rerun ./test/cli/bin/run.sh" >&2
    exit 2
}

# Flatten: every manifest command × required tier, joined with its case status.
if ! ROWS=$(jq -n \
    --argjson manifests "$MANIFESTS_JSON" \
    --argjson in_cases "$CASES_JSON" \
    --arg required_tiers "$REQUIRE_TIERS" '
[$manifests[]
 | . as $m
 | (.commands // [])[] as $c
 | ($required_tiers | split(","))[] as $tier
 | select(($c.tiers // ["A"]) | index($tier))
 | ($c.id + (if $tier == "B" then ".neg" else "" end)) as $case_id
 | ([$in_cases[] | select(.id == $case_id)] | first | .status // "untested") as $status
 | {service: ($m.service // "unknown"), id: $c.id, tier: $tier,
   cmd: ($c.cmd // ""), case_id: $case_id, status: $status}
]'); then
    echo "ERROR: failed to compute coverage rows from $RESULTS and the manifests" >&2
    exit 2
fi
if [[ -z "$ROWS" ]] || [[ "$(printf '%s' "$ROWS" | jq 'length')" == "0" ]]; then
    echo "ERROR: coverage computed zero rows — empty results or empty manifests; a gate over nothing proves nothing" >&2
    exit 2
fi

# Summary.
SUMMARY=$(printf '%s' "$ROWS" | jq '{
    total: length,
    pass: [.[] | select(.status == "pass")] | length,
    fail: [.[] | select(.status == "fail")] | length,
    known_failing: [.[] | select(.status == "known-failing")] | length,
    skipped: [.[] | select(.status == "skip")] | length,
    untested: [.[] | select(.status == "untested")] | length,
}')

# Per-service matrix.
(( QUIET )) || printf '%s\n' "$SUMMARY" | jq -r '
    "Coverage summary (required tiers: see exit status)",
    "  total:         \(.total)",
    "  pass:          \(.pass)",
    "  fail:          \(.fail)",
    "  known-failing: \(.known_failing)",
    "  skipped:       \(.skipped)",
    "  untested:      \(.untested)"
' >&2
printf '%s\n' "$ROWS" | jq -r '
    group_by(.service)[]
    | .[0].service as $s
    | "\($s): \([.[] | select(.status == "pass")] | length)/\(length) pass,"
      + " \([.[] | select(.status == "fail")] | length) fail,"
      + " \([.[] | select(.status == "known-failing")] | length) known-failing,"
      + " \([.[] | select(.status == "untested")] | length) untested"
'

# The gate: fail on fail / untested / skip in the required tiers.
VIOLATIONS=$(printf '%s' "$ROWS" | jq -r \
    '.[] | select(.status == "fail" or .status == "untested" or .status == "skip")
     | "  [\(.tier)] \(.id) (\(.status)): \(.cmd)"')
if [[ -n "$VIOLATIONS" ]]; then
    echo
    echo "Coverage gate FAILED — untested, failing, or skipped commands:"
    printf '%s\n' "$VIOLATIONS"
    exit 1
fi

(( QUIET )) || echo "Coverage gate PASSED"
exit 0
