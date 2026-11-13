#!/bin/bash
# baseline.sh — checked-in known-failure baseline for the CLI conformance gates (SPEC-028).
#
# The baseline is the ledger of cases that are *structurally unpassable* against
# the current pinned environment (stub-mode server or a pinned-client bug). A
# case recorded `known-failing` must appear here, or the gate fails: otherwise a
# regression that flips a regular case to known-failing would pass both
# `coverage.sh` and `run.sh --strict`.
#
# File format (baseline/known_failures.txt): one case id per line; blank lines
# and everything after `#` are ignored, so each entry can carry its reason.
# Override the path with CLI_BASELINE_FILE (used by the harness self-tests).
#
# Locked API:
#   cli_baseline_file   print the baseline path
#   cli_baseline_ids    print the ids, one per line; returns 2 when the file is
#                       missing/unreadable (both gates fail closed on it)

# Guard against double-sourcing.
[[ -n "${_CLI_BASELINE_SOURCED:-}" ]] && return 0
_CLI_BASELINE_SOURCED=1

cli_baseline_file() {
    printf '%s\n' "${CLI_BASELINE_FILE:-${CLI_ROOT:?CLI_ROOT must be set}/baseline/known_failures.txt}"
}

cli_baseline_ids() {
    local file
    file="$(cli_baseline_file)"
    if [[ ! -r "$file" ]]; then
        echo "ERROR: known-failure baseline not readable: $file" >&2
        return 2
    fi
    # Strip comments and all whitespace, then drop blank lines. Case ids never
    # contain spaces; an accidental inline comment (the documented format)
    # must not turn into part of an id.
    sed -e 's/#.*//' "$file" | awk '{ gsub(/[[:space:]]/, ""); if (length > 0) print }'
}
