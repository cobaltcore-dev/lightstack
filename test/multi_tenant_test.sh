#!/bin/bash
# Multi-tenancy and Project Isolation Test Suite

set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() {
    echo -e "${YELLOW}[TEST]${NC} $1"
}

log_pass() {
    echo -e "${GREEN}✓${NC} $1"
    TESTS_PASSED=$((TESTS_PASSED + 1))
}

log_fail() {
    echo -e "${RED}✗${NC} $1"
    TESTS_FAILED=$((TESTS_FAILED + 1))
}

# require_value aborts setup when something it needed was not created.
#
# The literal string "null" matters as much as the empty string: `jq -r` prints
# `null` for a field that is absent, and `[ -z "null" ]` is FALSE — which is exactly
# why the pre-existing ADMIN_TOKEN guard shape could never catch a failed project
# create. Without this the suite ran every check against PROJECT1=null and still
# printed a summary (cloud-storage#545 FR-017).
require_value() {
    local label="$1" value="$2"
    if [ -z "$value" ] || [ "$value" = "null" ]; then
        echo -e "${RED}SETUP FAILED: could not create ${label} (got '${value}')${NC}"
        echo -e "${RED}Refusing to run isolation checks against resources that do not exist.${NC}"
        exit 1
    fi
}

# require_status aborts setup when a call that returns no body failed. Role
# assignments are the case FR-017 names and the ids above cannot cover: they answer
# 204 with an empty body, so there is no value to inspect — and they were previously
# piped to /dev/null with the status discarded. A silently failed assignment leaves
# role-less tokens, every isolation check then refuses for the wrong reason, and the
# suite reports passes for isolation it never exercised.
require_status() {
    local label="$1" want="$2" got="$3"
    if [ "$got" != "$want" ]; then
        echo -e "${RED}SETUP FAILED: ${label} returned HTTP ${got}, want ${want}${NC}"
        exit 1
    fi
}

# Resources the teardown removes. Populated as setup progresses so an abort part-way
# through still cleans up what it managed to create.
PROJECT1_ID=""; PROJECT2_ID=""; USER1_ID=""; USER2_ID=""
USER1_TOKEN=""; USER2_TOKEN=""; MEMBER_ROLE_ID=""
SERVER1_ID=""; SERVER2_ID=""; NET1_ID=""; NET2_ID=""; SUBNET2_ID=""; VOL1_ID=""; IMAGE1_ID=""

# Teardown runs however the suite exits — pass, failed check, or aborted setup — and
# is registered before the first resource is created.
#
# It has to: the suite creates its projects under FIXED names, so once a duplicate
# name is a 409 (FR-004) and setup exits early (FR-017), one crashed run would make
# every later run abort at setup. Two fixes in this feature combine into that failure
# mode, so re-runnability is a requirement, not tidiness (FR-017a).
teardown() {
    local exit_code=$?
    echo ""
    echo "Cleaning up test resources..."
    # Dependants before dependencies. Every deletion tolerates a resource that was
    # never created, so an abort during setup is not itself an error.
    if [ -n "$USER1_TOKEN" ] && [ "$USER1_TOKEN" != "null" ]; then
        curl -s -X GET "http://localhost:8774/v2.1/servers" -H "X-Auth-Token: $USER1_TOKEN" 2>/dev/null \
            | jq -r '.servers[]?.id // empty' 2>/dev/null | while read -r sid; do
            curl -s -o /dev/null -X DELETE "http://localhost:8774/v2.1/servers/$sid" -H "X-Auth-Token: $USER1_TOKEN" 2>/dev/null || true
        done
        [ -n "$VOL1_ID" ] && [ "$VOL1_ID" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:8776/v3/$PROJECT1_ID/volumes/$VOL1_ID" -H "X-Auth-Token: $USER1_TOKEN" 2>/dev/null || true
        [ -n "$NET1_ID" ] && [ "$NET1_ID" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/networks/$NET1_ID" -H "X-Auth-Token: $USER1_TOKEN" 2>/dev/null || true
        [ -n "$IMAGE1_ID" ] && [ "$IMAGE1_ID" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:9292/v2/images/$IMAGE1_ID" -H "X-Auth-Token: $USER1_TOKEN" 2>/dev/null || true
    fi
    if [ -n "$USER2_TOKEN" ] && [ "$USER2_TOKEN" != "null" ]; then
        [ -n "$SERVER2_ID" ] && [ "$SERVER2_ID" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:8774/v2.1/servers/$SERVER2_ID" -H "X-Auth-Token: $USER2_TOKEN" 2>/dev/null || true
        # Before the network: DeleteNetwork refuses a network that still has subnets
        # ("network has subnets; delete subnets first"), and that refusal is invisible
        # here. Skipping it left the network row to the project's ON DELETE CASCADE and,
        # in real mode, left the subnet's bridge and dnsmasq running for good.
        [ -n "$SUBNET2_ID" ] && [ "$SUBNET2_ID" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/subnets/$SUBNET2_ID" -H "X-Auth-Token: $USER2_TOKEN" 2>/dev/null || true
        [ -n "$NET2_ID" ] && [ "$NET2_ID" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:9696/v2.0/networks/$NET2_ID" -H "X-Auth-Token: $USER2_TOKEN" 2>/dev/null || true
    fi
    if [ -n "$ADMIN_TOKEN" ] && [ "$ADMIN_TOKEN" != "null" ]; then
        for uid in "$USER1_ID" "$USER2_ID"; do
            [ -n "$uid" ] && [ "$uid" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/users/$uid" -H "X-Auth-Token: $ADMIN_TOKEN" 2>/dev/null || true
        done
        for pid in "$PROJECT1_ID" "$PROJECT2_ID"; do
            [ -n "$pid" ] && [ "$pid" != "null" ] && curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/projects/$pid" -H "X-Auth-Token: $ADMIN_TOKEN" 2>/dev/null || true
        done
    fi
    # Never mask the suite's own verdict with teardown's success.
    exit $exit_code
}
trap teardown EXIT

echo "========================================"
echo " Multi-Tenancy & Isolation Test Suite"
echo "========================================"
echo ""

# Setup: Create two projects with different users
echo "Setting up test projects and users..."

# Admin authentication
ADMIN_TOKEN=$(curl -i -s -X POST "http://localhost:35357/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' \
    | grep -i '^X-Subject-Token:' | awk '{print $2}' | tr -d '\r')

if [ -z "$ADMIN_TOKEN" ]; then
    echo -e "${RED}Failed to get admin token${NC}"
    exit 1
fi

# Create project1
PROJECT1_ID=$(curl -s -X POST "http://localhost:35357/v3/projects" \
    -H "X-Auth-Token: $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"project":{"name":"test-project-1","domain_id":"default"}}' \
    | jq -r '.project.id')
require_value "project test-project-1" "$PROJECT1_ID"

# Create project2
PROJECT2_ID=$(curl -s -X POST "http://localhost:35357/v3/projects" \
    -H "X-Auth-Token: $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"project":{"name":"test-project-2","domain_id":"default"}}' \
    | jq -r '.project.id')
require_value "project test-project-2" "$PROJECT2_ID"

# Create user1 for project1
USER1_ID=$(curl -s -X POST "http://localhost:35357/v3/users" \
    -H "X-Auth-Token: $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"user":{"name":"test-user-1","password":"password1","domain_id":"default"}}' \
    | jq -r '.user.id')
require_value "user test-user-1" "$USER1_ID"

# Create user2 for project2
USER2_ID=$(curl -s -X POST "http://localhost:35357/v3/users" \
    -H "X-Auth-Token: $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"user":{"name":"test-user-2","password":"password2","domain_id":"default"}}' \
    | jq -r '.user.id')
require_value "user test-user-2" "$USER2_ID"

# Get member role ID
# Pick the member role by name on the CLIENT side, and never by position.
#
# Two traps here, both found live (cloud-storage#545):
#   1. `GET /v3/roles?name=…` ignores the filter and returns every role, so
#      `.roles[0].id` is whichever sorts first — which is `admin`.
#   2. O3K seeds `admin` / `member` / `reader`; there is no `_member_` (the old
#      OpenStack spelling this suite used to ask for).
# Together they meant this suite granted **admin** to both of its test users, so every
# isolation check below ran with two administrators and could not have detected a
# cross-tenant breach. Selecting by name and asserting it is not admin closes that.
MEMBER_ROLE_ID=$(curl -s -X GET "http://localhost:35357/v3/roles" \
    -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.roles[] | select(.name=="member") | .id')
require_value "the member role lookup" "$MEMBER_ROLE_ID"

ADMIN_ROLE_ID=$(curl -s -X GET "http://localhost:35357/v3/roles" \
    -H "X-Auth-Token: $ADMIN_TOKEN" | jq -r '.roles[] | select(.name=="admin") | .id')
if [ "$MEMBER_ROLE_ID" = "$ADMIN_ROLE_ID" ]; then
    echo -e "${RED}SETUP FAILED: the member role resolved to the admin role — isolation checks would be meaningless${NC}"
    exit 1
fi

# Assign user1 to project1. The status is checked, not discarded: a role assignment
# answers 204 with an empty body, so there is no id to guard, and a silent failure
# here would leave a role-less token that makes every isolation check refuse for the
# wrong reason (FR-017).
ASSIGN1_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
    "http://localhost:35357/v3/projects/$PROJECT1_ID/users/$USER1_ID/roles/$MEMBER_ROLE_ID" \
    -H "X-Auth-Token: $ADMIN_TOKEN")
require_status "role assignment for test-user-1" "204" "$ASSIGN1_STATUS"

# Assign user2 to project2
ASSIGN2_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X PUT \
    "http://localhost:35357/v3/projects/$PROJECT2_ID/users/$USER2_ID/roles/$MEMBER_ROLE_ID" \
    -H "X-Auth-Token: $ADMIN_TOKEN")
require_status "role assignment for test-user-2" "204" "$ASSIGN2_STATUS"

echo "Created PROJECT1=$PROJECT1_ID, USER1=$USER1_ID"
echo "Created PROJECT2=$PROJECT2_ID, USER2=$USER2_ID"
echo ""

# Get tokens for user1 and user2
USER1_TOKEN=$(curl -i -s -X POST "http://localhost:35357/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"test-user-1","password":"password1","domain":{"name":"Default"}}}},"scope":{"project":{"id":"'$PROJECT1_ID'"}}}}' \
    | grep -i '^X-Subject-Token:' | awk '{print $2}' | tr -d '\r')

USER2_TOKEN=$(curl -i -s -X POST "http://localhost:35357/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"test-user-2","password":"password2","domain":{"name":"Default"}}}},"scope":{"project":{"id":"'$PROJECT2_ID'"}}}}' \
    | grep -i '^X-Subject-Token:' | awk '{print $2}' | tr -d '\r')

require_value "a scoped token for test-user-1" "$USER1_TOKEN"
require_value "a scoped token for test-user-2" "$USER2_TOKEN"

# Neither tenant token may carry `admin`: an admin caller legitimately bypasses the
# very isolation this suite is here to check, so an admin token turns every check
# below into a tautology.
for pair in "test-user-1:$USER1_TOKEN" "test-user-2:$USER2_TOKEN"; do
    who=${pair%%:*}; tk=${pair##*:}
    claims=$(echo "$tk" | cut -d. -f2 | tr '_-' '/+')
    case $(( ${#claims} % 4 )) in 2) claims="${claims}==" ;; 3) claims="${claims}=" ;; esac
    if echo "$claims" | base64 -d 2>/dev/null | jq -e '.roles | index("admin")' >/dev/null 2>&1; then
        echo -e "${RED}SETUP FAILED: ${who}'s token carries the admin role — isolation checks would be meaningless${NC}"
        exit 1
    fi
done

# Test 1: Server isolation - User1 creates server, User2 cannot see it
log_test "Server isolation between projects"

SERVER1_ID=$(curl -s -X POST "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: $USER1_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"server":{"name":"project1-server","flavorRef":"m1.tiny"}}' \
    | jq -r '.server.id')

if [ -n "$SERVER1_ID" ] && [ "$SERVER1_ID" != "null" ]; then
    # User1 can see their own server
    USER1_SEES_OWN=$(curl -s -X GET "http://localhost:8774/v2.1/servers/$SERVER1_ID" \
        -H "X-Auth-Token: $USER1_TOKEN" | jq -r '.server.id')

    if [ "$USER1_SEES_OWN" = "$SERVER1_ID" ]; then
        log_pass "User1 can see their own server"
    else
        log_fail "User1 cannot see their own server"
    fi

    # User2 cannot see user1's server
    USER2_SEES_USER1=$(curl -s -X GET "http://localhost:8774/v2.1/servers/$SERVER1_ID" \
        -H "X-Auth-Token: $USER2_TOKEN" | jq -r '.error // .itemNotFound // "not-found"')

    if [ "$USER2_SEES_USER1" != "$SERVER1_ID" ]; then
        log_pass "User2 cannot see User1's server (isolation working)"
    else
        log_fail "User2 can see User1's server (ISOLATION BREACH)"
    fi

    # User2 cannot delete user1's server
    DELETE_RESULT=$(curl -s -w "%{http_code}" -X DELETE "http://localhost:8774/v2.1/servers/$SERVER1_ID" \
        -H "X-Auth-Token: $USER2_TOKEN" -o /dev/null)

    if [ "$DELETE_RESULT" = "404" ] || [ "$DELETE_RESULT" = "403" ]; then
        log_pass "User2 cannot delete User1's server"
    else
        log_fail "User2 can delete User1's server (ISOLATION BREACH)"
    fi
else
    log_fail "Failed to create server for User1"
fi

# Test 2: Network isolation
log_test "Network isolation between projects"

NET1_ID=$(curl -s -X POST "http://localhost:9696/v2.0/networks" \
    -H "X-Auth-Token: $USER1_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"network":{"name":"project1-network"}}' \
    | jq -r '.network.id')

if [ -n "$NET1_ID" ] && [ "$NET1_ID" != "null" ]; then
    # User2 cannot see user1's network
    USER2_NETS=$(curl -s -X GET "http://localhost:9696/v2.0/networks" \
        -H "X-Auth-Token: $USER2_TOKEN" | jq -r '.networks[] | select(.id=="'$NET1_ID'") | .id')

    if [ -z "$USER2_NETS" ]; then
        log_pass "User2 cannot see User1's network"
    else
        log_fail "User2 can see User1's network (ISOLATION BREACH)"
    fi

    # Cross-tenant WRITE (cloud-storage#545 B8, FR-018). Everything above this point
    # only ever checked reads, which is exactly how a tenant being able to define the
    # address range, gateway and DHCP behaviour on another tenant's network survived
    # in a suite called "Multi-Tenancy & Isolation".
    SUBNET_WRITE_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://localhost:9696/v2.0/subnets" \
        -H "X-Auth-Token: $USER2_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"subnet":{"name":"intruder","network_id":"'$NET1_ID'","cidr":"10.199.0.0/24","ip_version":4}}')

    if [ "$SUBNET_WRITE_STATUS" = "404" ] || [ "$SUBNET_WRITE_STATUS" = "403" ]; then
        log_pass "User2 cannot add a subnet to User1's network (HTTP $SUBNET_WRITE_STATUS)"
    else
        log_fail "User2 CAN add a subnet to User1's network (HTTP $SUBNET_WRITE_STATUS) (ISOLATION BREACH)"
    fi

    # And the victim's own views must show no trace of the attempt.
    USER1_SUBNETS=$(curl -s -X GET "http://localhost:9696/v2.0/subnets" \
        -H "X-Auth-Token: $USER1_TOKEN" | jq -r '[.subnets[]? | select(.name=="intruder")] | length')
    if [ "$USER1_SUBNETS" = "0" ]; then
        log_pass "User1's subnet list shows no trace of the refused write"
    else
        log_fail "User1's subnet list contains User2's subnet (ISOLATION BREACH)"
    fi

    # The fix must cost no legitimate capability: User2 on User2's own network. A
    # check that only proved refusal would pass against a handler refusing everything.
    NET2_ID=$(curl -s -X POST "http://localhost:9696/v2.0/networks" \
        -H "X-Auth-Token: $USER2_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"network":{"name":"project2-network"}}' \
        | jq -r '.network.id')
    # Keep the body as well as the status: this is the one subnet the suite really
    # creates, and teardown needs its id to delete it before the network it sits on.
    OWN_SUBNET_BODY=$(curl -s -w '\n%{http_code}' -X POST "http://localhost:9696/v2.0/subnets" \
        -H "X-Auth-Token: $USER2_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"subnet":{"name":"project2-subnet","network_id":"'$NET2_ID'","cidr":"10.198.0.0/24","ip_version":4}}')
    OWN_SUBNET_STATUS=$(echo "$OWN_SUBNET_BODY" | tail -1)
    # `|| true`: on a refused create the body is not the subnet document jq expects (or
    # not JSON at all), and jq's non-zero exit inside a command substitution would trip
    # `set -e` and kill the suite before the check below could report the failure.
    SUBNET2_ID=$(echo "$OWN_SUBNET_BODY" | sed '$d' | jq -r '.subnet.id // empty' 2>/dev/null || true)
    if [ "$OWN_SUBNET_STATUS" = "201" ]; then
        log_pass "User2 can still add a subnet to their own network"
    else
        log_fail "User2 cannot add a subnet to their own network (HTTP $OWN_SUBNET_STATUS) — the fix cost a legitimate capability"
    fi
else
    log_fail "Failed to create network for User1"
fi

# Test 3: Volume isolation
log_test "Volume isolation between projects"

VOL1_ID=$(curl -s -X POST "http://localhost:8776/v3/$PROJECT1_ID/volumes" \
    -H "X-Auth-Token: $USER1_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"volume":{"name":"project1-volume","size":1}}' \
    | jq -r '.volume.id')

if [ -n "$VOL1_ID" ] && [ "$VOL1_ID" != "null" ]; then
    # User2 cannot see user1's volume
    USER2_VOLS=$(curl -s -X GET "http://localhost:8776/v3/$PROJECT2_ID/volumes/$VOL1_ID" \
        -H "X-Auth-Token: $USER2_TOKEN" | jq -r '.error // .itemNotFound // "not-found"')

    if [ "$USER2_VOLS" != "$VOL1_ID" ]; then
        log_pass "User2 cannot see User1's volume"
    else
        log_fail "User2 can see User1's volume (ISOLATION BREACH)"
    fi

    # The project-scoped volume LIST (cloud-storage#545 B2, FR-019). The suite has
    # always created and deleted on this URL form but never listed on it, which is how
    # a 405 on the collection went unnoticed.
    LIST_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X GET "http://localhost:8776/v3/$PROJECT1_ID/volumes" \
        -H "X-Auth-Token: $USER1_TOKEN")
    LIST_HAS_VOL=$(curl -s -X GET "http://localhost:8776/v3/$PROJECT1_ID/volumes" \
        -H "X-Auth-Token: $USER1_TOKEN" | jq -r '[.volumes[]? | select(.id=="'$VOL1_ID'")] | length')
    if [ "$LIST_STATUS" = "200" ] && [ "$LIST_HAS_VOL" = "1" ]; then
        log_pass "User1 can list volumes on the project-scoped URL form"
    else
        log_fail "Project-scoped volume list broken (HTTP $LIST_STATUS, found $LIST_HAS_VOL)"
    fi

    # The project segment must not widen visibility: User2 naming User1's project in
    # the path still sees only their own volumes (FR-015).
    USER2_SEES=$(curl -s -X GET "http://localhost:8776/v3/$PROJECT1_ID/volumes" \
        -H "X-Auth-Token: $USER2_TOKEN" | jq -r '[.volumes[]? | select(.id=="'$VOL1_ID'")] | length')
    if [ "$USER2_SEES" = "0" ]; then
        log_pass "The project segment in the URL does not widen what User2 can see"
    else
        log_fail "User2 sees User1's volume via the project-scoped URL (ISOLATION BREACH)"
    fi
else
    log_fail "Failed to create volume for User1"
fi

# Test 4: Image isolation (private images)
log_test "Image isolation between projects"

IMAGE1_ID=$(curl -s -X POST "http://localhost:9292/v2/images" \
    -H "X-Auth-Token: $USER1_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"name":"project1-private-image","visibility":"private","container_format":"bare","disk_format":"raw"}' \
    | jq -r '.id')

if [ -n "$IMAGE1_ID" ] && [ "$IMAGE1_ID" != "null" ]; then
    # User2 cannot see user1's private image
    USER2_IMAGES=$(curl -s -X GET "http://localhost:9292/v2/images" \
        -H "X-Auth-Token: $USER2_TOKEN" | jq -r '.images[] | select(.id=="'$IMAGE1_ID'") | .id')

    if [ -z "$USER2_IMAGES" ]; then
        log_pass "User2 cannot see User1's private image"
    else
        log_fail "User2 can see User1's private image (ISOLATION BREACH)"
    fi
else
    log_fail "Failed to create image for User1"
fi

# Test 5: Quota isolation
log_test "Quota isolation between projects"

# User1 creates resources
for i in {1..3}; do
    curl -s -X POST "http://localhost:8774/v2.1/servers" \
        -H "X-Auth-Token: $USER1_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"server":{"name":"project1-server-'$i'","flavorRef":"m1.tiny"}}' > /dev/null
done

# Check User1's usage affects their quota, not User2's
USER1_SERVERS=$(curl -s -X GET "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: $USER1_TOKEN" | jq -r '.servers | length')

USER2_SERVERS=$(curl -s -X GET "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: $USER2_TOKEN" | jq -r '.servers | length')

if [ "$USER1_SERVERS" -ge 3 ] && [ "$USER2_SERVERS" -eq 0 ]; then
    log_pass "Quota usage isolated between projects"
else
    log_fail "Quota usage not properly isolated (User1=$USER1_SERVERS, User2=$USER2_SERVERS)"
fi

# Test 6: Cross-project resource references
log_test "Cannot reference cross-project resources"

# User2 tries to attach User1's volume to their own server (should fail)
SERVER2_ID=$(curl -s -X POST "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: $USER2_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"server":{"name":"project2-server","flavorRef":"m1.tiny"}}' \
    | jq -r '.server.id')

if [ -n "$SERVER2_ID" ] && [ "$SERVER2_ID" != "null" ] && [ -n "$VOL1_ID" ]; then
    # Assert on the HTTP status, not on guessed error-body keys.
    #
    # This check used to read `jq -r '.error // .badRequest // "success"'`. O3K refuses
    # this attach with `{"itemNotFound": …}` — neither key the expression looks for — so
    # jq fell through to the literal "success" and the check reported an ISOLATION
    # BREACH that does not exist. It was a false FAIL for as long as it could run at all
    # (which was never, because volume creation had to succeed first).
    #
    # Same root cause as the rest of this suite's history: an assertion that cannot read
    # the response it is judging. A status code cannot be misread (cloud-storage#545).
    ATTACH_STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://localhost:8774/v2.1/servers/$SERVER2_ID/os-volume_attachments" \
        -H "X-Auth-Token: $USER2_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"volumeAttachment":{"volumeId":"'$VOL1_ID'"}}')

    case "$ATTACH_STATUS" in
        2*) log_fail "Can attach cross-project volume (HTTP $ATTACH_STATUS) (ISOLATION BREACH)" ;;
        *)  log_pass "Cannot attach cross-project volume (HTTP $ATTACH_STATUS)" ;;
    esac
fi

# Cleanup is handled by the EXIT trap registered at the top of this script, so it
# runs on a failed check or an aborted setup too — not only on the happy path this
# block used to be reachable from (FR-017a).

# Summary
echo ""
echo "========================================"
echo "Test Summary"
echo "========================================"
echo -e "Passed: ${GREEN}$TESTS_PASSED${NC}"
echo -e "Failed: ${RED}$TESTS_FAILED${NC}"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}All multi-tenancy tests passed!${NC}"
    exit 0
else
    echo -e "${RED}Some multi-tenancy tests failed!${NC}"
    exit 1
fi
