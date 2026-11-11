#!/bin/bash
#
# The KVM Compose stack runs either database backend from one file (SPEC-028)
#
# libvirtd on the host opens the paths Lightstack writes into each domain's XML — disks,
# config-drive ISOs, console sockets — so the stack mounts the node-local state root at the
# same path inside the container as outside. A SQLite database lives in that root too
# (<state_path>/db), so mounting the whole root, rather than a list of subdirectories, is
# what lets the database URL alone choose the backend. The root and the config directory
# are overridable, so a second installation on the host can keep its own; the database URL
# passes through to the binary, and empty keeps the config's.
#
# This test renders the stack with `compose config` and checks the mounts and the URL, once
# with nothing set and once with every override.
#
# Usage: ./test/compose_kvm_state_test.sh
#

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

STACK=deployments/docker-compose-kvm.yml

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
# Plain assignment, not ((x++)): that returns 1 while the counter is still 0.
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; TESTS_PASSED=$((TESTS_PASSED + 1)); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; TESTS_FAILED=$((TESTS_FAILED + 1)); }

if ! docker compose version >/dev/null 2>&1; then
    echo -e "${RED}[FAIL]${NC} 'docker compose' is required to render the stack"
    exit 1
fi
if ! python3 -c "import yaml" >/dev/null 2>&1; then
    echo -e "${RED}[FAIL]${NC} python3 with PyYAML is required to read the rendered stack"
    exit 1
fi

# Report the lightstack service's mounts as "MOUNT=<source>|<target>|<ro|rw>" lines, plus its
# database URL and command. Compose v1 renders volumes as strings, v2 as mappings.
read_stack() {
    python3 -c '
import sys, yaml

doc = yaml.safe_load(sys.stdin) or {}
svc = (doc.get("services") or {}).get("lightstack") or {}

for v in svc.get("volumes") or []:
    if isinstance(v, str):
        parts = v.split(":")
        src, dst = parts[0], parts[1] if len(parts) > 1 else parts[0]
        ro = len(parts) > 2 and "ro" in parts[2].split(",")
    else:
        src, dst, ro = v.get("source", ""), v.get("target", ""), bool(v.get("read_only"))
    print("MOUNT=%s|%s|%s" % (src, dst, "ro" if ro else "rw"))

env = svc.get("environment") or {}
if isinstance(env, list):                          # "- KEY=value" form
    env = dict(str(item).partition("=")[::2] for item in env)
url = env.get("LIGHTSTACK_DB_URL")
print("DBURL=%s" % ("-" if url is None else url))

cmd = svc.get("command") or []
print("COMMAND=%s" % (" ".join(cmd) if isinstance(cmd, list) else cmd))
'
}

# render [VAR=value ...]: the stack as Compose sees it with exactly these variables set, so a
# value left in the caller's shell cannot leak into the defaults.
render() {
    env -u LIGHTSTACK_STATE_PATH -u LIGHTSTACK_CONFIG_DIR -u LIGHTSTACK_DB_URL "$@" \
        timeout 120 docker compose -f "$STACK" config 2>/dev/null
}

mounts() { echo "$1" | sed -n 's/^MOUNT=//p' | sort; }
field() { echo "$1" | sed -n "s/^$2=//p"; }

# --- nothing set: today's paths, the config's database URL ------------------------
test_defaults() {
    log_test "defaults: $STACK"
    local out stack want
    out="$(render)"
    if [ -z "$out" ]; then
        log_fail "$STACK did not render"; return
    fi
    stack="$(echo "$out" | read_stack)"

    want="$(printf '%s\n' \
        '/opt/lightstack/config|/opt/lightstack/config|ro' \
        '/var/lib/lightstack|/var/lib/lightstack|rw' \
        '/var/run/libvirt|/var/run/libvirt|rw' | sort)"
    if [ "$(mounts "$stack")" != "$want" ]; then
        log_fail "default mounts are:"$'\n'"$(mounts "$stack")"$'\n'"expected:"$'\n'"$want"; return
    fi
    case "$(field "$stack" DBURL)" in
        ""|-) ;;
        *) log_fail "default LIGHTSTACK_DB_URL is '$(field "$stack" DBURL)', expected empty (the config's URL)"; return ;;
    esac
    if [ "$(field "$stack" COMMAND)" != "/app/lightstack -config /opt/lightstack/config/lightstack.yaml" ]; then
        log_fail "command is '$(field "$stack" COMMAND)'"; return
    fi
    log_pass "the state root and the config mount at today's paths; the config's URL is used"
}

# --- every override reaches the mounts and the binary -------------------------------
test_overrides() {
    log_test "overrides: $STACK"
    local out stack want url dbdir covered line src dst
    url="sqlite:///srv/ls-kvm-test/state/db/lightstack.db"
    out="$(render LIGHTSTACK_STATE_PATH=/srv/ls-kvm-test/state LIGHTSTACK_CONFIG_DIR=/srv/ls-kvm-test/config \
        LIGHTSTACK_DB_URL="$url")"
    if [ -z "$out" ]; then
        log_fail "$STACK did not render with the overrides"; return
    fi
    stack="$(echo "$out" | read_stack)"

    want="$(printf '%s\n' \
        '/srv/ls-kvm-test/config|/opt/lightstack/config|ro' \
        '/srv/ls-kvm-test/state|/srv/ls-kvm-test/state|rw' \
        '/var/run/libvirt|/var/run/libvirt|rw' | sort)"
    if [ "$(mounts "$stack")" != "$want" ]; then
        log_fail "overridden mounts are:"$'\n'"$(mounts "$stack")"$'\n'"expected:"$'\n'"$want"; return
    fi
    if [ "$(field "$stack" DBURL)" != "$url" ]; then
        log_fail "LIGHTSTACK_DB_URL is '$(field "$stack" DBURL)', expected '$url'"; return
    fi

    # The database's directory must lie on the host, at the path the URL names.
    dbdir="/srv/ls-kvm-test/state/db"
    covered=""
    while IFS='|' read -r src dst _; do
        if [ "$src" = "$dst" ] && { [ "$dbdir" = "$dst" ] || [ "${dbdir#"$dst"/}" != "$dbdir" ]; }; then
            covered="$dst"
        fi
    done <<< "$(mounts "$stack")"
    if [ -z "$covered" ]; then
        log_fail "no same-path mount holds $dbdir: the SQLite database would live in the container"; return
    fi
    log_pass "the state root, the config directory and the database URL follow the overrides"
}

main() {
    echo ""
    echo "KVM Compose stack: state root and database URL"
    echo ""

    test_defaults
    test_overrides

    echo ""
    echo -e "${GREEN}Tests Passed: $TESTS_PASSED${NC}"
    echo -e "${RED}Tests Failed: $TESTS_FAILED${NC}"
    echo ""
    [ "$TESTS_FAILED" -eq 0 ]
}

main "$@"
