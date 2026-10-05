#!/bin/bash
# Lightstack Horizon create/write UI enablement test (SPEC-020, cloud-storage#533)
#
# Probes the endpoints Horizon's create flows load before they will render a
# create action, open a modal, or validate a wizard step. Findings F2, F3, F7, F8.
#
# What it guards, section by section:
#   - SC-005 / FR-003 - every quota, limits and capability endpoint the Overview
#     and create flows read returns success, and the absolute limits agree between
#     the limits view and the quota-set view.
#   - FR-007 - networks carry project_id and every subnet id they reference resolves
#     (launch-wizard selectability).
#   - FR-021 / FR-022 / FR-023 - the paths and response shapes the OpenStack CLIENTS
#     actually use, not the ones the API reference documents: cinder groups/detail and
#     os-quota-sets, and the security-group rule key set. Added after a pass reported
#     26/26 while two workflows were still broken, precisely because these went
#     unprobed.
#   - SC-007a - a quota target that does not exist is a 404 on nova, neutron AND
#     cinder, for an unknown UUID, the nil UUID and a non-UUID alike.
#   - SC-001 - SC-004 - the create flows themselves: the requests Horizon sends to
#     create a volume, create a network with a subnet and launch an instance on the
#     wizard's defaults, and the Overview's limit reads after a project switch. Each
#     request is replayed in the shape Horizon builds it, and everything created is
#     removed again, on failure too.
#
# It began as a trace tool (T003/T005) for finding which endpoints 404'd; that phase
# is over and it is now a regression guard, run by CI on the Compose stack. It does
# not drive Horizon itself: the browser pass stays the runbook's manual check.
#
# Usage:  ./test/horizon_create_ui_test.sh
# Requires: a running Lightstack. The SPEC-020 runbooks use
#   deployments/docker-compose-horizon.yml (stub) or the k3s real-mode deployment;
#   deployments/docker-compose.yml is enough: these are API checks.
#   Override KEYSTONE_URL / NOVA_URL / NEUTRON_URL / CINDER_URL / GLANCE_URL to retarget.

set -u

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

PASSED=0
FAILED=0

KEYSTONE_URL="${KEYSTONE_URL:-http://localhost:35357}"
NOVA_URL="${NOVA_URL:-http://localhost:8774}"
NEUTRON_URL="${NEUTRON_URL:-http://localhost:9696}"
CINDER_URL="${CINDER_URL:-http://localhost:8776}"
GLANCE_URL="${GLANCE_URL:-http://localhost:9292}"
ADMIN_USER="${OS_USERNAME:-admin}"
ADMIN_PASSWORD="${OS_PASSWORD:-secret}"
PROJECT_NAME="${OS_PROJECT_NAME:-default}"

print_test() { echo -e "\n${YELLOW}[TEST]${NC} $1"; }
print_pass() { echo -e "${GREEN}[PASS]${NC} $1"; PASSED=$((PASSED + 1)); }
print_fail() { echo -e "${RED}[FAIL]${NC} $1"; FAILED=$((FAILED + 1)); }
print_info() { echo -e "  |  $1"; }
print_head() { echo -e "\n${BLUE}=== $1 ===${NC}"; }

# ---------------------------------------------------------------- auth

TOKEN=""
PROJECT_ID=""
USER_ID=""

authenticate() {
    print_test "Authenticate as ${ADMIN_USER} (scoped to ${PROJECT_NAME})"
    local response
    response=$(curl -s -i -X POST "${KEYSTONE_URL}/v3/auth/tokens" \
        -H "Content-Type: application/json" \
        -d '{
            "auth": {
                "identity": {
                    "methods": ["password"],
                    "password": {
                        "user": {
                            "name": "'"$ADMIN_USER"'",
                            "domain": {"name": "Default"},
                            "password": "'"$ADMIN_PASSWORD"'"
                        }
                    }
                },
                "scope": {
                    "project": {
                        "name": "'"$PROJECT_NAME"'",
                        "domain": {"name": "Default"}
                    }
                }
            }
        }')

    TOKEN=$(echo "$response" | grep -i "^X-Subject-Token:" | awk '{print $2}' | tr -d '\r')
    PROJECT_ID=$(echo "$response" | sed -n '/^{/,$p' | jq -r '.token.project.id // empty')
    USER_ID=$(echo "$response" | sed -n '/^{/,$p' | jq -r '.token.user.id // empty')

    if [ -n "$TOKEN" ] && [ -n "$PROJECT_ID" ]; then
        print_pass "Token acquired (project_id=${PROJECT_ID})"
    else
        print_fail "Could not authenticate - is Lightstack running at ${KEYSTONE_URL}?"
        echo "$response" | tail -5
        exit 1
    fi
}

# get <label> <url> -> sets HTTP_CODE and BODY
HTTP_CODE=""
BODY=""
get() {
    local url="$1"
    local raw
    raw=$(curl -s -w '\n%{http_code}' -H "X-Auth-Token: ${TOKEN}" "$url")
    HTTP_CODE=$(echo "$raw" | tail -1)
    BODY=$(echo "$raw" | sed '$d')
}

# expect_ok <label> <url>
expect_ok() {
    local label="$1" url="$2"
    get "$url"
    if [ "$HTTP_CODE" = "200" ]; then
        print_pass "${label} -> 200"
    else
        print_fail "${label} -> ${HTTP_CODE} (expected 200)  ${url}"
        print_info "$(echo "$BODY" | head -c 200)"
    fi
}

# expect_code <label> <want> <url> - for endpoints whose expected status is not 200
expect_code() {
    local label="$1" want="$2" url="$3"
    get "$url"
    if [ "$HTTP_CODE" = "$want" ]; then
        print_pass "${label} -> ${HTTP_CODE}"
    else
        print_fail "${label} -> ${HTTP_CODE} (expected ${want})  ${url}"
        print_info "$(echo "$BODY" | head -c 160)"
    fi
}

# expect_key <label> <url> <jq-filter>
expect_key() {
    local label="$1" url="$2" filter="$3"
    get "$url"
    if [ "$HTTP_CODE" != "200" ]; then
        print_fail "${label} -> ${HTTP_CODE} (expected 200)  ${url}"
        return
    fi
    local val
    val=$(echo "$BODY" | jq -r "$filter" 2>/dev/null)
    if [ -n "$val" ] && [ "$val" != "null" ]; then
        print_pass "${label} -> ${filter} = ${val}"
    else
        print_fail "${label} -> ${filter} is missing/null"
        print_info "$(echo "$BODY" | head -c 200)"
    fi
}

# jqval <url> <filter> -> echoes the value (empty on failure)
jqval() {
    get "$1"
    [ "$HTTP_CODE" = "200" ] || { echo ""; return; }
    echo "$BODY" | jq -r "$2" 2>/dev/null
}

# --------------------------------------------- F2: Overview limits widget

test_overview_limits() {
    print_head "F2 - Project Overview usage/limits aggregation"

    print_test "Compute limits (Nova)"
    expect_key "GET /v2.1/limits" "${NOVA_URL}/v2.1/limits" '.limits.absolute.maxTotalInstances'

    print_test "Block-storage limits (Cinder)"
    expect_key "GET /v3/limits" "${CINDER_URL}/v3/limits" '.limits.absolute.maxTotalVolumes'

    print_test "Network quota (Neutron)"
    expect_key "GET /v2.0/quotas/{project}" "${NEUTRON_URL}/v2.0/quotas/${PROJECT_ID}" '.quota.network'

    print_test "Network quota carries project scope (FR-002a / FR-004)"
    expect_key "quota.project_id" "${NEUTRON_URL}/v2.0/quotas/${PROJECT_ID}" '.quota.project_id'
}

# ------------------------------------- FR-002: quota endpoint variants

test_quota_variants() {
    print_head "FR-002 - quota endpoint variants Horizon and OpenStack clients request"

    print_test "Compute quota-set + variants"
    expect_ok "GET /v2.1/os-quota-sets/{project}"          "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}"
    expect_ok "GET /v2.1/os-quota-sets/{project}/defaults" "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}/defaults"
    expect_ok "GET /v2.1/os-quota-sets/{project}/detail"   "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}/detail"

    print_test "Network quota + variants (.json is a neutronclient convention)"
    expect_ok "GET /v2.0/quotas/{project}/details"      "${NEUTRON_URL}/v2.0/quotas/${PROJECT_ID}/details"
    expect_ok "GET /v2.0/quotas/{project}/details.json" "${NEUTRON_URL}/v2.0/quotas/${PROJECT_ID}/details.json"
    expect_ok "GET /v2.0/quotas/{project}/default"      "${NEUTRON_URL}/v2.0/quotas/${PROJECT_ID}/default"

    print_test "Block-storage quota-set + variants"
    expect_ok "GET /v3/quota-sets/{project}"          "${CINDER_URL}/v3/quota-sets/${PROJECT_ID}"
    expect_key "GET /v3/quota-sets/{project}?usage=true" "${CINDER_URL}/v3/quota-sets/${PROJECT_ID}?usage=true" '.quota_set.volumes.in_use'
    expect_ok "GET /v3/quota-sets/{project}/defaults" "${CINDER_URL}/v3/quota-sets/${PROJECT_ID}/defaults"
}

# --------------------------------- FR-003: limits and quota-set must agree

test_limits_agreement() {
    print_head "FR-003 - absolute limits agree across the limits and quota-set views"

    print_test "Compute: maxTotalInstances == quota_set.instances"
    local lim qs
    lim=$(jqval "${NOVA_URL}/v2.1/limits" '.limits.absolute.maxTotalInstances')
    qs=$(jqval "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}" '.quota_set.instances')
    if [ -n "$lim" ] && [ "$lim" = "$qs" ]; then
        print_pass "instances: limits=${lim} quota_set=${qs} (agree)"
    else
        print_fail "instances DISAGREE: limits=${lim:-?} quota_set=${qs:-?}"
    fi

    print_test "Compute: maxTotalCores == quota_set.cores"
    lim=$(jqval "${NOVA_URL}/v2.1/limits" '.limits.absolute.maxTotalCores')
    qs=$(jqval "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}" '.quota_set.cores')
    if [ -n "$lim" ] && [ "$lim" = "$qs" ]; then
        print_pass "cores: limits=${lim} quota_set=${qs} (agree)"
    else
        print_fail "cores DISAGREE: limits=${lim:-?} quota_set=${qs:-?}"
    fi

    print_test "Compute: maxTotalRAMSize == quota_set.ram"
    lim=$(jqval "${NOVA_URL}/v2.1/limits" '.limits.absolute.maxTotalRAMSize')
    qs=$(jqval "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}" '.quota_set.ram')
    if [ -n "$lim" ] && [ "$lim" = "$qs" ]; then
        print_pass "ram: limits=${lim} quota_set=${qs} (agree)"
    else
        print_fail "ram DISAGREE: limits=${lim:-?} quota_set=${qs:-?}"
    fi

    print_test "Block-storage: maxTotalVolumes == quota_set.volumes"
    lim=$(jqval "${CINDER_URL}/v3/limits" '.limits.absolute.maxTotalVolumes')
    qs=$(jqval "${CINDER_URL}/v3/quota-sets/${PROJECT_ID}" '.quota_set.volumes')
    if [ -n "$lim" ] && [ "$lim" = "$qs" ]; then
        print_pass "volumes: limits=${lim} quota_set=${qs} (agree)"
    else
        print_fail "volumes DISAGREE: limits=${lim:-?} quota_set=${qs:-?}"
    fi
}

# ------------------------------- F8/F3: capability advertisement

test_capabilities() {
    print_head "FR-013 - capability advertisement Horizon gates create controls on"

    print_test "Cinder extensions (the Create Volume form calls extension_supported)"
    expect_ok "GET /v3/extensions" "${CINDER_URL}/v3/extensions"
    get "${CINDER_URL}/v3/extensions"
    if [ "$HTTP_CODE" = "200" ] && echo "$BODY" | jq -e '.extensions[] | select(.name == "AvailabilityZones")' >/dev/null 2>&1; then
        print_pass "cinder advertises AvailabilityZones"
    else
        print_fail "cinder does not advertise AvailabilityZones"
    fi

    print_test "Neutron extensions advertise quota_details"
    get "${NEUTRON_URL}/v2.0/extensions"
    if [ "$HTTP_CODE" = "200" ] && echo "$BODY" | jq -e '.extensions[] | select(.alias == "quota_details")' >/dev/null 2>&1; then
        print_pass "neutron advertises quota_details"
    else
        print_fail "neutron does not advertise quota_details (Horizon falls back to multi-call usage counting)"
    fi
}

# ---------------------------------- F8: Create Volume supporting metadata

test_volume_create_metadata() {
    print_head "F8 - Create Volume dialog supporting metadata"

    print_test "Volume types"
    expect_key "GET /v3/types" "${CINDER_URL}/v3/types" '.volume_types | length'

    print_test "Default volume type"
    expect_key "GET /v3/types/default" "${CINDER_URL}/v3/types/default" '.volume_type.name'

    print_test "Availability zones"
    expect_key "GET /v3/os-availability-zone" "${CINDER_URL}/v3/os-availability-zone" '.availabilityZoneInfo | length'
}

# ------------------------- F7: launch wizard network selectability

test_network_selectability() {
    print_head "F7 - launch wizard network selectability"

    print_test "Networks carry project_id (not only tenant_id)"
    get "${NEUTRON_URL}/v2.0/networks"
    if [ "$HTTP_CODE" != "200" ]; then
        print_fail "GET /v2.0/networks -> ${HTTP_CODE}"
        return
    fi
    local count missing
    count=$(echo "$BODY" | jq -r '.networks | length')
    if [ "$count" = "0" ]; then
        print_info "no networks present - skipping field assertions"
        print_pass "GET /v2.0/networks -> 200 (empty)"
        return
    fi
    missing=$(echo "$BODY" | jq -r '[.networks[] | select(has("project_id") | not)] | length')
    if [ "$missing" = "0" ]; then
        print_pass "all ${count} network(s) carry project_id"
    else
        print_fail "${missing}/${count} network(s) are missing project_id"
    fi

    print_test "Subnet-reference integrity: every network.subnets id resolves in the subnet list"
    local subnet_ids referenced dangling
    subnet_ids=$(curl -s -H "X-Auth-Token: ${TOKEN}" "${NEUTRON_URL}/v2.0/subnets" | jq -r '[.subnets[].id] | sort | join(",")')
    referenced=$(echo "$BODY" | jq -r '[.networks[].subnets[]?] | sort | join(",")')
    if [ -z "$referenced" ]; then
        print_info "no subnets referenced by any network - nothing to resolve"
        print_pass "subnet-reference integrity (vacuous)"
    else
        dangling=""
        for sid in $(echo "$referenced" | tr ',' ' '); do
            case ",${subnet_ids}," in
                *",${sid},"*) ;;
                *) dangling="${dangling} ${sid}" ;;
            esac
        done
        if [ -z "$dangling" ]; then
            print_pass "every referenced subnet id resolves in GET /v2.0/subnets"
        else
            print_fail "dangling subnet ids (Horizon renders these as null rows):${dangling}"
        fi
    fi
}

# ------------------- B1/N3/N1: paths the OpenStack clients actually request
#
# Added after the first stub pass reported 26/26 while F7 and F8 were still broken.
# Each of these is a path Horizon reaches through python-cinderclient or openstacksdk
# rather than one derived from the API reference, which is why the original probe
# missed them.

test_client_paths() {
    print_head "B1/N3/N1 - endpoints the clients request (not just the ones we document)"

    print_test "Cinder volume groups (cinderclient groups.list(detailed=True) -> groups/detail)"
    expect_ok "GET /v3/groups" "${CINDER_URL}/v3/groups"
    expect_ok "GET /v3/groups/detail" "${CINDER_URL}/v3/groups/detail"
    expect_ok "GET /v3/{project}/groups" "${CINDER_URL}/v3/${PROJECT_ID}/groups"
    expect_ok "GET /v3/{project}/groups/detail" "${CINDER_URL}/v3/${PROJECT_ID}/groups/detail"

    print_test "Cinder os-quota-sets (upstream spelling; cinderclient uses it, Horizon 500s without it)"
    expect_ok "GET /v3/os-quota-sets/{project}" "${CINDER_URL}/v3/os-quota-sets/${PROJECT_ID}"
    expect_key "GET /v3/os-quota-sets/{project}?usage=true" "${CINDER_URL}/v3/os-quota-sets/${PROJECT_ID}?usage=true" '.quota_set.volumes.limit'
    expect_ok "GET /v3/os-quota-sets/{project}/defaults" "${CINDER_URL}/v3/os-quota-sets/${PROJECT_ID}/defaults"

    print_test "Neutron security groups carry a rules array (openstacksdk turns a missing key into null)"
    get "${NEUTRON_URL}/v2.0/security-groups"
    # The three checks below all read this one $BODY, so bail out here rather than
    # letting them run against an unusable one. They are jq expressions whose errors go
    # to /dev/null, and an empty result reads as "nothing missing" — so on a 500 body the
    # key check below printed [PASS]. That is finding N1, the very thing this section
    # exists to catch, reported as success. Fail once on the real cause and stop.
    if [ "$HTTP_CODE" != "200" ]; then
        print_fail "GET /v2.0/security-groups -> ${HTTP_CODE} (expected 200)"
        print_info "$(echo "$BODY" | head -c 200)"
        return 1
    fi
    if ! echo "$BODY" | jq -e '.security_groups | length > 0' >/dev/null 2>&1; then
        print_fail "no security groups returned (migration 002 seeds a default group)"
        print_info "$(echo "$BODY" | head -c 200)"
        return 1
    fi
    if echo "$BODY" | jq -e '[.security_groups[] | select((.security_group_rules | type) != "array")] | length == 0' >/dev/null 2>&1; then
        print_pass "every security group has a security_group_rules array"
    else
        print_fail "a security group is missing security_group_rules -> Horizon: 'NoneType' object is not iterable"
        print_info "$(echo "$BODY" | jq -c '[.security_groups[] | {name, rules: (.security_group_rules | type)}]' 2>/dev/null | head -c 200)"
    fi

    print_test "Neutron security group rules carry every key Horizon dereferences (null is fine, absent is not)"
    # An empty "missing" list means either "no key is absent" or "there were no rules to
    # look at" — so count them first. Without this the check also passes vacuously on a
    # project whose groups happen to carry no rules.
    rule_count=$(echo "$BODY" | jq -r '[.security_groups[].security_group_rules[]?] | length' 2>/dev/null)
    missing=$(echo "$BODY" | jq -r '[.security_groups[].security_group_rules[]? | to_entries | map(.key) as $k | ["id","security_group_id","direction","ethertype","protocol","port_range_min","port_range_max","remote_ip_prefix","remote_group_id"] - $k | .[]] | unique | join(",")' 2>/dev/null)
    if [ -z "$rule_count" ] || [ "$rule_count" = "0" ]; then
        print_fail "no rules to inspect (migration 002 seeds rules on the default group) -> the key check cannot prove anything"
    elif [ -z "$missing" ]; then
        print_pass "all rule keys present (${rule_count} rules inspected)"
    else
        print_fail "rule keys absent: ${missing} -> Horizon: KeyError (HTTP 500) + 'Unable to retrieve security groups'"
    fi

    print_test "Neutron security groups carry project scope"
    if echo "$BODY" | jq -e --arg p "$PROJECT_ID" '[.security_groups[] | select(.project_id != $p or .tenant_id != $p)] | length == 0' >/dev/null 2>&1; then
        print_pass "every security group reports project_id and tenant_id"
    else
        print_fail "a security group is missing project_id/tenant_id"
    fi
}

# ------------------- SC-007: a quota target that does not exist is a 404
#
# All three services resolve the project through common.ResolveProjectRef, so none of
# them may serve defaults for a phantom project or 500 on a non-UUID. Cinder enforced
# this first; nova and neutron used the admin's URL id verbatim until the helper was
# lifted into common.

test_quota_target_authz() {
    print_head "SC-007 - phantom quota targets 404 across nova, neutron and cinder"

    for ref in 11111111-1111-1111-1111-111111111111 00000000-0000-0000-0000-000000000000 no-such-project; do
        print_test "phantom target '${ref}'"
        expect_code "nova os-quota-sets" 404 "${NOVA_URL}/v2.1/os-quota-sets/${ref}"
        expect_code "nova os-quota-sets detail" 404 "${NOVA_URL}/v2.1/os-quota-sets/${ref}/detail"
        expect_code "neutron quotas" 404 "${NEUTRON_URL}/v2.0/quotas/${ref}"
        expect_code "neutron quotas details" 404 "${NEUTRON_URL}/v2.0/quotas/${ref}/details"
        expect_code "cinder quota-sets" 404 "${CINDER_URL}/v3/quota-sets/${ref}"
    done

    print_test "a real project still resolves (by id and by the 'default' alias)"
    expect_code "nova os-quota-sets/{project}" 200 "${NOVA_URL}/v2.1/os-quota-sets/${PROJECT_ID}"
    expect_code "neutron quotas/{project}" 200 "${NEUTRON_URL}/v2.0/quotas/${PROJECT_ID}"
    expect_code "cinder quota-sets/default" 200 "${CINDER_URL}/v3/quota-sets/default"
}

# ------------------------------------------- SC-001 - SC-004: the create flows
#
# The checks above cover what Horizon reads before it enables a create control. These
# replay what it then sends, as Horizon stable/2025.2 builds each request, so the flows
# are gated and not only their prerequisites:
#   - Create Volume: python-cinderclient's body, unset fields null;
#   - Create Network: the network, then its subnet;
#   - Launch Instance on the wizard's defaults (launch-instance-model.service.js):
#     no imageRef, a new boot volume of the default type that outlives the server, the
#     network by id, and the security group by id under "name";
#   - the Overview's limit reads with a token scoped to a second project.

GATE_TAG="spec020-gate-$$"
GATE_VOLUME=""
GATE_NET=""
GATE_SUBNET=""
GATE_IMAGE=""
GATE_SERVER=""
GATE_BOOT_VOLUME=""
GATE_PROJECT=""

# send <method> <url> [json-body] -> sets HTTP_CODE and BODY
send() {
    local method="$1" url="$2" data="${3:-}" raw
    if [ -n "$data" ]; then
        raw=$(curl -s -w '\n%{http_code}' -X "$method" -H "X-Auth-Token: ${TOKEN}" \
            -H "Content-Type: application/json" -d "$data" "$url")
    else
        raw=$(curl -s -w '\n%{http_code}' -X "$method" -H "X-Auth-Token: ${TOKEN}" "$url")
    fi
    HTTP_CODE=$(echo "$raw" | tail -1)
    BODY=$(echo "$raw" | sed '$d')
}

# expect_send <label> <want> <method> <url> [json-body] - a write; want is a status
# code, or 2xx for a delete, whose exact success code differs between services.
expect_send() {
    local label="$1" want="$2" ok=false
    shift 2
    send "$@"
    case "$want" in
        2xx) case "$HTTP_CODE" in 2[0-9][0-9]) ok=true ;; esac ;;
        *) [ "$HTTP_CODE" = "$want" ] && ok=true ;;
    esac
    if $ok; then
        print_pass "${label} -> ${HTTP_CODE}"
        return 0
    fi
    print_fail "${label} -> ${HTTP_CODE} (expected ${want})"
    print_info "$(echo "$BODY" | head -c 200)"
    return 1
}

# wait_for <url> <jq-filter> <want> - polls for up to a minute
wait_for() {
    local url="$1" filter="$2" want="$3" got="" i
    for i in $(seq 1 30); do
        got=$(jqval "$url" "$filter")
        [ "$got" = "$want" ] && return 0
        sleep 2
    done
    print_info "last ${filter} = ${got:-<none>}"
    return 1
}

# check <label> <condition-result> - records a pass when the result is 0
check() {
    if [ "$2" = "0" ]; then print_pass "$1"; else print_fail "$1"; fi
}

# cleanup_gate removes, best effort and in dependency order, whatever a failed run left.
cleanup_gate() {
    local v
    [ -n "$GATE_SERVER" ] && curl -s -o /dev/null -X DELETE -H "X-Auth-Token: ${TOKEN}" "${NOVA_URL}/v2.1/servers/${GATE_SERVER}"
    for v in $GATE_BOOT_VOLUME $GATE_VOLUME; do
        curl -s -o /dev/null -X DELETE -H "X-Auth-Token: ${TOKEN}" "${CINDER_URL}/v3/volumes/${v}"
    done
    [ -n "$GATE_SUBNET" ] && curl -s -o /dev/null -X DELETE -H "X-Auth-Token: ${TOKEN}" "${NEUTRON_URL}/v2.0/subnets/${GATE_SUBNET}"
    [ -n "$GATE_NET" ] && curl -s -o /dev/null -X DELETE -H "X-Auth-Token: ${TOKEN}" "${NEUTRON_URL}/v2.0/networks/${GATE_NET}"
    [ -n "$GATE_IMAGE" ] && curl -s -o /dev/null -X DELETE -H "X-Auth-Token: ${TOKEN}" "${GLANCE_URL}/v2/images/${GATE_IMAGE}"
    [ -n "$GATE_PROJECT" ] && curl -s -o /dev/null -X DELETE -H "X-Auth-Token: ${TOKEN}" "${KEYSTONE_URL}/v3/projects/${GATE_PROJECT}"
    return 0
}
trap cleanup_gate EXIT

test_create_volume() {
    print_head "SC-002 - Create Volume, as the dialog submits it"

    print_test "POST /v3/volumes with python-cinderclient's body"
    if expect_send "create volume" 202 POST "${CINDER_URL}/v3/volumes" '{"volume": {
            "size": 1, "consistencygroup_id": null, "snapshot_id": null,
            "name": "'"${GATE_TAG}"'-vol", "description": "", "volume_type": "__DEFAULT__",
            "availability_zone": null, "metadata": {}, "imageRef": null,
            "source_volid": null, "backup_id": null}}'; then
        GATE_VOLUME=$(echo "$BODY" | jq -r '.volume.id // empty')
    fi
    [ -n "$GATE_VOLUME" ] || { print_fail "no volume id in the create response"; return; }

    print_test "it becomes available and is listed once"
    wait_for "${CINDER_URL}/v3/volumes/${GATE_VOLUME}" '.volume.status' available
    check "volume is available" $?
    [ "$(jqval "${CINDER_URL}/v3/volumes/detail" "[.volumes[] | select(.id == \"${GATE_VOLUME}\")] | length")" = "1" ]
    check "listed once in GET /v3/volumes/detail" $?

    print_test "delete it"
    expect_send "delete volume" 2xx DELETE "${CINDER_URL}/v3/volumes/${GATE_VOLUME}" && GATE_VOLUME=""
}

test_create_network() {
    print_head "SC-003 - Create Network with a subnet, as the Project panel's workflow submits it"

    print_test "POST /v2.0/networks, then POST /v2.0/subnets"
    if expect_send "create network" 201 POST "${NEUTRON_URL}/v2.0/networks" \
        '{"network": {"name": "'"${GATE_TAG}"'-net", "admin_state_up": true}}'; then
        GATE_NET=$(echo "$BODY" | jq -r '.network.id // empty')
    fi
    [ -n "$GATE_NET" ] || { print_fail "no network id in the create response"; return; }
    if expect_send "create subnet" 201 POST "${NEUTRON_URL}/v2.0/subnets" \
        '{"subnet": {"network_id": "'"${GATE_NET}"'", "name": "'"${GATE_TAG}"'-sub",
                     "cidr": "10.250.0.0/24", "ip_version": 4, "enable_dhcp": true}}'; then
        GATE_SUBNET=$(echo "$BODY" | jq -r '.subnet.id // empty')
    fi
    [ -n "$GATE_SUBNET" ] || { print_fail "no subnet id in the create response"; return; }

    print_test "listed once, owned by the project, with a subnet that resolves (wizard-selectable)"
    local entry
    entry=$(jqval "${NEUTRON_URL}/v2.0/networks" "[.networks[] | select(.id == \"${GATE_NET}\")]")
    [ "$(echo "$entry" | jq 'length')" = "1" ]
    check "listed once in GET /v2.0/networks" $?
    [ "$(echo "$entry" | jq -r '.[0].project_id // empty')" = "$PROJECT_ID" ]
    check "project_id is the caller's project" $?
    echo "$entry" | jq -e --arg s "$GATE_SUBNET" 'any(.[0].subnets[]?; . == $s)' > /dev/null
    check "the network lists the subnet" $?
    expect_code "GET /v2.0/subnets/{subnet}" 200 "${NEUTRON_URL}/v2.0/subnets/${GATE_SUBNET}"
}

test_launch_instance() {
    print_head "SC-001 - Launch Instance on the wizard's defaults"
    [ -n "$GATE_NET" ] && [ -n "$GATE_SUBNET" ] || { print_fail "no network from SC-003 to launch on"; return; }

    print_test "an image to boot, uploaded so it is active"
    if expect_send "create image" 201 POST "${GLANCE_URL}/v2/images" \
        '{"name": "'"${GATE_TAG}"'-img", "disk_format": "raw", "container_format": "bare"}'; then
        GATE_IMAGE=$(echo "$BODY" | jq -r '.id // empty')
    fi
    [ -n "$GATE_IMAGE" ] || { print_fail "no image id in the create response"; return; }
    [ "$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "X-Auth-Token: ${TOKEN}" \
        -H "Content-Type: application/octet-stream" --data-binary "lightstack spec020 gate image" \
        "${GLANCE_URL}/v2/images/${GATE_IMAGE}/file")" = "204" ]
    check "upload image data" $?

    local flavor sg
    flavor=$(jqval "${NOVA_URL}/v2.1/flavors" '[.flavors[] | select(.name == "m1.tiny")][0].id // empty')
    sg=$(jqval "${NEUTRON_URL}/v2.0/security-groups" "[.security_groups[] | select(.name == \"default\" and .project_id == \"${PROJECT_ID}\")][0].id // empty")
    [ -n "$flavor" ] && [ -n "$sg" ]
    check "the wizard's choices exist: flavor m1.tiny and the project's default security group" $?

    print_test "POST /v2.1/servers with the wizard's body"
    if expect_send "launch" 202 POST "${NOVA_URL}/v2.1/servers" '{"server": {
            "name": "'"${GATE_TAG}"'-vm", "imageRef": "", "flavorRef": "'"${flavor}"'",
            "min_count": 1, "max_count": 1, "OS-DCF:diskConfig": "AUTO",
            "security_groups": [{"name": "'"${sg}"'"}],
            "networks": [{"uuid": "'"${GATE_NET}"'"}],
            "block_device_mapping_v2": [{"source_type": "image", "destination_type": "volume",
                "delete_on_termination": false, "uuid": "'"${GATE_IMAGE}"'", "boot_index": "0",
                "volume_type": "__DEFAULT__", "volume_size": 1}]}}'; then
        GATE_SERVER=$(echo "$BODY" | jq -r '.server.id // empty')
    fi
    [ -n "$GATE_SERVER" ] || { print_fail "no server id in the launch response"; return; }

    print_test "ACTIVE, listed in the Instances panel's list, with an address on the network"
    wait_for "${NOVA_URL}/v2.1/servers/${GATE_SERVER}" '.server.status' ACTIVE
    check "server reaches ACTIVE" $?
    local listed
    listed=$(jqval "${NOVA_URL}/v2.1/servers/detail" "[.servers[] | select(.id == \"${GATE_SERVER}\")]")
    [ "$(echo "$listed" | jq 'length')" = "1" ]
    check "listed once in GET /v2.1/servers/detail" $?
    echo "$listed" | jq -e --arg n "${GATE_TAG}-net" '.[0].addresses[$n] | length > 0' > /dev/null
    check "an address on ${GATE_TAG}-net" $?

    print_test "it boots from the volume made for it, attached at /dev/vda"
    GATE_BOOT_VOLUME=$(jqval "${NOVA_URL}/v2.1/servers/${GATE_SERVER}" '.server["os-extended-volumes:volumes_attached"][0].id // empty')
    [ -n "$GATE_BOOT_VOLUME" ]
    check "volumes_attached lists the boot volume" $?
    if [ -n "$GATE_BOOT_VOLUME" ]; then
        get "${CINDER_URL}/v3/volumes/${GATE_BOOT_VOLUME}"
        [ "$(echo "$BODY" | jq -r '[.volume.status, .volume.attachments[0].device, .volume.attachments[0].server_id] | join(" ")')" = "in-use /dev/vda ${GATE_SERVER}" ]
        check "the boot volume is in-use, attached to the server at /dev/vda" $?
    fi

    print_test "delete it; the boot volume outlives it, released, and is deleted after"
    expect_send "delete server" 2xx DELETE "${NOVA_URL}/v2.1/servers/${GATE_SERVER}" && GATE_SERVER=""
    if [ -n "$GATE_BOOT_VOLUME" ]; then
        wait_for "${CINDER_URL}/v3/volumes/${GATE_BOOT_VOLUME}" '.volume.status' available
        check "the boot volume is released" $?
        expect_send "delete the boot volume" 2xx DELETE "${CINDER_URL}/v3/volumes/${GATE_BOOT_VOLUME}" && GATE_BOOT_VOLUME=""
    fi
    expect_send "delete subnet" 2xx DELETE "${NEUTRON_URL}/v2.0/subnets/${GATE_SUBNET}" && GATE_SUBNET=""
    expect_send "delete network" 2xx DELETE "${NEUTRON_URL}/v2.0/networks/${GATE_NET}" && GATE_NET=""
    expect_send "delete image" 2xx DELETE "${GLANCE_URL}/v2/images/${GATE_IMAGE}" && GATE_IMAGE=""
}

test_overview_after_project_switch() {
    print_head "SC-004 - Overview limits after a project switch"

    print_test "a second project the user holds a role in"
    local domain role
    domain=$(jqval "${KEYSTONE_URL}/v3/domains" '[.domains[] | select(.name == "Default")][0].id // empty')
    if expect_send "create project" 201 POST "${KEYSTONE_URL}/v3/projects" \
        '{"project": {"name": "'"${GATE_TAG}"'", "domain_id": "'"${domain}"'", "enabled": true}}'; then
        GATE_PROJECT=$(echo "$BODY" | jq -r '.project.id // empty')
    fi
    [ -n "$GATE_PROJECT" ] || { print_fail "no project id in the create response"; return; }
    role=$(jqval "${KEYSTONE_URL}/v3/roles" '[.roles[] | select(.name == "member")][0].id // empty')
    expect_send "assign the member role" 204 PUT "${KEYSTONE_URL}/v3/projects/${GATE_PROJECT}/users/${USER_ID}/roles/${role}"

    print_test "switch: a token scoped to it reads the Overview's limits"
    local switched
    switched=$(curl -s -i -X POST "${KEYSTONE_URL}/v3/auth/tokens" -H "Content-Type: application/json" \
        -d '{"auth": {"identity": {"methods": ["password"], "password": {"user": {
                "name": "'"$ADMIN_USER"'", "domain": {"name": "Default"}, "password": "'"$ADMIN_PASSWORD"'"}}},
              "scope": {"project": {"id": "'"${GATE_PROJECT}"'"}}}}' |
        grep -i "^X-Subject-Token:" | awk '{print $2}' | tr -d '\r')
    [ -n "$switched" ]
    check "token scoped to ${GATE_TAG}" $?
    [ -n "$switched" ] || return

    local own="$TOKEN"
    TOKEN="$switched"
    expect_key "GET /v2.1/limits" "${NOVA_URL}/v2.1/limits" '.limits.absolute.maxTotalInstances'
    expect_key "GET /v3/limits" "${CINDER_URL}/v3/limits" '.limits.absolute.maxTotalVolumes'
    [ "$(jqval "${NEUTRON_URL}/v2.0/quotas/${GATE_PROJECT}" '.quota.project_id // empty')" = "$GATE_PROJECT" ]
    check "GET /v2.0/quotas/{project} answers for the switched-to project" $?
    TOKEN="$own"

    expect_send "delete project" 2xx DELETE "${KEYSTONE_URL}/v3/projects/${GATE_PROJECT}" && GATE_PROJECT=""
}

# ---------------------------------------------------------------- main

echo "=========================================="
echo " Lightstack Horizon create/write UI enablement"
echo " SPEC-020 / cloud-storage#533 (F2 F3 F7 F8)"
echo "=========================================="

authenticate
test_overview_limits
test_quota_variants
test_limits_agreement
test_capabilities
test_volume_create_metadata
test_network_selectability
test_client_paths
test_quota_target_authz
test_create_volume
test_create_network
test_launch_instance
test_overview_after_project_switch

echo ""
echo "=========================================="
echo -e " ${GREEN}Passed: ${PASSED}${NC}   ${RED}Failed: ${FAILED}${NC}"
echo "=========================================="

[ "$FAILED" -eq 0 ] || exit 1
