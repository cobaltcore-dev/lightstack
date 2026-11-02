#!/bin/bash
# Lightstack Horizon Compatibility Test (Bash version)
# Simulates Horizon dashboard API calls

set -euo pipefail

# Cross-function state, pre-declared so `set -u` cannot abort a later group when an
# earlier one did not reach its assignment.
TOKEN=""
PROJECT_ID=""
IMAGE_ID=""
FLAVOR_ID=""
NETWORK_ID=""
SERVER_ID=""
resp=""
delete_code=""

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

PASSED=0
FAILED=0

LIGHTSTACK_URL="http://localhost:35357"
ADMIN_USER="admin"
ADMIN_PASSWORD="${OS_PASSWORD:-${LIGHTSTACK_ADMIN_PASSWORD:-secret}}"
PROJECT_NAME="default"

print_test() {
    echo -e "\n${YELLOW}[TEST]${NC} $1"
}

# The counters use `VAR=$((VAR + 1))`, not `((VAR++))`, on purpose.
#
# `((VAR++))` evaluates to the value *before* the increment, and bash reports an
# arithmetic expression evaluating to 0 as exit status 1. Combined with `set -e` above,
# that made the FIRST failure fatal: print_fail is the last command of the
# `... && print_pass || print_fail` chains below, so its status became the chain's, and
# the script died before reaching any later check. Passes were immune only because
# `&&`/`||` suppresses `set -e` for their left-hand elements.
#
# The effect was a suite that could never report more than one failure and whose
# summary was therefore unreliable — the same class of defect as the suites repaired in
# the cloud-storage#534 review. Fixed here because SPEC-022 SC-008 requires this suite's
# verdict to be meaningful (cloud-storage#545).
print_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
    PASSED=$((PASSED + 1))
}

print_fail() {
    echo -e "${RED}[FAIL]${NC} $1" >&2
    FAILED=$((FAILED + 1))
}

print_info() {
    echo -e "  ℹ  $1"
}

# Assert a response is JSON and carries an expected key. A substring test on the key name passes
# on any body that merely contains the word — an HTML error page, or a 502 from something proxying
# in front of the service — so it cannot tell a real payload from unrelated text. jq -e rejects
# both halves: a missing key yields null, and unparseable input is an error.
json_has() {
    printf '%s' "${1:-}" | jq -e "${2:-}" >/dev/null 2>&1
}

# json_has plus pass/fail reporting, for the checks that only need a verdict.
check_json_key() {
    local body="${1:-}" filter="${2:-}" label="${3:-}"
    if json_has "$body" "$filter"; then
        print_pass "$label"
    else
        print_fail "$label"
    fi
}

# Test 1: Horizon Login Flow
test_login() {
    print_test "Horizon Login Flow"

    # Get scoped token
    response=$(curl -s -i -X POST "${LIGHTSTACK_URL}/v3/auth/tokens" \
        -H "Content-Type: application/json" \
        -d '{
            "auth": {
                "identity": {
                    "methods": ["password"],
                    "password": {
                        "user": {
                            "name": "'$ADMIN_USER'",
                            "domain": {"name": "Default"},
                            "password": "'$ADMIN_PASSWORD'"
                        }
                    }
                },
                "scope": {
                    "project": {
                        "name": "'$PROJECT_NAME'",
                        "domain": {"name": "Default"}
                    }
                }
            }
        }')

    TOKEN=$(echo "$response" | grep -i "X-Subject-Token:" | awk '{print $2}' | tr -d '\r' || true)

    if [ -n "$TOKEN" ]; then
        print_pass "Authenticated successfully"
        print_info "Token: ${TOKEN:0:50}..."

        # Check for service catalog
        # $response comes from `curl -i`, so strip the headers before parsing JSON.
        token_body=$(printf '%s' "$response" | sed -n '/^{/,$p')
        if printf '%s' "$token_body" | jq -e '.token.catalog | length > 0' >/dev/null 2>&1; then
            print_pass "Service catalog present"

            service_count=$(printf '%s' "$token_body" | jq '.token.catalog | length')
            print_info "Found $service_count services in catalog"
        else
            print_fail "Service catalog missing or empty"
            return 1
        fi
    else
        print_fail "Authentication failed"
        return 1
    fi

    export TOKEN
}

# Test 2: Project Dashboard
test_dashboard() {
    print_test "Project Dashboard Load"

    # Horizon loads these endpoints on dashboard
    resp=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/servers")
    check_json_key "$resp" '.servers' "Nova - List servers"

    resp=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/flavors")
    check_json_key "$resp" '.flavors' "Nova - List flavors"

    resp=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/networks")
    check_json_key "$resp" '.networks' "Neutron - List networks"

    # Get project ID
    PROJECT_ID=$(curl -s -X POST "${LIGHTSTACK_URL}/v3/auth/tokens" \
        -H "Content-Type: application/json" \
        -d '{
            "auth": {
                "identity": {
                    "methods": ["password"],
                    "password": {
                        "user": {
                            "name": "'$ADMIN_USER'",
                            "domain": {"name": "Default"},
                            "password": "'$ADMIN_PASSWORD'"
                        }
                    }
                },
                "scope": {
                    "project": {
                        "name": "'$PROJECT_NAME'",
                        "domain": {"name": "Default"}
                    }
                }
            }
        }' | jq -r '.token.project.id')

    export PROJECT_ID

    resp=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/${PROJECT_ID}/volumes")
    check_json_key "$resp" '.volumes' "Cinder - List volumes"

    resp=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9292/v2/images")
    check_json_key "$resp" '.images' "Glance - List images"
}

# Test 3: Instances Tab
test_instances_tab() {
    print_test "Instances Tab (Nova)"

    # List servers (detailed)
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/servers/detail")

    if json_has "$response" '.servers'; then
        server_count=$(echo "$response" | jq '.servers | length')
        print_pass "Listed $server_count servers"
    else
        print_fail "Failed to list servers"
    fi

    # Get hypervisor statistics
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/os-hypervisors/statistics")

    if json_has "$response" '.hypervisor_statistics'; then
        print_pass "Hypervisor statistics retrieved"

        vcpus=$(echo "$response" | jq -r '.hypervisor_statistics.vcpus')
        vcpus_used=$(echo "$response" | jq -r '.hypervisor_statistics.vcpus_used')
        memory_mb=$(echo "$response" | jq -r '.hypervisor_statistics.memory_mb')
        memory_mb_used=$(echo "$response" | jq -r '.hypervisor_statistics.memory_mb_used')

        print_info "vCPUs: $vcpus total, $vcpus_used used"
        print_info "Memory: $memory_mb MB total, $memory_mb_used MB used"
    else
        print_fail "Failed to get hypervisor statistics"
    fi
}

# Test 4: Networks Tab
test_networks_tab() {
    print_test "Networks Tab (Neutron)"

    # List networks
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/networks")

    if json_has "$response" '.networks'; then
        network_count=$(echo "$response" | jq '.networks | length')
        print_pass "Listed $network_count networks"
    else
        print_fail "Failed to list networks"
    fi

    # List subnets
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/subnets")

    if json_has "$response" '.subnets'; then
        subnet_count=$(echo "$response" | jq '.subnets | length')
        print_pass "Listed $subnet_count subnets"
    else
        print_fail "Failed to list subnets"
    fi

    # List routers
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/routers")

    if json_has "$response" '.routers'; then
        router_count=$(echo "$response" | jq '.routers | length')
        print_pass "Listed $router_count routers"
    else
        print_fail "Failed to list routers"
    fi
}

# Test 5: Volumes Tab
test_volumes_tab() {
    print_test "Volumes Tab (Cinder)"

    # List volumes
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/${PROJECT_ID}/volumes/detail")

    if json_has "$response" '.volumes'; then
        volume_count=$(echo "$response" | jq '.volumes | length')
        print_pass "Listed $volume_count volumes"
    else
        print_fail "Failed to list volumes"
    fi

    # List volume types
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8776/v3/${PROJECT_ID}/types")

    if json_has "$response" '.volume_types'; then
        type_count=$(echo "$response" | jq '.volume_types | length')
        print_pass "Listed $type_count volume types"
    else
        print_fail "Failed to list volume types"
    fi
}

# Test 6: Images Tab
test_images_tab() {
    print_test "Images Tab (Glance)"

    # List images
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9292/v2/images")

    if json_has "$response" '.images'; then
        image_count=$(echo "$response" | jq '.images | length')
        print_pass "Listed $image_count images"

        # Show image details
        echo "$response" | jq -r '.images[] | "  - \(.name // "Unnamed") (\(.status // "unknown"))"'
    else
        print_fail "Failed to list images"
    fi
}

# Test 7: Launch Instance Workflow
test_launch_instance() {
    print_test "Launch Instance Workflow"

    # Get flavors
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:8774/v2.1/flavors/detail")

    if json_has "$response" '.flavors'; then
        # `.flavors` is truthy for an empty list too, so index 0 has to be checked separately --
        # otherwise FLAVOR_ID becomes the string "null" and the launch below fails as a bogus
        # Nova error instead of a missing fixture.
        FLAVOR_ID=$(printf '%s' "$response" | jq -r '.flavors[0].id // empty')
        if [ -z "$FLAVOR_ID" ]; then
            print_fail "No flavors available - launch flow NOT validated"
            return 1
        fi

        flavor_count=$(echo "$response" | jq '.flavors | length')
        print_pass "Retrieved $flavor_count flavors"

        flavor_name=$(echo "$response" | jq -r '.flavors[0].name')
        print_info "Using flavor: $flavor_name ($FLAVOR_ID)"
    else
        print_fail "Failed to get flavors"
        return 1
    fi

    # Get images
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9292/v2/images")

    if json_has "$response" '.images'; then
        image_count=$(echo "$response" | jq '.images | length')
        print_pass "Retrieved $image_count images"

        # Filter active images
        IMAGE_ID=$(printf '%s' "$response" | jq -r 'first(.images[] | select(.status == "active") | .id) // empty')

        if [ -z "$IMAGE_ID" ]; then
            print_fail "No active images available - Glance may be broken; launch flow NOT validated"
            return 1
        fi

        image_name=$(echo "$response" | jq -r ".images[] | select(.id == \"$IMAGE_ID\") | .name")
        print_info "Using image: $image_name ($IMAGE_ID)"
    else
        print_fail "Failed to get images"
        return 1
    fi

    # Get networks
    response=$(curl -s -H "X-Auth-Token: $TOKEN" "http://localhost:9696/v2.0/networks")

    if json_has "$response" '.networks'; then
        NETWORK_ID=$(printf '%s' "$response" | jq -r '.networks[0].id // empty')
        if [ -z "$NETWORK_ID" ]; then
            print_fail "No networks available - launch flow NOT validated"
            return 1
        fi

        network_name=$(echo "$response" | jq -r '.networks[0].name')
        print_pass "Retrieved networks"
        print_info "Using network: $network_name ($NETWORK_ID)"
    else
        print_fail "Failed to get networks"
        return 1
    fi

    # Create server
    server_name="horizon-test-vm-$(date +%Y%m%d%H%M%S)"
    response=$(curl -s -w "\n%{http_code}" -X POST -H "X-Auth-Token: $TOKEN" \
        -H "Content-Type: application/json" \
        "http://localhost:8774/v2.1/servers" \
        -d '{
            "server": {
                "name": "'$server_name'",
                "flavorRef": "'$FLAVOR_ID'",
                "imageRef": "'$IMAGE_ID'",
                "networks": [{"uuid": "'$NETWORK_ID'"}]
            }
        }')

    http_code=$(echo "$response" | tail -1)
    body=$(echo "$response" | sed '$d')  # Remove last line (portable)

    if [ "$http_code" = "202" ]; then
        SERVER_ID=$(echo "$body" | jq -r '.server.id')
        print_info "Server creation accepted: $server_name ($SERVER_ID)"

        # 202 only means Nova accepted the request. Poll until the server actually reaches
        # ACTIVE, so a Nova that accepts and then discards the request cannot pass this test.
        server_status=""
        for _ in $(seq 1 30); do
            server_status=$(curl -s -H "X-Auth-Token: $TOKEN" \
                "http://localhost:8774/v2.1/servers/$SERVER_ID" | jq -r '.server.status // empty')
            case "$server_status" in
                ACTIVE|ERROR) break ;;
            esac
            sleep 2
        done

        if [ "$server_status" = "ACTIVE" ]; then
            print_pass "Server reached ACTIVE: $server_name"
        else
            print_fail "Server never reached ACTIVE (last status: ${server_status:-unknown})"
        fi

        # Cleanup - delete test server. curl exits 0 on a 5xx, so the HTTP code is what
        # decides: 204 is Nova's success code and 404 means it is already gone. Anything
        # else leaves SERVER_ID set for the EXIT trap to retry, and says so rather than
        # logging a deletion that did not happen.
        delete_code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
            -H "X-Auth-Token: $TOKEN" \
            "http://localhost:8774/v2.1/servers/$SERVER_ID")
        case "$delete_code" in
            204|404)
                SERVER_ID=""
                print_info "Test server deleted"
                ;;
            *)
                print_fail "Test server cleanup failed (HTTP $delete_code) - retrying at exit"
                ;;
        esac
    else
        print_fail "Server creation failed (HTTP $http_code)"
        if [ -n "$body" ]; then
            print_info "Error: $body"
        fi
    fi
}

# Main
# Assert one service rejects a bogus token. Used by test_auth_rejection below.
expect_invalid_token_401() {
    local label="$1" url="$2" code
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: definitely-not-a-valid-token" "$url")
    if [ "$code" = "401" ]; then
        print_pass "$label rejects an invalid token (401)"
    else
        print_fail "$label returned HTTP $code for an invalid token (expected 401)"
    fi
}

# Test 7: negative paths. Every service the dashboard talks to must refuse bad credentials.
# These are expected to pass — the group exists as a regression gate, so that an auth
# middleware that starts fetching data for an unauthenticated caller fails the suite.
test_auth_rejection() {
    print_test "Auth rejection (regression gate)"

    expect_invalid_token_401 "Nova"    "http://localhost:8774/v2.1/servers"
    expect_invalid_token_401 "Neutron" "http://localhost:9696/v2.0/networks"
    expect_invalid_token_401 "Glance"  "http://localhost:9292/v2/images"
    expect_invalid_token_401 "Cinder"  "http://localhost:8776/v3/volumes"

    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8774/v2.1/servers")
    if [ "$code" = "401" ]; then
        print_pass "Nova rejects a missing token (401)"
    else
        print_fail "Nova returned HTTP $code with no token (expected 401)"
    fi

    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${LIGHTSTACK_URL}/v3/auth/tokens" \
        -H "Content-Type: application/json" \
        -d '{"auth": {"identity": {"methods": ["password"], "password": {"user": {"name": "'"$ADMIN_USER"'", "domain": {"name": "Default"}, "password": "definitely-the-wrong-password"}}}}}')
    if [ "$code" = "401" ]; then
        print_pass "Keystone rejects a wrong password (401)"
    else
        print_fail "Keystone returned HTTP $code for a wrong password (expected 401)"
    fi
}

# The test server is deleted inline on success; this trap covers an interrupt or an early
# exit between create and delete, which would otherwise leak it.
SERVER_ID=""
cleanup() {
    if [ -n "${SERVER_ID:-}" ] && [ -n "${TOKEN:-}" ]; then
        curl -s -X DELETE -H "X-Auth-Token: $TOKEN" \
            "http://localhost:8774/v2.1/servers/$SERVER_ID" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT INT TERM

echo "=========================================="
echo " Lightstack Horizon Compatibility Test"
echo "=========================================="
echo ""

test_login || exit 1

# Each group records its own pass/fail via the counters; `|| true` keeps one failing group
# from aborting the run under `set -e`, so the summary below is always reached and the exit
# status comes from FAILED rather than from whichever group happened to fail first.
test_dashboard || true
test_instances_tab || true
test_networks_tab || true
test_volumes_tab || true
test_images_tab || true
test_launch_instance || true
test_auth_rejection || true

echo ""
echo "=========================================="
echo " Test Summary"
echo "=========================================="
echo "Total Passed: $PASSED"
echo "Total Failed: $FAILED"
echo ""

if [ $FAILED -eq 0 ]; then
    echo -e "${GREEN}✓ All Horizon compatibility tests passed!${NC}"
    echo ""
    echo "Next steps:"
    echo "  1. Deploy actual Horizon dashboard with Docker"
    echo "  2. Test with web browser"
    echo "  3. Verify UI rendering and interactions"
    exit 0
else
    echo -e "${RED}✗ Some tests failed${NC}"
    exit 1
fi
