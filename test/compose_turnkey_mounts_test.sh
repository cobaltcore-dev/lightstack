#!/bin/bash
#
# The turnkey Compose stack mounts what the KVM one does for real networking (SPEC-033 T246)
#
# scripts/deploy-turnkey.sh writes deployments/docker-compose-turnkey.yml and runs Lightstack in it
# with networking_mode iptables. The router and DHCP namespaces Lightstack makes must be the host's,
# so they and the traffic through them outlive the container: the host's /run/netns is mounted shared
# both ways (FR-036), and the script creates it first, since Podman refuses a missing bind source.
# /lib/modules is mounted read-only for `modprobe br_netfilter` (FR-046).
#
# This test renders the script's Compose heredoc with placeholder paths and reads the lightstack
# service's mounts; nothing is started and no container runtime is needed.
#
# Usage: ./test/compose_turnkey_mounts_test.sh
#

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="${TURNKEY_SCRIPT:-$REPO_ROOT/scripts/deploy-turnkey.sh}"

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; TESTS_PASSED=$((TESTS_PASSED + 1)); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; TESTS_FAILED=$((TESTS_FAILED + 1)); }

if ! python3 -c "import yaml" >/dev/null 2>&1; then
    echo -e "${RED}[FAIL]${NC} python3 with PyYAML is required to read the rendered stack"
    exit 1
fi

# The Compose file the script writes, its variables set to placeholders.
render() {
    local body
    body="$(sed -n '/cat > "$REPO_DIR\/deployments\/docker-compose-turnkey.yml" <<EOF/,/^EOF$/p' "$SCRIPT" | sed '1d;$d')"
    [ -n "$body" ] || return 1
    STORAGE_DIR=/srv/ls-turnkey/state CONFIG_DIR=/srv/ls-turnkey/config HORIZON_SECRET_KEY=x \
        bash -c "cat <<EOF
$body
EOF"
}

test_host_mounts() {
    log_test "the lightstack service mounts the host's namespaces both ways and its modules read-only"
    local out volumes missing=""
    if ! out="$(render)"; then
        log_fail "could not read the Compose heredoc out of $SCRIPT"; return
    fi
    if ! volumes="$(echo "$out" | python3 -c '
import sys, yaml
svc = (yaml.safe_load(sys.stdin).get("services") or {}).get("lightstack") or {}
print("\n".join(str(v) for v in svc.get("volumes") or []))')"; then
        log_fail "the rendered Compose file is not YAML"; return
    fi
    for want in /run/netns:/run/netns:rshared /lib/modules:/lib/modules:ro; do
        grep -qxF "$want" <<<"$volumes" || missing="$missing $want"
    done
    if [ -n "$missing" ]; then
        log_fail "missing:$missing"$'\n'"the lightstack service mounts:"$'\n'"$volumes"; return
    fi
    log_pass "/run/netns shared both ways, /lib/modules read-only"
}

test_netns_created_first() {
    log_test "the script creates /run/netns before it starts the stack"
    local mkdir_line up_line
    mkdir_line="$(grep -n 'mkdir -p /run/netns' "$SCRIPT" | head -1 | cut -d: -f1)"
    up_line="$(grep -n 'docker compose -f docker-compose-turnkey.yml up' "$SCRIPT" | head -1 | cut -d: -f1)"
    if [ -z "$up_line" ]; then
        log_fail "could not find the stack's start in $SCRIPT"; return
    fi
    if [ -z "$mkdir_line" ] || [ "$mkdir_line" -gt "$up_line" ]; then
        log_fail "/run/netns is not created before line $up_line starts the stack"; return
    fi
    log_pass "created at line $mkdir_line, before the start at line $up_line"
}

test_host_mounts
test_netns_created_first

echo ""
echo -e "${GREEN}Passed: $TESTS_PASSED${NC}  ${RED}Failed: $TESTS_FAILED${NC}"
[ "$TESTS_FAILED" -eq 0 ]
