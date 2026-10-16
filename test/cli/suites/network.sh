#!/bin/bash
# network.sh — SPEC-028 tier A/B conformance cases for Neutron (network).
#
# One case per manifest/network.json entry: tier A uses the manifest id, tier B
# uses <id>.neg. Lifecycle entries cover the full CRUD of their resource in a
# single case (the volume.transfer.request precedent) so the standalone suite
# stays inside its runtime budget:
#
#     bash test/cli/suites/network.sh
#
# Every resource is named cli-conf-nt-* and removed by the case that created
# it, so the suite is rerun-safe when run standalone. Ports, subnet pools and
# floating IPs are addressed by ID everywhere: the first two resolve by name
# only through the CLI's list fallback (their by-name GETs answer 404, as
# upstream does — see the .byname probe cases), and floating IPs carry no
# name at all — every case that allocates one releases it before
# t_end, on all paths.
#
# Known-failing cases (t_known_failure) record today's product truth with the
# precise divergence in the reason string; see manifest/network.json. They are
# structured as self-healing probes: if the server is fixed the normal
# assertions take over again.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin network

# ---------------------------------------------------------------------------
# Quiet best-effort cleanup helpers. Rerun-safety: every case pre-cleans the
# exact names it is about to create; all of these tolerate "not found".
# ---------------------------------------------------------------------------

# _nt_show_id <show-args...> — run a <noun> show, print .id or nothing.
_nt_show_id() {
    OSC_QUIET=1 osc "$@"
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r '.id // empty'
}

# Resources whose by-name GET works (the handler resolves names).
_nt_net_id()    { _nt_show_id network show "$1"; }
_nt_subnet_id() { _nt_show_id subnet show "$1"; }
_nt_router_id() { _nt_show_id router show "$1"; }
_nt_sg_id()     { _nt_show_id security group show "$1"; }

# Ports and subnet pools resolve via the list commands: robust against the
# by-name GET answering 404 (as upstream does) and against display-cased
# list keys alike.
_nt_port_id() {
    OSC_QUIET=1 osc port list
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r --arg n "$1" \
        '.[] | select((.Name // .name) == $n) | (.ID // .id) // empty' | head -1
}
_nt_pool_id() {
    OSC_QUIET=1 osc subnet pool list
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r --arg n "$1" \
        '.[] | select((.Name // .name) == $n) | (.ID // .id) // empty' | head -1
}

_nt_clean_sg() {
    local id
    id=$(_nt_sg_id "$1")
    [[ -n "$id" ]] && OSC_QUIET=1 osc security group delete "$id"
}

_nt_clean_pool() {
    local id
    id=$(_nt_pool_id "$1")
    [[ -n "$id" ]] && OSC_QUIET=1 osc subnet pool delete "$id"
}

# _nt_clean_router <name-or-id> — detach every interface (by port id, so it
# works even when the owning subnet is already gone), then delete the router.
_nt_clean_router() {
    local rid p
    rid=$(_nt_router_id "$1")
    [[ -z "$rid" ]] && return 0
    OSC_QUIET=1 osc router show "$rid"
    if (( OSC_RC == 0 )); then
        while IFS= read -r p; do
            [[ -n "$p" ]] && OSC_QUIET=1 osc router remove port "$rid" "$p"
        done < <(printf '%s' "$OSC_OUT" | jq -r '.interfaces_info // [] | .[].port_id')
    fi
    OSC_QUIET=1 osc router delete "$rid"
}

# _nt_clean_network <name-or-id> — delete leftover ports on the network
# (including the nameless router-interface-* ports a failed interface add can
# leak), then its subnets, then the network itself.
_nt_clean_network() {
    local nid id
    nid=$(_nt_net_id "$1")
    [[ -z "$nid" ]] && return 0
    OSC_QUIET=1 osc port list --network "$nid"
    if (( OSC_RC == 0 )); then
        while IFS= read -r id; do
            [[ -n "$id" ]] && OSC_QUIET=1 osc port delete "$id"
        done < <(printf '%s' "$OSC_OUT" | jq -r '.[] | (.ID // .id)')
    fi
    OSC_QUIET=1 osc subnet list --network "$nid"
    if (( OSC_RC == 0 )); then
        while IFS= read -r id; do
            [[ -n "$id" ]] && OSC_QUIET=1 osc subnet delete "$id"
        done < <(printf '%s' "$OSC_OUT" | jq -r '.[] | (.ID // .id)')
    fi
    OSC_QUIET=1 osc network delete "$nid"
}

# ===========================================================================
# Networks
# ===========================================================================

t_begin network.network.create
# Lifecycle entry: create + list + delete of the created network.
N="cli-conf-nt-netcr"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$N"
assert_jq_type '.id' string
assert_jq '.status' 'ACTIVE'
assert_jq '.admin_state_up' true
assert_jq_type '.subnets' array
assert_jq '.subnets | length' 0
assert_jq '.shared' false
assert_jq '.["router:external"]' false
assert_jq_type '.project_id' string
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$NID" ]] || t_fail "no network id in create output"
osc network list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$N\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$N\")][0] | (.ID // .id) | type" 'string'
assert_jq "[.[] | select((.Name // .name) == \"$N\")][0].Subnets | type" 'array'
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.network.create.external
N="cli-conf-nt-netext"
_nt_clean_network "$N"
osc network create --external "$N"
assert_rc 0
assert_jq '.name' "$N"
assert_jq '.["router:external"]' true
assert_jq_type '.id' string
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$NID" ]] || t_fail "no network id in create output"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.network.show
N="cli-conf-nt-netshw"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
osc network show "$N"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$N"
assert_jq '.status' 'ACTIVE'
assert_jq '.admin_state_up' true
assert_jq_type '.id' string
assert_jq_type '.subnets' array
assert_jq_type '.project_id' string
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$NID" ]] && OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.network.show.neg
osc network show cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent network must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin network.network.set
N="cli-conf-nt-netset"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc network set --name "$N-rn" --disable "$N"
assert_rc 0
osc network show "$N-rn"
assert_rc 0
assert_jq '.name' "$N-rn"
assert_jq '.admin_state_up' false
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.network.set.description
N="cli-conf-nt-netdesc"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc network set --description "set by cli-conf network suite" "$N"
SETRC=$OSC_RC
SETERR=$(_snip "$OSC_ERR")
osc network show "$N"
assert_rc 0
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.description // empty')" != "set by cli-conf network suite" ]]; then
    t_known_failure "PUT /v2.0/networks/{id} binds only name/admin_state_up/router:external (internal/neutron/network.go UpdateNetwork), so 'network set --description' answers HTTP 400 'no updates provided' (rc=$SETRC: $SETERR) instead of persisting — real Neutron updates description (and shared) here"
else
    assert_jq '.description' 'set by cli-conf network suite'
fi
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.network.delete
N="cli-conf-nt-netdel"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
osc network delete "$N"
assert_rc 0
osc network show "$N"
if (( OSC_RC == 0 )); then
    t_fail "network still visible after delete"
fi
t_end

t_begin network.network.delete.neg
# unknown id: must be refused
osc network delete cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent network must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
# network in use (a port on it): must be refused with a conflict
N="cli-conf-nt-netdelneg"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$N-sub" --network "$N" --subnet-range 10.121.0.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port create "$N-port" --network "$N"
assert_rc 0
PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc network delete "$N"
if (( OSC_RC == 0 )); then
    t_fail "delete of a network with a port on it must not exit 0"
else
    printf '%s' "$OSC_ERR" | grep -q "409" || t_fail "expected a 409 conflict for an in-use network, stderr: $(_snip "$OSC_ERR")"
fi
# teardown in dependency order: port, subnet, then the network
OSC_QUIET=1 osc port delete "$PID"
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$NID"
t_end

# ===========================================================================
# Subnets
# ===========================================================================

t_begin network.subnet.create
# Lifecycle entry: create + list + show + delete of the created subnet.
N="cli-conf-nt-subcr-net"
S="cli-conf-nt-subcr"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.101.0.0/24
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$S"
assert_jq '.cidr' '10.101.0.0/24'
assert_jq '.ip_version' 4
assert_jq '.enable_dhcp' true
assert_jq '.gateway_ip' '10.101.0.1'
assert_jq_type '.id' string
assert_jq_type '.allocation_pools' array
assert_jq '.allocation_pools | length' 1
assert_jq '.network_id' "$NID"
osc subnet list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$S\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Subnet" '10.101.0.0/24'
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Network" "$NID"
osc subnet show "$S"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$S"
assert_jq '.cidr' '10.101.0.0/24'
osc subnet delete "$S"
assert_rc 0
osc subnet show "$S"
if (( OSC_RC == 0 )); then
    t_fail "subnet still visible after delete"
fi
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.subnet.create.neg
# unknown network: the CLI must refuse
osc subnet create cli-conf-nt-subbad --network cli-conf-nt-does-not-exist --subnet-range 10.102.0.0/24
if (( OSC_RC == 0 )); then
    t_fail "subnet create on a nonexistent network must not exit 0"
    SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
    [[ -n "$SUBID" ]] && OSC_QUIET=1 osc subnet delete "$SUBID"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
# malformed CIDR: the server must refuse (400 invalid CIDR)
N="cli-conf-nt-subneg"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create cli-conf-nt-subbad --network "$N" --subnet-range not-a-cidr
if (( OSC_RC == 0 )); then
    t_fail "subnet create with a malformed CIDR must not exit 0"
    SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
    [[ -n "$SUBID" ]] && OSC_QUIET=1 osc subnet delete "$SUBID"
else
    assert_err_contains 'invalid CIDR'
fi
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.subnet.set
N="cli-conf-nt-subset-net"
S="cli-conf-nt-subset"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.104.0.0/24
assert_rc 0
osc subnet set --name "$S-rn" "$S"
assert_rc 0
osc subnet set --gateway 10.104.0.254 "$S-rn"
assert_rc 0
osc subnet show "$S-rn"
assert_rc 0
assert_jq '.name' "$S-rn"
assert_jq '.gateway_ip' '10.104.0.254'
OSC_QUIET=1 osc subnet delete "$S-rn"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.subnet.set.dhcp
N="cli-conf-nt-subdhcp-net"
S="cli-conf-nt-subdhcp"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.105.0.0/24
assert_rc 0
osc subnet set --no-dhcp "$S"
assert_rc 0
osc subnet show "$S"
assert_rc 0
# tostring, not '// empty': jq's alternative operator treats false as falsy,
# so '.enable_dhcp // empty' prints nothing exactly when the flag is off —
# the one answer this probe exists to detect.
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.enable_dhcp | tostring')" != "false" ]]; then
    t_known_failure "PUT /v2.0/subnets/{id} binds only name and gateway_ip (internal/neutron/network.go UpdateSubnet), so 'subnet set --no-dhcp' answers 200 but persists nothing — GET /v2.0/subnets/{id} still reports enable_dhcp: true, where real Neutron flips the flag (same for --dhcp, --dns-nameserver, --host-route, --description)"
else
    assert_jq '.enable_dhcp' false
    osc subnet set --dhcp "$S"
    assert_rc 0
    osc subnet show "$S"
    assert_rc 0
    assert_jq '.enable_dhcp' true
fi
OSC_QUIET=1 osc subnet delete "$S"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.subnet.delete.neg
# A subnet held by a router interface must not be deletable (Neutron: 409).
N="cli-conf-nt-subdelneg"
S="cli-conf-nt-subdelneg-sub"
R="cli-conf-nt-subdelneg-rt"
_nt_clean_router "$R"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.107.0.0/24
assert_rc 0
osc router create "$R"
assert_rc 0
osc router add subnet "$R" "$S"
assert_rc 0
osc router show "$R"
assert_rc 0
assert_jq '.interfaces_info | length' 1
IFPORT=$(printf '%s' "$OSC_OUT" | jq -r '.interfaces_info[0].port_id // empty')
osc subnet delete "$S"
if (( OSC_RC == 0 )); then
    t_known_failure "DELETE /v2.0/subnets/{id} succeeds while a router interface still holds the subnet — real Neutron refuses with 409 (SubnetInUse) — and leaves the interface port $IFPORT orphaned on the network, where it blocks network deletion with 409 'Cannot delete network' (the case sweeps it up by hand)"
    # the interface record now dangles (remove-by-port 404s), so take the
    # orphaned port out directly before removing the router and network
    [[ -n "$IFPORT" ]] && OSC_QUIET=1 osc port delete "$IFPORT"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
    osc router remove subnet "$R" "$S"
    assert_rc 0
    osc subnet delete "$S"
    assert_rc 0
fi
_nt_clean_router "$R"
_nt_clean_network "$N"
t_end

# ===========================================================================
# Ports
# ===========================================================================

t_begin network.port.create
# Lifecycle entry: create + list + show + delete of the created port.
N="cli-conf-nt-portcr-net"
P="cli-conf-nt-portcr"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$N-sub" --network "$N" --subnet-range 10.108.0.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port create "$P" --network "$N"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$P"
assert_jq_type '.id' string
assert_jq '.network_id' "$NID"
assert_jq '.status' 'ACTIVE'
assert_jq '.admin_state_up' true
assert_jq_type '.mac_address' string
assert_jq_type '.fixed_ips' array
assert_jq '.fixed_ips | length' 1
assert_jq '.fixed_ips[0].subnet_id' "$SUBID"
assert_jq_type '.fixed_ips[0].ip_address' string
PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$P\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$P\")][0] | (.ID // .id) | type" 'string'
assert_jq "[.[] | select((.Name // .name) == \"$P\")][0][\"MAC Address\"] | type" 'string'
assert_jq "[.[] | select((.Name // .name) == \"$P\")][0][\"Fixed IP Addresses\"] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$P\")][0].Status" 'ACTIVE'
osc port show "$PID"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$P"
assert_jq '.status' 'ACTIVE'
osc port delete "$PID"
assert_rc 0
osc port show "$PID"
if (( OSC_RC == 0 )); then
    t_fail "port still visible after delete"
fi
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.port.create.neg
# unknown network: the CLI must refuse
osc port create cli-conf-nt-portbad --network cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "port create on a nonexistent network must not exit 0"
    PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
    [[ -n "$PID" ]] && OSC_QUIET=1 osc port delete "$PID"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
# unknown port: show must refuse
osc port show cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent port must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin network.port.show.byname
N="cli-conf-nt-portname-net"
P="cli-conf-nt-portname"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port create "$P" --network "$N"
assert_rc 0
PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port show "$P" # by NAME
if (( OSC_RC != 0 )); then
    t_known_failure "GET /v2.0/ports/{name-or-non-uuid} answers HTTP 500 'failed to get port' (internal/neutron/ports.go GetPort casts the path segment to the uuid column p.id) instead of 404 — openstacksdk's find() only falls back to a name-filtered list on 404/400/403, so 'port show/set/delete <name>' all fail against lightstack while they resolve the name on real Neutron (by-ID forms work; evidence: $(_snip "$OSC_ERR"))"
else
    assert_rc 0
    assert_jq '.name' "$P"
fi
OSC_QUIET=1 osc port delete "$PID"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.port.set
N="cli-conf-nt-portset-net"
P="cli-conf-nt-portset"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port create "$P" --network "$N"
assert_rc 0
PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc port set --name "$P-rn" "$PID"
assert_rc 0
osc port show "$PID"
assert_rc 0
assert_jq '.name' "$P-rn"
OSC_QUIET=1 osc port delete "$PID"
OSC_QUIET=1 osc network delete "$NID"
t_end

# ===========================================================================
# Routers
# ===========================================================================

t_begin network.router.create
# Lifecycle entry: create + list + show + delete of the created router.
R="cli-conf-nt-rtcr"
_nt_clean_router "$R"
osc router create "$R"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$R"
assert_jq_type '.id' string
assert_jq '.status' 'ACTIVE'
assert_jq '.admin_state_up' true
assert_jq '(.external_gateway_info == null)' true
assert_jq_type '.project_id' string
RID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$RID" ]] || t_fail "no router id in create output"
osc router list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$R\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$R\")][0].Status" 'ACTIVE'
assert_jq "[.[] | select((.Name // .name) == \"$R\")][0].State" true
osc router show "$R"
assert_rc 0
assert_jq_type '.' object
assert_jq_type '.interfaces_info' array
osc router delete "$R"
assert_rc 0
osc router show "$R"
if (( OSC_RC == 0 )); then
    t_fail "router still visible after delete"
fi
t_end

t_begin network.router.create.neg
# unknown router: show and delete must refuse
osc router show cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent router must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
osc router delete cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent router must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin network.router.set
R="cli-conf-nt-rtset"
_nt_clean_router "$R"
osc router create "$R"
assert_rc 0
osc router set --name "$R-rn" "$R"
assert_rc 0
osc router show "$R-rn"
assert_rc 0
assert_jq '.name' "$R-rn"
_nt_clean_router "$R-rn"
t_end

t_begin network.router.addsubnet
# Covers both directions: router add subnet + router remove subnet.
N="cli-conf-nt-rtsub-net"
S="cli-conf-nt-rtsub-sub"
R="cli-conf-nt-rtsub"
_nt_clean_router "$R"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.114.0.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc router create "$R"
assert_rc 0
osc router add subnet "$R" "$S"
assert_rc 0
osc router show "$R"
assert_rc 0
assert_jq_type '.interfaces_info' array
assert_jq '.interfaces_info | length' 1
assert_jq '.interfaces_info[0].subnet_id' "$SUBID"
assert_jq '.interfaces_info[0].ip_address' '10.114.0.1'
assert_jq_type '.interfaces_info[0].port_id' string
osc router remove subnet "$R" "$S"
assert_rc 0
osc router show "$R"
assert_rc 0
assert_jq '.interfaces_info | length' 0
OSC_QUIET=1 osc router delete "$R"
OSC_QUIET=1 osc subnet delete "$S"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.router.addsubnet.neg
# adding the same subnet twice must be refused (Neutron: 409)
N="cli-conf-nt-rtdup-net"
S="cli-conf-nt-rtdup-sub"
R="cli-conf-nt-rtdup"
_nt_clean_router "$R"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.115.0.0/24
assert_rc 0
osc router create "$R"
assert_rc 0
osc router add subnet "$R" "$S"
assert_rc 0
osc router add subnet "$R" "$S"
if (( OSC_RC == 0 )); then
    t_fail "adding the same subnet to a router twice must not exit 0"
elif printf '%s' "$OSC_ERR" | grep -q "409"; then
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
else
    t_known_failure "PUT /v2.0/routers/{id}/add_router_interface for an already-attached subnet answers HTTP 500 'failed to create router interface record' (unique violation on router_interfaces) where real Neutron answers 409 Conflict — worse, the failed call has already inserted the interface port, so it is orphaned on the network and later blocks network deletion with 409 (the case sweeps it up; evidence: $(_snip "$OSC_ERR"))"
fi
# cleanup: detach the one real interface, then sweep any port the failed
# call leaked before removing the subnet and network
_nt_clean_router "$R"
OSC_QUIET=1 osc port list --network "$NID"
if (( OSC_RC == 0 )); then
    while IFS= read -r p; do
        [[ -n "$p" ]] && OSC_QUIET=1 osc port delete "$p"
    done < <(printf '%s' "$OSC_OUT" | jq -r '.[] | (.ID // .id)')
fi
OSC_QUIET=1 osc subnet delete "$S"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.router.addport
N="cli-conf-nt-rtap-net"
P="cli-conf-nt-rtap"
R="cli-conf-nt-rtap"
_nt_clean_router "$R"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$N-sub" --network "$N" --subnet-range 10.117.0.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc router create "$R"
assert_rc 0
osc port create "$P" --network "$N"
assert_rc 0
PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc router add port "$R" "$PID"
if (( OSC_RC != 0 )); then
    t_known_failure "PUT /v2.0/routers/{id}/add_router_interface ignores port_id — internal/neutron/router.go AddRouterInterface always resolves req.SubnetID (empty for 'router add port'), the uuid column cast fails and the server answers HTTP 500 'failed to get subnet'; real Neutron attaches the given port as the router interface (evidence: $(_snip "$OSC_ERR"))"
else
    osc router show "$R"
    assert_rc 0
    assert_jq '.interfaces_info | length' 1
    assert_jq '[.interfaces_info[] | select(.port_id == "'"$PID"'")] | length' 1
    osc router remove port "$R" "$PID"
    assert_rc 0
fi
# 'router remove port' deletes the interface port itself (upstream Neutron
# does the same, and so does this server) — no separate cleanup needed on the
# fixed path. The belt-and-braces delete below stays for the known-failure
# path, where the interface row never existed and the port is still ours.
OSC_QUIET=1 osc port delete "$PID"
_nt_clean_router "$R"
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.router.removeport
N="cli-conf-nt-rtrp-net"
S="cli-conf-nt-rtrp-sub"
R="cli-conf-nt-rtrp"
_nt_clean_router "$R"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$S" --network "$N" --subnet-range 10.118.0.0/24
assert_rc 0
osc router create "$R"
assert_rc 0
osc router add subnet "$R" "$S"
assert_rc 0
osc router show "$R"
assert_rc 0
IFPORT=$(printf '%s' "$OSC_OUT" | jq -r '.interfaces_info[0].port_id // empty')
[[ -n "$IFPORT" ]] || t_fail "no interface port in router show output"
if [[ -n "$IFPORT" ]]; then
    osc router remove port "$R" "$IFPORT"
    assert_rc 0
    osc router show "$R"
    assert_rc 0
    assert_jq '.interfaces_info | length' 0
    # removing the interface takes its port with it
    osc port list --network "$NID"
    assert_rc 0
    assert_jq 'length' 0
fi
OSC_QUIET=1 osc router delete "$R"
OSC_QUIET=1 osc subnet delete "$S"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.router.setgateway
EXT="cli-conf-nt-rtgw-ext"
R="cli-conf-nt-rtgw"
_nt_clean_router "$R"
_nt_clean_network "$EXT"
osc network create --external "$EXT"
assert_rc 0
EXTID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$EXT-sub" --network "$EXT" --subnet-range 203.0.113.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc router create "$R"
assert_rc 0
osc router set --external-gateway "$EXT" "$R"
assert_rc 0
osc router show "$R"
assert_rc 0
assert_jq '.external_gateway_info.network_id' "$EXTID"
osc router unset --external-gateway "$R"
assert_rc 0
osc router show "$R"
assert_rc 0
assert_jq '(.external_gateway_info == null)' true
OSC_QUIET=1 osc router delete "$R"
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$EXTID"
t_end

# ===========================================================================
# Security groups
# ===========================================================================

t_begin network.securitygroup.create
# Lifecycle entry: create + list + show + delete of the created group.
SG="cli-conf-nt-sgcr"
SG2="cli-conf-nt-sgcr-tbl"
_nt_clean_sg "$SG"
_nt_clean_sg "$SG2"
osc security group create "$SG"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$SG"
assert_jq '.description' "$SG" # the CLI sends the name as default description
assert_jq_type '.id' string
assert_jq_type '.project_id' string
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.rules | type')" != "array" ]]; then
    # Live evidence of the user impact: with OSC's default table formatter the
    # same create crashes outright ('NoneType' object is not iterable).
    osc security group create "$SG2" -f table
    TBLRC=$OSC_RC
    OSC_QUIET=1 osc security group delete "$SG2"
    t_known_failure "POST /v2.0/security-groups returns the created group without security_group_rules (OSC reports rules: null; GET /v2.0/security-groups/{id} correctly returns []) where real Neutron returns [] — with OSC's default table output 'security group create' therefore crashes with \"'NoneType' object is not iterable\" (table run rc=$TBLRC)"
else
    assert_jq_type '.rules' 'array'
fi
osc security group list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$SG\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$SG\")][0].Description" "$SG"
osc security group show "$SG"
assert_rc 0
assert_jq_type '.' object
assert_jq_type '.rules' array
osc security group delete "$SG"
assert_rc 0
osc security group list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$SG\")] | length" 0
t_end

t_begin network.securitygroup.create.neg
# unknown group: show and delete must refuse
osc security group show cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent security group must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
osc security group delete cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent security group must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin network.securitygroup.set
SG="cli-conf-nt-sgset"
_nt_clean_sg "$SG"
osc security group create "$SG"
assert_rc 0
osc security group set --description "set by cli-conf network suite" "$SG"
assert_rc 0
osc security group show "$SG"
assert_rc 0
assert_jq '.description' 'set by cli-conf network suite'
_nt_clean_sg "$SG"
t_end

# ===========================================================================
# Security group rules
# ===========================================================================

t_begin network.securitygrouprule.create
# Lifecycle entry: create + list + delete of the created rule.
SG="cli-conf-nt-rulecr"
_nt_clean_sg "$SG"
osc security group create "$SG"
assert_rc 0
SGID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc security group rule create --proto tcp --dst-port 22 "$SG"
assert_rc 0
assert_jq_type '.' object
assert_jq '.direction' 'ingress'
assert_jq '.protocol' 'tcp'
assert_jq '.port_range_min' 22
assert_jq '.port_range_max' 22
assert_jq '.ether_type' 'IPv4'
assert_jq '.remote_ip_prefix' '0.0.0.0/0'
assert_jq '.security_group_id' "$SGID"
assert_jq_type '.id' string
RULEID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$RULEID" ]] || t_fail "no rule id in create output"
osc security group rule list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.\"Security Group\" // .security_group_id) == \"$SGID\")] | length" 1
assert_jq "[.[] | select((.\"Security Group\" // .security_group_id) == \"$SGID\")][0].\"IP Protocol\"" 'tcp'
assert_jq "[.[] | select((.\"Security Group\" // .security_group_id) == \"$SGID\")][0].\"Port Range\"" '22:22'
assert_jq "[.[] | select((.\"Security Group\" // .security_group_id) == \"$SGID\")][0].Direction" 'ingress'
osc security group rule delete "$RULEID"
assert_rc 0
osc security group rule list
assert_rc 0
assert_jq "[.[] | select((.ID // .id) == \"$RULEID\")] | length" 0
_nt_clean_sg "$SG"
t_end

t_begin network.securitygrouprule.create.neg
# unknown group: the CLI must refuse
osc security group rule create --proto tcp --dst-port 22 cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "rule create on a nonexistent security group must not exit 0"
    RULEID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
    [[ -n "$RULEID" ]] && OSC_QUIET=1 osc security group rule delete "$RULEID"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
# unknown rule: delete must refuse
osc security group rule delete 00000000-0000-0000-0000-000000000000
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent rule must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

# ===========================================================================
# Floating IPs (nameless — every allocating case releases before t_end)
# ===========================================================================

t_begin network.floatingip.create
# Lifecycle entry: create + list + delete of the allocation.
EXT="cli-conf-nt-fipcr-ext"
_nt_clean_network "$EXT"
osc network create --external "$EXT"
assert_rc 0
EXTID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$EXT-sub" --network "$EXT" --subnet-range 198.51.100.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc floating ip create "$EXT"
assert_rc 0
assert_jq_type '.' object
assert_jq_type '.id' string
assert_jq '.floating_ip_address | startswith("198.51.100.")' true
assert_jq '.floating_network_id' "$EXTID"
assert_jq '.status' 'DOWN'
assert_jq '(.port_id == null)' true
assert_jq '(.router_id == null)' true
assert_jq_type '.project_id' string
FIPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$FIPID" ]] || t_fail "no floating ip id in create output"
FIPADDR=$(printf '%s' "$OSC_OUT" | jq -r '.floating_ip_address // empty')
osc floating ip list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")] | length" 1
assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")][0][\"Floating IP Address\"]" "$FIPADDR"
assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")][0][\"Floating Network\"]" "$EXTID"
# release the allocation on every path — a floating ip must never leak
[[ -n "$FIPID" ]] && OSC_QUIET=1 osc floating ip delete "$FIPID"
osc floating ip list
assert_rc 0
assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")] | length" 0
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$EXTID"
t_end

t_begin network.floatingip.create.neg
# a floating IP may only come from an external network
N="cli-conf-nt-fipneg"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$N-sub" --network "$N" --subnet-range 10.122.0.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc floating ip create "$N"
if (( OSC_RC == 0 )); then
    t_fail "floating ip create on an internal network must not exit 0"
    # belt and braces: release an allocation the server should have refused
    FIPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
    [[ -n "$FIPID" ]] && OSC_QUIET=1 osc floating ip delete "$FIPID"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.floatingip.set
EXT="cli-conf-nt-fipset-ext"
N="cli-conf-nt-fipset"
R="cli-conf-nt-fipset-rt"
P="cli-conf-nt-fipset-port"
FIPID=""
PID=""
_nt_clean_router "$R"
_nt_clean_network "$N"
_nt_clean_network "$EXT"
osc network create --external "$EXT"
assert_rc 0
EXTID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$EXT-sub" --network "$EXT" --subnet-range 203.0.113.0/24
assert_rc 0
EXTSUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet create "$N-sub" --network "$N" --subnet-range 10.120.0.0/24
assert_rc 0
SUBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc router create "$R"
assert_rc 0
osc router add subnet "$R" "$N-sub"
assert_rc 0
osc router set --external-gateway "$EXT" "$R"
assert_rc 0
osc port create "$P" --network "$N"
assert_rc 0
PID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc floating ip create "$EXT"
assert_rc 0
FIPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$FIPID" ]] || t_fail "no floating ip id in create output"
if [[ -n "$FIPID" && -n "$PID" ]]; then
    osc floating ip set --port "$PID" "$FIPID"
    assert_rc 0
    osc floating ip list
    assert_rc 0
    assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")][0].Port" "$PID"
    assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")][0][\"Fixed IP Address\"] | type" 'string'
    osc floating ip unset --port "$FIPID"
    assert_rc 0
    osc floating ip list
    assert_rc 0
    assert_jq "[.[] | select((.ID // .id) == \"$FIPID\")][0].Port" 'null'
fi
# release the allocation on every path — a floating ip must never leak
[[ -n "$FIPID" ]] && OSC_QUIET=1 osc floating ip delete "$FIPID"
[[ -n "$PID" ]] && OSC_QUIET=1 osc port delete "$PID"
_nt_clean_router "$R"
OSC_QUIET=1 osc subnet delete "$SUBID"
OSC_QUIET=1 osc network delete "$NID"
OSC_QUIET=1 osc subnet delete "$EXTSUBID"
OSC_QUIET=1 osc network delete "$EXTID"
t_end

# ===========================================================================
# Agents (admin, read-only)
# ===========================================================================

t_begin network.agent.list
# This deployment registers no agents, so the contract here is the command's
# health and shape, not a non-empty list.
osc network agent list
assert_rc 0
assert_jq_type '.' array
t_end

# ===========================================================================
# Subnet pools
# ===========================================================================

t_begin network.subnetpool.create
# Lifecycle entry: create + list + show + delete of the created pool.
SP="cli-conf-nt-poolcr"
_nt_clean_pool "$SP"
osc subnet pool create "$SP" --pool-prefix 10.108.0.0/16 --default-prefix-length 24
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$SP"
assert_jq '.prefixes == ["10.108.0.0/16"]' true
assert_jq '.default_prefixlen' 24
assert_jq '.ip_version' 4
assert_jq '.is_default' false
assert_jq '.shared' false
assert_jq_type '.id' string
SPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$SPID" ]] || t_fail "no subnet pool id in create output"
osc subnet pool list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$SP\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$SP\")][0].\"Prefixes\" == [\"10.108.0.0/16\"]" true
assert_jq "[.[] | select((.Name // .name) == \"$SP\")][0] | (.ID // .id) | type" 'string'
osc subnet pool show "$SPID"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$SP"
assert_jq '.prefixes | length' 1
assert_jq '.default_prefixlen' 24
osc subnet pool delete "$SPID"
assert_rc 0
osc subnet pool list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$SP\")] | length" 0
t_end

t_begin network.subnetpool.create.neg
# unknown pool: show and delete must refuse. The by-name GET answers 404
# (the uuid guard; see network.subnetpool.show.byname) — either way the
# command must not exit 0.
osc subnet pool show cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent subnet pool must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
osc subnet pool delete cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent subnet pool must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin network.subnetpool.show.byname
SP="cli-conf-nt-poolname"
_nt_clean_pool "$SP"
osc subnet pool create "$SP" --pool-prefix 10.111.0.0/16
assert_rc 0
SPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc subnet pool show "$SP" # by NAME
if (( OSC_RC != 0 )); then
    t_known_failure "GET /v2.0/subnetpools/{name-or-non-uuid} answers HTTP 500 'failed to get subnet pool' (internal/neutron/subnet_pools.go GetSubnetPool casts the path segment to the uuid column) instead of 404 — openstacksdk's find() only falls back to a name-filtered list on 404/400/403, so 'subnet pool show/set/delete <name>' all fail against lightstack while they resolve the name on real Neutron (by-ID forms work; evidence: $(_snip "$OSC_ERR"))"
else
    assert_rc 0
    assert_jq '.name' "$SP"
fi
OSC_QUIET=1 osc subnet pool delete "$SPID"
t_end

t_begin network.subnetpool.set
SP="cli-conf-nt-poolset"
_nt_clean_pool "$SP"
osc subnet pool create "$SP" --pool-prefix 10.112.0.0/16
assert_rc 0
SPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$SPID" ]] || t_fail "no subnet pool id in create output"
osc subnet pool set --name "$SP-rn" "$SPID"
assert_rc 0
osc subnet pool show "$SPID"
assert_rc 0
assert_jq '.name' "$SP-rn"
OSC_QUIET=1 osc subnet pool delete "$SPID"
t_end

t_begin network.subnetpool.set.description
SP="cli-conf-nt-pooldesc"
_nt_clean_pool "$SP"
osc subnet pool create "$SP" --pool-prefix 10.113.0.0/16
assert_rc 0
SPID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$SPID" ]] || t_fail "no subnet pool id in create output"
osc subnet pool set --description "set by cli-conf network suite" "$SPID"
SETRC=$OSC_RC
SETERR=$(_snip "$OSC_ERR")
osc subnet pool show "$SPID"
assert_rc 0
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.description // empty')" != "set by cli-conf network suite" ]]; then
    t_known_failure "PUT /v2.0/subnetpools/{id} binds only name/prefixes/prefix lengths (internal/neutron/subnet_pools.go UpdateSubnetPool), so 'subnet pool set --description' answers HTTP 400 'no updates provided' (rc=$SETRC: $SETERR) instead of persisting — real Neutron updates the description here"
else
    assert_jq '.description' 'set by cli-conf network suite'
fi
OSC_QUIET=1 osc subnet pool delete "$SPID"
t_end

# ===========================================================================
# RBAC policies ('network rbac set' is excluded — see manifest/network.json)
# ===========================================================================

t_begin network.rbac.create
# Lifecycle entry: create + list + show + delete of the policy.
N="cli-conf-nt-rbcr-net"
_nt_clean_network "$N"
osc network create "$N"
assert_rc 0
NID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc network rbac create --type network --target-project default --action access_as_shared "$N"
assert_rc 0
assert_jq_type '.' object
assert_jq '.object_type' 'network'
assert_jq '.action' 'access_as_shared'
assert_jq '.object_id' "$NID"
assert_jq_type '.target_project_id' string
assert_jq_type '.id' string
RBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$RBID" ]] || t_fail "no rbac policy id in create output"
osc network rbac list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.\"Object ID\" // .object_id) == \"$NID\")] | length" 1
assert_jq "[.[] | select((.\"Object ID\" // .object_id) == \"$NID\")][0].\"Object Type\"" 'network'
osc network rbac show "$RBID"
assert_rc 0
assert_jq_type '.' object
assert_jq '.object_type' 'network'
assert_jq '.action' 'access_as_shared'
assert_jq '.object_id' "$NID"
osc network rbac delete "$RBID"
assert_rc 0
osc network rbac list
assert_rc 0
assert_jq "[.[] | select((.ID // .id) == \"$RBID\")] | length" 0
OSC_QUIET=1 osc network delete "$NID"
t_end

t_begin network.rbac.create.neg
# unknown network: create must refuse
osc network rbac create --type network --target-project default --action access_as_shared cli-conf-nt-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "rbac create on a nonexistent network must not exit 0"
    RBID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
    [[ -n "$RBID" ]] && OSC_QUIET=1 osc network rbac delete "$RBID"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
# unknown policy: show and delete must refuse
osc network rbac show 00000000-0000-0000-0000-000000000000
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent rbac policy must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
osc network rbac delete 00000000-0000-0000-0000-000000000000
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent rbac policy must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

suite_end
