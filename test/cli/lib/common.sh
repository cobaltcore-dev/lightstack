#!/bin/bash
# common.sh — shared foundation for the lightstack CLI conformance harness (SPEC-028).
#
# Every suite sources exactly this file; it in turn sources assert.sh and report.sh.
# The locked API (suites depend on these names — do not rename):
#
#   osc <args...>            run the OpenStack CLI (always -f json unless caller
#                            passed -f). Sets OSC_RC, OSC_OUT, OSC_ERR, LAST_CMD,
#                            LAST_CMD_MS. Never fails the case by itself.
#   t_begin <case-id>        open a test case (id must be unique, e.g. keystone.project.list)
#   t_fail <message>         mark the current case failed with a reason (repeatable)
#   t_known_failure <msg>    mark the current case known-failing (tracked, not fatal)
#   t_skip <message>         mark the current case skipped
#   t_end                    close the current case and record it
#   suite_begin <name> / suite_end
#   log_info / log_warn / log_error <msg>
#
# Environment (all optional):
#   OS_AUTH_URL, OS_USERNAME, OS_PASSWORD, OS_PROJECT_NAME,
#   OS_USER_DOMAIN_NAME, OS_PROJECT_DOMAIN_NAME   — standard OpenStack client vars
#   OPENSTACK_CLI   — path to the openstack binary (default: whatever is in PATH)
#   OSC_TIMEOUT     — per-command timeout in seconds (default 30)
#   RESULTS_FILE    — JSONL sink for case records (set by bin/run.sh; standalone
#                     suite runs get one automatically)

# Guard against double-sourcing.
[[ -n "${_CLI_COMMON_SOURCED:-}" ]] && return 0
_CLI_COMMON_SOURCED=1

CLI_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_ROOT="$(cd "$CLI_LIB_DIR/.." && pwd)"
# shellcheck source=assert.sh
source "$CLI_LIB_DIR/assert.sh"
# shellcheck source=report.sh
source "$CLI_LIB_DIR/report.sh"

# ---------------------------------------------------------------------------
# Standalone mode: when a suite is run directly (bash suites/foo.sh) instead
# of via bin/run.sh, record results to results/standalone/ and exit non-zero
# if any case failed. run.sh sets CLI_RUNNER_ACTIVE=1 and owns reporting.
# Takes the exit status to preserve as $1 — the EXIT trap runs other commands
# before this, which would otherwise overwrite $?.
# ---------------------------------------------------------------------------
_standalone_exit() {
    local rc="${1:-$?}"
    trap - EXIT
    if [[ "${CLI_RUNNER_ACTIVE:-0}" != "1" ]] && (( ${_CASES_RECORDED:-0} )); then
        report_write "$CLI_ROOT/results/standalone"
        report_summary
        if report_failed; then
            rc=1
        fi
    fi
    exit "$rc"
}

# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------

# A clouds.yaml selection would silently override every OS_* variable below and
# point the suite at the wrong cloud. This harness always talks to the server
# named by OS_AUTH_URL.
unset OS_CLOUD 2>/dev/null || true

: "${OS_AUTH_URL:=http://localhost:35357/v3}"
: "${OS_USERNAME:=admin}"
: "${OS_PASSWORD:=secret}"
: "${OS_PROJECT_NAME:=default}"
: "${OS_USER_DOMAIN_NAME:=Default}"
: "${OS_PROJECT_DOMAIN_NAME:=Default}"
export OS_AUTH_URL OS_USERNAME OS_PASSWORD OS_PROJECT_NAME \
    OS_USER_DOMAIN_NAME OS_PROJECT_DOMAIN_NAME
export OS_IDENTITY_API_VERSION=3

: "${OSC_TIMEOUT:=30}"
export OSC_TIMEOUT

# Locate the CLI once, lazily.
_cli_find_binary() {
    if [[ -n "${OPENSTACK_CLI:-}" ]]; then
        command -v "$OPENSTACK_CLI" >/dev/null 2>&1 || return 1
        printf '%s\n' "$OPENSTACK_CLI"
        return 0
    fi
    command -v openstack 2>/dev/null || return 1
}

_require_cli() {
    if [[ -n "${OSC_BIN:-}" ]]; then
        return 0
    fi
    if ! OSC_BIN="$(_cli_find_binary)"; then
        cat >&2 <<'EOF'
[cli] ERROR: the OpenStack CLI (python-openstackclient) is not installed.

Install the pinned version used by this harness:

    pip install -c test/cli/constraints.txt python-openstackclient

or point OPENSTACK_CLI at an existing binary.
EOF
        return 1
    fi
    export OSC_BIN
}

# Monotonic-ish wall clock in ms. Bash 5's EPOCHREALTIME avoids a fork; the
# python3 fallback keeps macOS/dev boxes working (python3 is guaranteed here —
# the CLI itself needs it).
_now_ms() {
    if [[ -n "${EPOCHREALTIME:-}" ]]; then
        local s frac
        s=${EPOCHREALTIME%.*}
        frac=${EPOCHREALTIME#*.}
        printf '%s%03d\n' "$s" "$((10#${frac:0:3}))"
    else
        python3 -c 'import time; print(int(time.time() * 1000))'
    fi
}

# ---------------------------------------------------------------------------
# Logging (stderr only — stdout stays clean for summaries)
# ---------------------------------------------------------------------------

_use_color() { [[ -t 2 && "${NO_COLOR:-}" == "" ]]; }
log_info()  { if _use_color; then printf '\033[1;34m[cli]\033[0m %s\n' "$*" >&2; else printf '[cli] %s\n' "$*" >&2; fi; }
log_warn()  { if _use_color; then printf '\033[1;33m[cli] WARN\033[0m %s\n' "$*" >&2; else printf '[cli] WARN %s\n' "$*" >&2; fi; }
log_error() { if _use_color; then printf '\033[0;31m[cli] ERROR\033[0m %s\n' "$*" >&2; else printf '[cli] ERROR %s\n' "$*" >&2; fi; }

# ---------------------------------------------------------------------------
# The one way to invoke the CLI
# ---------------------------------------------------------------------------

# Reusable stderr capture file (suites run sequentially).
_CLI_ERR_FILE="${TMPDIR:-/tmp}/lightstack-cli-err.$$"
_CLI_ERR_FILE_CREATED=0

# Inner executor: runs the CLI with exactly the given args.
_osc_exec() {
    local args=("$@")
    [[ $_CLI_ERR_FILE_CREATED -eq 0 ]] && : >"$_CLI_ERR_FILE" && _CLI_ERR_FILE_CREATED=1

    local t0 t1
    t0=$(_now_ms)
    # OSC_OUT/OSC_RC/OSC_ERR/LAST_CMD* are the harness's cross-file contract:
    # every suite sources this library and reads them after each osc() call,
    # which shellcheck cannot see from this file alone.
    # shellcheck disable=SC2034
    OSC_OUT="$(timeout -k 10 "$OSC_TIMEOUT" "$OSC_BIN" "${args[@]}" 2>"$_CLI_ERR_FILE")"
    OSC_RC=$?
    t1=$(_now_ms)
    OSC_ERR="$(<"$_CLI_ERR_FILE")"
    LAST_CMD="$OSC_BIN ${args[*]}"
    # shellcheck disable=SC2034
    LAST_CMD_MS=$(( t1 - t0 ))
}

osc() {
    _require_cli || return 1

    local args=("$@") has_format=0 a
    for a in "$@"; do
        [[ "$a" == "-f" || "$a" == "--format" ]] && has_format=1
    done
    # Machine-readable output unless the caller explicitly asked otherwise
    # (e.g. -f value, -f csv, --help).
    (( has_format )) || args+=(-f json)

    _osc_exec "${args[@]}"

    # Mutation commands with no formatter (set/delete/associate/...) reject
    # -f json with argparse exit code 2. Retry once without the flag — this
    # keeps suite code uniform and survives CLI version differences.
    if (( OSC_RC == 2 )) && printf '%s' "$OSC_ERR" | grep -q -- "-f json"; then
        _osc_exec "$@"
    fi

    if (( OSC_RC == 124 )); then
        log_warn "command timed out after ${OSC_TIMEOUT}s: $LAST_CMD"
    elif (( OSC_RC != 0 )) && [[ -z "${CURRENT_CASE:-}" ]] && [[ "${OSC_QUIET:-0}" != "1" ]]; then
        # A failing call outside any case is worth surfacing, but the caller
        # decides whether it is a failure (negative tests expect rc != 0).
        # OSC_QUIET=1 lets existence probes and best-effort sweeps silence
        # the expected 404s.
        log_warn "non-zero exit (${OSC_RC}) outside a case: $LAST_CMD"
    fi
    return 0
}

_cli_cleanup() {
    # Capture the exit status BEFORE any other command — rm would otherwise
    # reset it, flattening every early failure (unknown option, preflight
    # errors) to a successful exit.
    local rc=$?
    rm -f "$_CLI_ERR_FILE"
    _standalone_exit "$rc"
}
# The single EXIT trap: always removes the stderr capture file, and in
# standalone mode (not under bin/run.sh) also writes results and converts
# case failures into a non-zero exit code.
trap _cli_cleanup EXIT

# ---------------------------------------------------------------------------
# Case lifecycle
# ---------------------------------------------------------------------------

CURRENT_CASE=""
CASE_FAILED=0
CASE_KNOWN=0
CASE_SKIPPED=0
CASE_START=0
CASE_ERRORS=()

t_begin() {
    if [[ -n "$CURRENT_CASE" ]]; then
        # Defensive: a missing t_end would silently lose the case.
        log_warn "t_begin called while case '$CURRENT_CASE' is open — auto-closing it"
        t_end
    fi
    CURRENT_CASE="$1"
    CASE_FAILED=0
    CASE_KNOWN=0
    CASE_SKIPPED=0
    CASE_START=$(_now_ms)
}

t_fail() {
    if [[ -z "${CURRENT_CASE:-}" ]]; then
        log_error "t_fail called outside a case: $*"
        return
    fi
    CASE_FAILED=1
    CASE_ERRORS+=("$*")
}

t_known_failure() {
    if [[ -z "${CURRENT_CASE:-}" ]]; then
        log_error "t_known_failure called outside a case: $*"
        return
    fi
    CASE_KNOWN=1
    CASE_ERRORS+=("known failure: $*")
}

t_skip() {
    if [[ -z "${CURRENT_CASE:-}" ]]; then
        log_error "t_skip called outside a case: $*"
        return
    fi
    CASE_SKIPPED=1
    CASE_ERRORS+=("skipped: $*")
}

t_end() {
    [[ -z "$CURRENT_CASE" ]] && return 0
    local dur=$(( $(_now_ms) - CASE_START ))
    local status=pass
    (( CASE_SKIPPED )) && status=skip
    (( CASE_KNOWN )) && status=known-failing
    (( CASE_FAILED )) && status=fail

    local detail
    detail="$(printf '%s; ' "${CASE_ERRORS[@]}")"

    _record_case "${SUITE_NAME:-unknown}" "$CURRENT_CASE" "$status" "$dur" \
        "${LAST_CMD:-}" "${detail%; }"

    if [[ "$status" == pass ]]; then
        if _use_color; then printf '  \033[0;32m✓\033[0m %s (%dms)\n' "$CURRENT_CASE" "$dur" >&2; else printf '  ok %s (%dms)\n' "$CURRENT_CASE" "$dur" >&2; fi
    elif [[ "$status" == fail ]]; then
        if _use_color; then printf '  \033[0;31m✗ FAIL\033[0m %s (%dms): %s\n' "$CURRENT_CASE" "$dur" "${detail%; }" >&2; else printf '  FAIL %s (%dms): %s\n' "$CURRENT_CASE" "$dur" "${detail%; }" >&2; fi
    elif [[ "$status" == known-failing ]]; then
        if _use_color; then printf '  \033[1;33m○ known-failing\033[0m %s: %s\n' "$CURRENT_CASE" "${detail%; }" >&2; else printf '  known-failing %s: %s\n' "$CURRENT_CASE" "${detail%; }" >&2; fi
    else
        printf '  - skip %s: %s\n' "$CURRENT_CASE" "${detail%; }" >&2
    fi

    CURRENT_CASE=""
    CASE_ERRORS=()
}

suite_begin() {
    SUITE_NAME="$1"
    printf '\n== suite: %s ==\n' "$SUITE_NAME" >&2
}

suite_end() {
    t_end # close any dangling case
    printf '== end suite: %s ==\n' "${SUITE_NAME:-?}" >&2
}
