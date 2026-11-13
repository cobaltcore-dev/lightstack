#!/bin/bash
# workflows.sh — SPEC-028 tier C: cross-service scenario coverage.
#
# One case per scenario; the case id equals the manifest id verbatim
# (manifest/workflows.json). A failing step marks the case failed via t_fail
# but never aborts the scenario or the suite, so a single run reports every
# divergence with full context. Genuine product divergences are recorded with
# t_known_failure instead of a hard failure.
#
# Conventions (test/cli/README.md):
#   * every resource created here is named cli-conf-wf-* (sweepable prefix);
#   * floating IPs are nameless — each scenario that allocates one releases
#     it, and pre-clean releases any leaked on this suite's own networks;
#   * every scenario pre-deletes possible leftovers, so runs are rerun-safe;
#   * stub mode: servers go ACTIVE immediately and neither security-group
#     traffic nor router dataplane is enforced — API state is asserted only.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin workflows
# Standalone rerun-safety: this suite references the shared fixtures
# (cli-conf, cli-conf-image, cli-conf-net) by name. bin/run.sh ensures them
# and its teardown sweeps them, so running this suite alone after a sweep
# re-ensures here instead of failing with confusing missing-resource errors.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/fixtures.sh"
fixtures_require

# ---------------------------------------------------------------------------
# Suite-local helpers (the lib API is locked; additions stay in this file)
# ---------------------------------------------------------------------------

# Bounded wait: poll `openstack <kind> show <name>` until <jq-expr> equals
# <want>, at most <tries> seconds. Marks the case failed (with context) if
# the condition never holds. Set WF_WAIT_PROJECT to scope every poll to
# another project (resources created there are invisible to the default
# project's token).
wf_wait() {
    local kind="$1" name="$2" expr="$3" want="$4" tries="${5:-20}"
    local i=0 got=""
    for (( i = 0; i < tries; i++ )); do
        if [[ -n "${WF_WAIT_PROJECT:-}" ]]; then
            osc_as_project "$WF_WAIT_PROJECT" "$kind" show "$name"
        else
            osc "$kind" show "$name"
        fi
        if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
            got="$(printf '%s' "$OSC_OUT" | jq -r "$expr" 2>/dev/null)"
            if [[ "$got" == "$want" ]]; then
                return 0
            fi
        fi
        sleep 1
    done
    t_fail "wf_wait: $kind '$name' never reached $expr='$want' within ${tries}s (last observed: '${got:-none}')"
    return 1
}

# Run osc scoped to another project as the admin user (restores the env after).
osc_as_project() {
    local proj="$1"
    shift
    local saved="$OS_PROJECT_NAME"
    export OS_PROJECT_NAME="$proj"
    osc "$@"
    local rc=$OSC_RC
    export OS_PROJECT_NAME="$saved"
    return "$rc"
}

# Run osc as an arbitrary user scoped to a project (restores the env after).
osc_as_user() {
    local user="$1" pass="$2" proj="$3"
    shift 3
    local saved_user="$OS_USERNAME" saved_pass="$OS_PASSWORD" saved_project="$OS_PROJECT_NAME"
    export OS_USERNAME="$user" OS_PASSWORD="$pass" OS_PROJECT_NAME="$proj"
    osc "$@"
    local rc=$OSC_RC
    export OS_USERNAME="$saved_user" OS_PASSWORD="$saved_pass" OS_PROJECT_NAME="$saved_project"
    return "$rc"
}

# Extract a field from the current OSC_OUT (JSON) with a jq expression.
wf_jq() {
    printf '%s' "$OSC_OUT" | jq -r "$1" 2>/dev/null
}

# ===========================================================================
# 1. wf.boot.volume-and-network
#    Network + subnet, bootable volume (from the fixture image), boot a server
#    with --network and --block-device; verify ACTIVE, an address on the net,
#    and the volume attachment visible from both compute and volume views.
# ===========================================================================
t_begin wf.boot.volume-and-network

WF_BN_NET="cli-conf-wf-bn-net"
WF_BN_SUB="cli-conf-wf-bn-sub"
WF_BN_VOL="cli-conf-wf-bn-vol"
WF_BN_SRV="cli-conf-wf-bn-srv"

# Rerun-safety: reclaim leftovers from a crashed previous run (dependents
# first). Failures here are expected (404) and ignored.
osc server delete --wait "$WF_BN_SRV"
osc volume delete "$WF_BN_VOL"
osc subnet delete "$WF_BN_SUB"
osc network delete "$WF_BN_NET"

osc network create "$WF_BN_NET"
assert_rc 0
WF_BN_NET_ID="$(wf_jq '.id')"
assert_jq '.name' "$WF_BN_NET"

osc subnet create "$WF_BN_SUB" --network "$WF_BN_NET_ID" --subnet-range 10.98.0.0/24
assert_rc 0
assert_jq '.cidr' '10.98.0.0/24'

# A volume booted from must be bootable: create it from the fixture image.
osc volume create "$WF_BN_VOL" --size 1 --image cli-conf-image
assert_rc 0
WF_BN_VOL_ID="$(wf_jq '.id')"
assert_jq '.bootable' 'true'
wf_wait volume "$WF_BN_VOL_ID" '.status' 'available'

osc server create "$WF_BN_SRV" --flavor m1.tiny \
    --network "$WF_BN_NET_ID" \
    --block-device "uuid=$WF_BN_VOL_ID,source_type=volume,destination_type=volume,boot_index=0"
assert_rc 0
WF_BN_SRV_ID="$(wf_jq '.id')"
wf_wait server "$WF_BN_SRV_ID" '.status' 'ACTIVE'

osc server show "$WF_BN_SRV_ID"
assert_rc 0
assert_jq '.status' 'ACTIVE'
assert_jq '.addresses["cli-conf-wf-bn-net"] | length' 1
assert_jq '.addresses["cli-conf-wf-bn-net"][0] | startswith("10.98.")' 'true'
assert_jq '([.volumes_attached[].id] | index("'$WF_BN_VOL_ID'")) != null' 'true'

osc volume show "$WF_BN_VOL_ID"
assert_rc 0
assert_jq '.status' 'in-use'
assert_jq '.attachments | length' 1
assert_jq '.attachments[0].server_id' "$WF_BN_SRV_ID"
assert_jq '.attachments[0].volume_id' "$WF_BN_VOL_ID"

# Clean up within the scenario (dependents first).
osc server delete --wait "$WF_BN_SRV_ID"
assert_rc 0
osc volume delete "$WF_BN_VOL_ID"
assert_rc 0
osc subnet delete "$WF_BN_SUB"
assert_rc 0
osc network delete "$WF_BN_NET"
assert_rc 0

t_end

# ===========================================================================
# 2. wf.snapshot.to-image.to-server
#    Boot a server, snapshot it into an image (server image create), boot a
#    second server from that image; verify both ACTIVE; clean up.
# ===========================================================================
t_begin wf.snapshot.to-image.to-server

WF_SN_SRV1="cli-conf-wf-snap-srv1"
WF_SN_IMG="cli-conf-wf-snap-img"
WF_SN_SRV2="cli-conf-wf-snap-srv2"

osc server delete --wait "$WF_SN_SRV2"
osc server delete --wait "$WF_SN_SRV1"
osc image delete "$WF_SN_IMG"

osc server create "$WF_SN_SRV1" --flavor m1.tiny --image cli-conf-image --network cli-conf-net
assert_rc 0
WF_SN_SRV1_ID="$(wf_jq '.id')"
wf_wait server "$WF_SN_SRV1_ID" '.status' 'ACTIVE'

osc server image create "$WF_SN_SRV1" --name "$WF_SN_IMG"
assert_rc 0
WF_SN_IMG_ID="$(wf_jq '.id')"
assert_jq '.name' "$WF_SN_IMG"
wf_wait image "$WF_SN_IMG_ID" '.status' 'active'

osc image show "$WF_SN_IMG_ID"
assert_rc 0
assert_jq '.name' "$WF_SN_IMG"
assert_jq '.status' 'active'

# Boot a second server from the snapshot image.
osc server create "$WF_SN_SRV2" --flavor m1.tiny --image "$WF_SN_IMG_ID" --network cli-conf-net
assert_rc 0
WF_SN_SRV2_ID="$(wf_jq '.id')"
wf_wait server "$WF_SN_SRV2_ID" '.status' 'ACTIVE'

osc server show "$WF_SN_SRV2_ID"
assert_rc 0
assert_jq '.status' 'ACTIVE'
assert_jq '.image' "cli-conf-wf-snap-img ($WF_SN_IMG_ID)"
assert_jq '.addresses["cli-conf-net"] | length' 1

osc server delete --wait "$WF_SN_SRV2_ID"
assert_rc 0
osc server delete --wait "$WF_SN_SRV1_ID"
assert_rc 0
osc image delete "$WF_SN_IMG_ID"
assert_rc 0

t_end

# ===========================================================================
# 3. wf.volume.lifecycle.attach-detach
#    Volume create → server attach → verify attachment from both services →
#    detach → verify empty → delete → verify gone.
# ===========================================================================
t_begin wf.volume.lifecycle.attach-detach

WF_VA_SRV="cli-conf-wf-va-srv"
WF_VA_VOL="cli-conf-wf-va-vol"

osc server delete --wait "$WF_VA_SRV"
osc volume delete "$WF_VA_VOL"

osc server create "$WF_VA_SRV" --flavor m1.tiny --image cli-conf-image --network cli-conf-net
assert_rc 0
WF_VA_SRV_ID="$(wf_jq '.id')"
wf_wait server "$WF_VA_SRV_ID" '.status' 'ACTIVE'

osc volume create "$WF_VA_VOL" --size 1
assert_rc 0
WF_VA_VOL_ID="$(wf_jq '.id')"
wf_wait volume "$WF_VA_VOL_ID" '.status' 'available'

osc server add volume "$WF_VA_SRV" "$WF_VA_VOL_ID"
assert_rc 0

# Attachment visible from the volume side.
osc volume show "$WF_VA_VOL_ID"
assert_rc 0
assert_jq '.status' 'in-use'
assert_jq '.attachments | length' 1
assert_jq '.attachments[0].server_id' "$WF_VA_SRV_ID"

# And from the compute side.
osc server show "$WF_VA_SRV_ID"
assert_rc 0
assert_jq '([.volumes_attached[].id] | index("'$WF_VA_VOL_ID'")) != null' 'true'

osc server remove volume "$WF_VA_SRV" "$WF_VA_VOL_ID"
assert_rc 0

osc volume show "$WF_VA_VOL_ID"
assert_rc 0
assert_jq '.status' 'available'
assert_jq '.attachments | length' 0

osc server show "$WF_VA_SRV_ID"
assert_rc 0
assert_jq '.volumes_attached | length' 0

osc volume delete "$WF_VA_VOL_ID"
assert_rc 0

# The volume must be gone from both show and list.
osc volume show "$WF_VA_VOL_ID"
if (( OSC_RC == 0 )); then
    t_fail "volume show of deleted volume '$WF_VA_VOL' must not exit 0"
fi
osc volume list
assert_rc 0
assert_jq '[.[] | select((.Name // .name) == "'$WF_VA_VOL'")] | length' 0

osc server delete --wait "$WF_VA_SRV_ID"
assert_rc 0

t_end

# ===========================================================================
# 4. wf.securitygroup.lifecycle
#    Secgroup create → ssh rule add → rule listed with the right port range →
#    attach to a running server → verify listed → detach → rule remove →
#    group delete. Stub mode: API state only, no dataplane.
# ===========================================================================
t_begin wf.securitygroup.lifecycle

WF_SG_GRP="cli-conf-wf-sg"
WF_SG_SRV="cli-conf-wf-sg-srv"

osc server delete --wait "$WF_SG_SRV"
osc security group delete "$WF_SG_GRP"

osc security group create "$WF_SG_GRP"
assert_rc 0
WF_SG_ID="$(wf_jq '.id')"
assert_jq '.name' "$WF_SG_GRP"

osc security group rule create "$WF_SG_ID" --protocol tcp --dst-port 22:22 --remote-ip 0.0.0.0/0
assert_rc 0
WF_SG_RULE_ID="$(wf_jq '.id')"
assert_jq '.protocol' 'tcp'
assert_jq '.port_range_min' '22'
assert_jq '.port_range_max' '22'
assert_jq '.direction' 'ingress'

# The rule is listed on the group with the correct port range.
osc security group show "$WF_SG_ID"
assert_rc 0
assert_jq '[.rules[] | select(.id == "'$WF_SG_RULE_ID'" and .protocol == "tcp" and .port_range_min == 22 and .port_range_max == 22)] | length' 1

osc server create "$WF_SG_SRV" --flavor m1.tiny --image cli-conf-image --network cli-conf-net
assert_rc 0
WF_SG_SRV_ID="$(wf_jq '.id')"
wf_wait server "$WF_SG_SRV_ID" '.status' 'ACTIVE'

osc server add security group "$WF_SG_SRV" "$WF_SG_GRP"
assert_rc 0

osc server show "$WF_SG_SRV_ID"
assert_rc 0
assert_jq '([.security_groups[].name] | index("'$WF_SG_GRP'")) != null' 'true'

osc server remove security group "$WF_SG_SRV" "$WF_SG_GRP"
assert_rc 0

osc server show "$WF_SG_SRV_ID"
assert_rc 0
assert_jq '([.security_groups[].name] | index("'$WF_SG_GRP'")) == null' 'true'

osc security group rule delete "$WF_SG_RULE_ID"
assert_rc 0

osc security group show "$WF_SG_ID"
assert_rc 0
assert_jq '.rules | length' 0

osc server delete --wait "$WF_SG_SRV_ID"
assert_rc 0
osc security group delete "$WF_SG_ID"
assert_rc 0

t_end

# ===========================================================================
# 5. wf.floatingip.associate-disassociate
#    External network + subnet, router with external gateway and an internal
#    interface, server on the internal net, fip create → associate → verify
#    the address on the server → disassociate → release. API state only.
# ===========================================================================
t_begin wf.floatingip.associate-disassociate

WF_FP_EXT="cli-conf-wf-fip-ext"
WF_FP_EXTSUB="cli-conf-wf-fip-extsub"
WF_FP_RTR="cli-conf-wf-fip-rtr"
WF_FP_NET="cli-conf-wf-fip-net"
WF_FP_SUB="cli-conf-wf-fip-sub"
WF_FP_SRV="cli-conf-wf-fip-srv"

# Rerun-safety: reclaim leftovers. Floating IPs are nameless; release any
# leaked on a previous run's external network before deleting that network
# (a network with allocated floating IPs cannot be deleted).
osc server delete --wait "$WF_FP_SRV"
osc network show "$WF_FP_EXT"
if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
    WF_FP_OLD_EXT_ID="$(wf_jq '.id')"
    osc floating ip list
    if (( OSC_RC == 0 )); then
        while IFS= read -r wf_fip_id; do
            [[ -z "$wf_fip_id" || "$wf_fip_id" == "null" ]] && continue
            osc floating ip delete "$wf_fip_id"
        done < <(printf '%s' "$OSC_OUT" | jq -r \
            '.[] | select((."Floating Network" // .floating_network_id) == "'$WF_FP_OLD_EXT_ID'") | (.ID // .id)' 2>/dev/null)
    fi
fi
osc router remove subnet "$WF_FP_RTR" "$WF_FP_SUB"
osc router delete "$WF_FP_RTR"
osc subnet delete "$WF_FP_SUB"
osc subnet delete "$WF_FP_EXTSUB"
osc network delete "$WF_FP_NET"
osc network delete "$WF_FP_EXT"

osc network create "$WF_FP_EXT" --external
assert_rc 0
WF_FP_EXT_ID="$(wf_jq '.id')"
assert_jq '.["router:external"]' 'true'

osc subnet create "$WF_FP_EXTSUB" --network "$WF_FP_EXT_ID" --subnet-range 203.0.113.0/24 --no-dhcp
assert_rc 0

osc network create "$WF_FP_NET"
assert_rc 0
WF_FP_NET_ID="$(wf_jq '.id')"

osc subnet create "$WF_FP_SUB" --network "$WF_FP_NET_ID" --subnet-range 10.97.0.0/24
assert_rc 0
WF_FP_SUB_ID="$(wf_jq '.id')"

osc router create "$WF_FP_RTR"
assert_rc 0
WF_FP_RTR_ID="$(wf_jq '.id')"

osc router set "$WF_FP_RTR" --external-gateway "$WF_FP_EXT_ID"
assert_rc 0

osc router add subnet "$WF_FP_RTR" "$WF_FP_SUB_ID"
assert_rc 0

osc router show "$WF_FP_RTR_ID"
assert_rc 0
assert_jq '.external_gateway_info.network_id' "$WF_FP_EXT_ID"
assert_jq '([.interfaces_info[].subnet_id] | index("'$WF_FP_SUB_ID'")) != null' 'true'

osc server create "$WF_FP_SRV" --flavor m1.tiny --image cli-conf-image --network "$WF_FP_NET_ID"
assert_rc 0
WF_FP_SRV_ID="$(wf_jq '.id')"
wf_wait server "$WF_FP_SRV_ID" '.status' 'ACTIVE'

osc floating ip create "$WF_FP_EXT_ID"
assert_rc 0
WF_FP_FIP_ID="$(wf_jq '.id')"
WF_FP_FIP_ADDR="$(wf_jq '.floating_ip_address')"
assert_jq '.floating_ip_address | startswith("203.0.113.")' 'true'
assert_jq '.status' 'DOWN'

osc server add floating ip "$WF_FP_SRV" "$WF_FP_FIP_ID"
assert_rc 0

osc server show "$WF_FP_SRV_ID"
assert_rc 0
assert_jq '.addresses["cli-conf-wf-fip-net"] | index("'$WF_FP_FIP_ADDR'") != null' 'true'

# The association is also visible on the floating ip via `floating ip list`
# (`floating ip show` is broken CLI-side in this pin — see the final report).
osc floating ip list
assert_rc 0
assert_jq '[.[] | select(.ID == "'$WF_FP_FIP_ID'")] | (length == 1) and ((.[0].Port // "") != "")' 'true'

osc server remove floating ip "$WF_FP_SRV" "$WF_FP_FIP_ID"
assert_rc 0

osc server show "$WF_FP_SRV_ID"
assert_rc 0
assert_jq '.addresses["cli-conf-wf-fip-net"] | index("'$WF_FP_FIP_ADDR'") == null' 'true'

osc floating ip list
assert_rc 0
assert_jq '[.[] | select(.ID == "'$WF_FP_FIP_ID'") | (.Port // "")] | .[0]' ''

# Release the floating ip (nameless resources must not leak) and verify.
osc floating ip delete "$WF_FP_FIP_ID"
assert_rc 0
osc floating ip list
assert_rc 0
assert_jq '[.[] | select(.ID == "'$WF_FP_FIP_ID'")] | length' 0

# Clean up: dependents first.
osc server delete --wait "$WF_FP_SRV_ID"
assert_rc 0
osc router remove subnet "$WF_FP_RTR" "$WF_FP_SUB_ID"
assert_rc 0
osc router delete "$WF_FP_RTR_ID"
assert_rc 0
osc subnet delete "$WF_FP_SUB"
assert_rc 0
osc subnet delete "$WF_FP_EXTSUB"
assert_rc 0
osc network delete "$WF_FP_NET"
assert_rc 0
osc network delete "$WF_FP_EXT"
assert_rc 0

t_end

# ===========================================================================
# 6. wf.project.isolation
#    Two projects with a user each and identically-named networks; each user
#    sees only their own network (auth-scoped listing), while the admin
#    cross-project listing filter is a tracked known failure.
# ===========================================================================
t_begin wf.project.isolation

WF_ISO_P1="cli-conf-wf-p1"
WF_ISO_P2="cli-conf-wf-p2"
WF_ISO_U1="cli-conf-wf-u1"
WF_ISO_U2="cli-conf-wf-u2"
WF_ISO_NET="cli-conf-wf-iso-net"
WF_ISO_U1_PASS="cli-conf-wf-u1-pass"
WF_ISO_U2_PASS="cli-conf-wf-u2-pass"

# Rerun-safety: users first, then projects (project delete cascades their
# networks — verified against this server).
osc user delete "$WF_ISO_U1"
osc user delete "$WF_ISO_U2"
osc project delete "$WF_ISO_P1"
osc project delete "$WF_ISO_P2"

osc project create "$WF_ISO_P1"
assert_rc 0
WF_ISO_P1_ID="$(wf_jq '.id')"
osc project create "$WF_ISO_P2"
assert_rc 0
WF_ISO_P2_ID="$(wf_jq '.id')"

osc user create "$WF_ISO_U1" --password "$WF_ISO_U1_PASS" --project "$WF_ISO_P1"
assert_rc 0
osc role add --project "$WF_ISO_P1" --user "$WF_ISO_U1" member
assert_rc 0
osc user create "$WF_ISO_U2" --password "$WF_ISO_U2_PASS" --project "$WF_ISO_P2"
assert_rc 0
osc role add --project "$WF_ISO_P2" --user "$WF_ISO_U2" member
assert_rc 0

# Identically-named networks, one per project (created as admin with --project).
osc network create "$WF_ISO_NET" --project "$WF_ISO_P1_ID"
assert_rc 0
WF_ISO_N1_ID="$(wf_jq '.id')"
assert_jq '.project_id' "$WF_ISO_P1_ID"

osc network create "$WF_ISO_NET" --project "$WF_ISO_P2_ID"
assert_rc 0
WF_ISO_N2_ID="$(wf_jq '.id')"
assert_jq '.project_id' "$WF_ISO_P2_ID"

# User 1, scoped to project 1: sees exactly one network of that name — their
# own — and never project 2's network.
osc_as_user "$WF_ISO_U1" "$WF_ISO_U1_PASS" "$WF_ISO_P1" network list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "'$WF_ISO_NET'")] | length' 1
assert_jq '[.[] | select((.Name // .name) == "'$WF_ISO_NET'")][0] | (.ID // .id)' "$WF_ISO_N1_ID"
assert_jq '[.[] | select((.ID // .id) == "'$WF_ISO_N2_ID'")] | length' 0

# User 2, scoped to project 2: mirrored assertions.
osc_as_user "$WF_ISO_U2" "$WF_ISO_U2_PASS" "$WF_ISO_P2" network list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "'$WF_ISO_NET'")] | length' 1
assert_jq '[.[] | select((.Name // .name) == "'$WF_ISO_NET'")][0] | (.ID // .id)' "$WF_ISO_N2_ID"
assert_jq '[.[] | select((.ID // .id) == "'$WF_ISO_N1_ID'")] | length' 0

# Project scoping holds for the other services too: project 1's user sees no
# volumes or servers from the admin project.
osc_as_user "$WF_ISO_U1" "$WF_ISO_U1_PASS" "$WF_ISO_P1" volume list
assert_rc 0
assert_jq 'length' 0
osc_as_user "$WF_ISO_U1" "$WF_ISO_U1_PASS" "$WF_ISO_P1" server list
assert_rc 0
assert_jq 'length' 0

# Admin cross-project listing (GET /v2.0/networks?tenant_id=<project>).
# Genuine product divergence: the tenant_id list filter is not honored — the
# admin gets an empty list for a project that owns a network. Real Neutron
# returns that project's networks. Tracked, not hidden.
osc network list --project "$WF_ISO_P1_ID"
if (( OSC_RC == 0 )) && [[ "$(wf_jq '[.[] | select((.ID // .id) == "'$WF_ISO_N1_ID'")] | length')" == "1" ]]; then
    : # the filter works — no known failure to record
else
    t_known_failure "openstack network list --project <id> as admin (GET /v2.0/networks?tenant_id=...) returns no networks even though the project owns one — the tenant_id list filter is not honored; per-project visibility only works via project-scoped tokens"
fi

# Clean up: users, then projects (projects cascade-delete their networks).
osc user delete "$WF_ISO_U1"
assert_rc 0
osc user delete "$WF_ISO_U2"
assert_rc 0
osc project delete "$WF_ISO_P1_ID"
assert_rc 0
osc project delete "$WF_ISO_P2_ID"
assert_rc 0

t_end

# ===========================================================================
# 7. wf.quota.enforcement
#    On a dedicated project: set a tiny volume quota, verify oversized/count
#    -exceeding creations fail with a quota error, restore the quota, verify
#    creation works again, clean up.
# ===========================================================================
t_begin wf.quota.enforcement

WF_QT_PROJ="cli-conf-wf-quota"
WF_QT_V1="cli-conf-wf-qvol1"
WF_QT_V2="cli-conf-wf-qvol2"

# Rerun-safety: reclaim leaked volumes (as admin scoped to the project, if
# the project survived a crashed run), then the project itself.
osc project show "$WF_QT_PROJ"
if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
    osc role add --project "$WF_QT_PROJ" --user admin admin
    osc_as_project "$WF_QT_PROJ" volume delete "$WF_QT_V1" "$WF_QT_V2"
fi
osc project delete "$WF_QT_PROJ"

osc project create "$WF_QT_PROJ"
assert_rc 0

# The admin user needs a role in the project to scope a token to it.
osc role add --project "$WF_QT_PROJ" --user admin admin
assert_rc 0

# Capture the current quota so it can be restored exactly.
osc quota show "$WF_QT_PROJ"
assert_rc 0
WF_QT_GB0="$(wf_jq '[.[] | select(.Resource == "gigabytes") | .Limit] | .[0]')"
WF_QT_VOL0="$(wf_jq '[.[] | select(.Resource == "volumes") | .Limit] | .[0]')"

# --- Capacity quota: gigabytes=1, volumes=2 (the count must not trip first).
osc quota set "$WF_QT_PROJ" --gigabytes 1 --volumes 2
assert_rc 0

osc quota show "$WF_QT_PROJ"
assert_rc 0
assert_jq '[.[] | select(.Resource == "gigabytes") | .Limit] | .[0]' '1'
assert_jq '[.[] | select(.Resource == "volumes") | .Limit] | .[0]' '2'

osc_as_project "$WF_QT_PROJ" volume create "$WF_QT_V1" --size 1
assert_rc 0
WF_QT_V1_ID="$(wf_jq '.id')"
WF_WAIT_PROJECT="$WF_QT_PROJ"
wf_wait volume "$WF_QT_V1_ID" '.status' 'available'
WF_WAIT_PROJECT=""

# A second 1 GB volume fits the volume count but exceeds the gigabytes quota.
osc_as_project "$WF_QT_PROJ" volume create "$WF_QT_V2" --size 1
if (( OSC_RC == 0 )); then
    t_fail "volume create of 1GB must fail under gigabytes=1 quota with one 1GB volume already present"
    osc_as_project "$WF_QT_PROJ" volume delete "$WF_QT_V2"
else
    assert_err_contains 'Quota exceeded'
    assert_err_contains 'gigabytes'
fi

# --- Count quota: restore gigabytes, tighten volumes to 1.
osc quota set "$WF_QT_PROJ" --gigabytes "$WF_QT_GB0" --volumes 1
assert_rc 0

osc_as_project "$WF_QT_PROJ" volume create "$WF_QT_V2" --size 1
if (( OSC_RC == 0 )); then
    t_fail "volume create must fail under volumes=1 quota with one volume already present"
    osc_as_project "$WF_QT_PROJ" volume delete "$WF_QT_V2"
else
    assert_err_contains 'Quota exceeded'
    assert_err_contains 'volumes'
fi

# --- Restore the original quota; creation must work again.
osc quota set "$WF_QT_PROJ" --gigabytes "$WF_QT_GB0" --volumes "$WF_QT_VOL0"
assert_rc 0

osc quota show "$WF_QT_PROJ"
assert_rc 0
assert_jq '[.[] | select(.Resource == "gigabytes") | .Limit] | .[0]' "$WF_QT_GB0"
assert_jq '[.[] | select(.Resource == "volumes") | .Limit] | .[0]' "$WF_QT_VOL0"

osc_as_project "$WF_QT_PROJ" volume create "$WF_QT_V2" --size 1
assert_rc 0
WF_QT_V2_ID="$(wf_jq '.id')"
WF_WAIT_PROJECT="$WF_QT_PROJ"
wf_wait volume "$WF_QT_V2_ID" '.status' 'available'
WF_WAIT_PROJECT=""

# Clean up: volumes, then the project (takes its role assignment and quota
# with it).
osc_as_project "$WF_QT_PROJ" volume delete "$WF_QT_V1"
assert_rc 0
osc_as_project "$WF_QT_PROJ" volume delete "$WF_QT_V2"
assert_rc 0
osc project delete "$WF_QT_PROJ"
assert_rc 0

t_end

suite_end
