#!/bin/bash
# run.sh — entrypoint for the lightstack OpenStack CLI conformance harness (SPEC-028).
#
# Usage:
#   run.sh [--suite <name>]... [--fast] [--strict] [--no-cleanup]
#          [--keep-server] [--results-dir <dir>] [--help]
#
#   --suite <name>     run only the named suite (repeatable). A single
#                      --suite implies --no-cleanup unless --teardown is
#                      also given, so a developer can iterate on one suite.
#   --fast             run the PR-time subset (all suites except workflows).
#   --strict           fail on skipped cases and on any known-failing case that
#                      is not in test/cli/baseline/known_failures.txt. The
#                      nightly runs this way: the PR gate tracks the debt, the
#                      nightly refuses to let it grow. (A baseline entry that
#                      now passes is a stale warning, not a failure.)
#   --no-cleanup       skip the fixture teardown sweep at the end.
#   --teardown         with --suite: still tear down afterwards.
#   --keep-server      do not stop a server this script started itself.
#   --results-dir      where results.json / results.junit.xml go
#                      (default: test/cli/results)
#
# Server selection:
#   1. If $OS_AUTH_URL (default http://localhost:35357/v3) answers, the
#      already-running server is used as-is.
#   2. Otherwise, if docker is available, the binary-test compose stack is
#      started (deployments/docker-compose.binary-test.yml — the binary built
#      by `make build`, bind-mounted over the image copy) and stopped on exit.
#   3. Otherwise: error with instructions.
#
# Exit code: 0 iff every recorded case passed (known-failing and skipped do
# not fail the run; the coverage gate explains the known-failing baseline).
# With --strict, skipped cases and known-failing cases outside the baseline
# fail it.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
REPO_ROOT="$(cd "$CLI_ROOT/../.." && pwd)"

# shellcheck source=../lib/common.sh
source "$CLI_ROOT/lib/common.sh"

SUITES=()
FAST=0
STRICT=0
NO_CLEANUP=0
TEARDOWN=0
KEEP_SERVER=0
RESULTS_DIR="$CLI_ROOT/results"

usage() {
    sed -n '2,34p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit "${1:-0}"
}

# Fail with the usage hint instead of bash's unbound-variable error when an
# option that needs a value is given without one.
_need_arg() {
    if (( $# < 2 )); then
        log_error "option $1 requires a value"
        usage 1
    fi
}

while (( $# > 0 )); do
    case "$1" in
        --suite)       _need_arg "$@"; SUITES+=("$2"); shift 2 ;;
        --fast)        FAST=1; shift ;;
        --strict)      STRICT=1; shift ;;
        --no-cleanup)  NO_CLEANUP=1; shift ;;
        --teardown)    TEARDOWN=1; shift ;;
        --keep-server) KEEP_SERVER=1; shift ;;
        --results-dir) _need_arg "$@"; RESULTS_DIR="$2"; shift 2 ;;
        -h|--help)     usage 0 ;;
        *)             log_error "unknown option: $1"; usage 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
for tool in jq curl python3; do
    command -v "$tool" >/dev/null 2>&1 || {
        log_error "required tool '$tool' not found in PATH"
        exit 2
    }
done
_require_cli || exit 2

# ---------------------------------------------------------------------------
# Server selection
# ---------------------------------------------------------------------------
STARTED_STACK=0

_start_compose_stack() {
    command -v docker >/dev/null 2>&1 || return 1
    docker compose version >/dev/null 2>&1 || return 1

    if [[ ! -x "$REPO_ROOT/bin/lightstack" ]]; then
        log_warn "bin/lightstack not found — building it first (make build)"
        (cd "$REPO_ROOT" && make build) || return 1
    fi

    log_info "starting binary-test compose stack (uses bin/lightstack from this checkout)"
    # -p pins the compose project: without it the project name defaults to the
    # CWD ("lightstack") and `down -v` here would also tear down a developer's
    # running docker-compose.yml stack.
    # STARTED_STACK is set BEFORE `up -d`, not after: a partially-started
    # project (up fails midway) still leaves containers and ports behind, and
    # only a marked stack is torn down by the failed-startup exit path below —
    # which runs before the EXIT trap exists. `down -v` on a project that never
    # came up is a harmless no-op.
    STARTED_STACK=1
    (cd "$REPO_ROOT" && docker compose -p lightstack-binary-test -f deployments/docker-compose.binary-test.yml up -d) || return 1

    log_info "waiting for services to become ready..."
    local deadline=$(( SECONDS + 180 ))
    while (( SECONDS < deadline )); do
        if _server_ready; then
            log_info "all services ready"
            return 0
        fi
        sleep 3
    done
    log_error "services did not become ready within 180s"
    return 1
}

_server_ready() {
    # Derive scheme://host (without the Keystone port) from OS_AUTH_URL so the
    # probe works for non-default hosts too. The stub-mode stack is plain http.
    local scheme="${OS_AUTH_URL%%://*}"
    local host="${OS_AUTH_URL#*://}"
    host="${host%%[:/]*}"
    [[ -n "$host" ]] || host=localhost
    local base="$scheme://$host"
    curl -sf -o /dev/null --max-time 3 "$OS_AUTH_URL" || return 1
    curl -sf -o /dev/null --max-time 3 "$base:8774/v2.1" || return 1
    curl -sf -o /dev/null --max-time 3 "$base:9696/v2.0" || return 1
    curl -sf -o /dev/null --max-time 3 "$base:8776/v3" || return 1
    curl -sf -o /dev/null --max-time 3 "$base:9292/v2" || return 1
    return 0
}

# ---------------------------------------------------------------------------
# Suite selection — BEFORE the server is (possibly) started: the selection is
# pure flag logic, and an empty selection must die without having leased
# anything (a started compose stack would otherwise outlive the exit 2).
# ---------------------------------------------------------------------------
if (( ${#SUITES[@]} == 0 )); then
    # Every suite in suites/, sorted for deterministic order.
    for f in "$CLI_ROOT"/suites/*.sh; do
        SUITES+=("$(basename "$f" .sh)")
    done
fi
if (( FAST )); then
    filtered=()
    for s in "${SUITES[@]}"; do
        [[ "$s" == "workflows" ]] && continue
        filtered+=("$s")
    done
    SUITES=("${filtered[@]}")
fi

# A selection that matches nothing must fail loudly, not run zero suites and
# exit 0 — the exact vacuous-green class this harness exists to prevent
# (e.g. `--fast --suite workflows`: the fast pool excludes workflows). The
# selection is necessarily empty here, so there is nothing to interpolate.
if (( ${#SUITES[@]} == 0 )); then
    log_error "the --suite selection matches no suite under the current flags (--fast excludes workflows)"
    exit 2
fi

# Strict mode is read by report_failed (lib/report.sh): known-failing and
# skipped cases fail the run as well.
if (( STRICT )); then
    export CLI_STRICT=1
fi

# A single --suite implies no teardown unless explicitly requested.
if (( ${#SUITES[@]} == 1 )) && (( ! TEARDOWN )) && (( ! NO_CLEANUP )); then
    NO_CLEANUP=1
fi

if ! curl -sf -o /dev/null --max-time 5 "$OS_AUTH_URL"; then
    if ! _start_compose_stack; then
        # The EXIT trap is not installed yet — anything the failed start left
        # behind must be torn down HERE, or a half-started stack (up succeeded,
        # readiness never came) would outlive the exit 2.
        if (( STARTED_STACK )) && (( ! KEEP_SERVER )); then
            (cd "$REPO_ROOT" && docker compose -p lightstack-binary-test -f deployments/docker-compose.binary-test.yml down -v) >/dev/null 2>&1 || true
        fi
        cat >&2 <<EOF
[cli] ERROR: no lightstack server at $OS_AUTH_URL, and starting one failed
(docker missing, build failed, or the stack never became ready — see above).

Either start a server yourself:
    make build && make run
or via docker compose:
    docker compose -f deployments/docker-compose.yml up -d
or point OS_AUTH_URL at a running server.
EOF
        exit 2
    fi
else
    log_info "using external server at $OS_AUTH_URL"
fi

# ---------------------------------------------------------------------------
# Run
# ---------------------------------------------------------------------------
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cli-conformance.XXXXXX")"
export RESULTS_FILE="$RUN_DIR/cases.jsonl"
: >"$RESULTS_FILE"
export CLI_RUNNER_ACTIVE=1

# shellcheck source=../lib/fixtures.sh
source "$CLI_ROOT/lib/fixtures.sh"

cleanup() {
    # failed is declared HERE, before report_write's check below — a later
    # `local failed=0` would shadow an earlier assignment made while the
    # variable was still global, silently disabling the check (round 9).
    local rc=$? failed=0
    trap - EXIT INT TERM
    if (( ! NO_CLEANUP )); then
        fixtures_teardown || true
    fi
    if (( STARTED_STACK )) && (( ! KEEP_SERVER )); then
        (cd "$REPO_ROOT" && docker compose -p lightstack-binary-test -f deployments/docker-compose.binary-test.yml down -v) >/dev/null 2>&1 || true
    fi
    report_write "$RESULTS_DIR" || failed=1
    report_summary
    # report_failed must read the results BEFORE the run dir is removed —
    # it used to run after `rm -rf` and always saw a missing file, which
    # the old "missing means clean" reading silently absorbed (failure
    # detection at exit was dead code). Now it genuinely gates the exit.
    if (( rc == 0 )) && report_failed; then
        failed=1
    fi
    rm -rf "$RUN_DIR"
    if (( failed )); then
        exit 1
    fi
    exit "$rc"
}
# INT/TERM re-exit with the conventional 128+signal status so the EXIT trap
# (which preserves and propagates it) runs once, with the right code — a trap
# on the signal itself would run cleanup with whatever $? happened to be,
# which can flatten a killed run into a successful exit.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fixtures_ensure || {
    log_error "fixture setup failed — cannot run suites"
    exit 2
}

FAILED_SUITES=0
for suite in "${SUITES[@]}"; do
    suite_file="$CLI_ROOT/suites/$suite.sh"
    if [[ ! -f "$suite_file" ]]; then
        log_error "suite not found: $suite (looked for $suite_file)"
        FAILED_SUITES=1
        continue
    fi
    log_info "running suite: $suite"
    if ! bash "$suite_file"; then
        log_error "suite $suite exited non-zero"
        FAILED_SUITES=1
    fi
done

exit $FAILED_SUITES
