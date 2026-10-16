#!/bin/bash
# compute.sh — SPEC-028 tier A/B conformance cases for the compute (Nova) service.
#
# Covers every command in manifest/compute.json: server CRUD + actions, image/
# backup creation, consoles, flavors, keypairs, server groups, availability
# zones, hypervisors, usage, aggregates, and server migrations.
#
# Fixtures (already ensured by the runner, used BY NAME only):
#   image `cli-conf-image`, network `cli-conf-net` (10.99.0.0/24), flavor m1.tiny.
# Every case-created resource is named cli-conf-cp-* and cleaned up by the case.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin compute

CP_IMAGE="cli-conf-image"
CP_NETWORK="cli-conf-net"
CP_FLAVOR="m1.tiny"
CP_MISSING="cli-conf-cp-missing"

# A real 2048-bit RSA public key (generated for this suite; embedding it keeps
# the suite free of a runtime ssh-keygen dependency and makes the expected
# fingerprint a constant).
CP_KEY_FINGERPRINT="a5:01:44:15:f2:42:c6:cb:cc:fc:3d:e6:14:e6:b0:4d"
CP_PUBKEY_FILE="$(mktemp "${TMPDIR:-/tmp}/cli-conf-cp-key.XXXXXX.pub")"
# Removed by the EXIT trap via this hook (common.sh _cli_cleanup), so a
# mid-flight kill of a standalone run cannot leak the temp file.
suite_cleanup() {
    rm -f "$CP_PUBKEY_FILE"
}
printf '%s\n' "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQDIoG5/PC4ypt7TTVk9LqDrwDULkkkA+T7gROFk0Bb3s1hTGhE5JyUtHConWSKAPrEzw8X9CfZtuCi0t+abThZG6Va5ZJB2Vn4zmQA3ScGgd6vtMKUeesK69jOnw6CW1J/HspZn3/bFdZTBnB+JlFEDh1cm5RQQdDJsyURqkjcJQrZOwfBKgLPIY5T2iipNAZwLpmDdC3oJ90E3uqHBtvHKAzwJx8JccHeXrwUTkhrUx6j0Tje0uQjR1T0h/T+psrFtIY7G6EJ5vQ7E4dqtwkAHcgWdHww6iHXU7AC0Zk7WhR/nrp9IxpKQXok1pSKmzjWqwN+VLXqcEno77+wx7tl7 root@dev" >"$CP_PUBKEY_FILE"

# ---------------------------------------------------------------------------
# Helpers (case-scoped, rerun-safe: purge leftovers before creating)
# ---------------------------------------------------------------------------

# Best-effort deletion of a possibly-leftover server; a 404 here is expected.
_cp_server_purge() {
    local name="$1"
    OSC_QUIET=1 osc server delete --wait "$name"
    return 0
}

# Create a server on the shared fixtures and wait (bounded) for it to be ACTIVE.
_cp_server_ensure() {
    local name="$1"
    _cp_server_purge "$name"
    osc server create "$name" --image "$CP_IMAGE" --flavor "$CP_FLAVOR" --network "$CP_NETWORK"
    if (( OSC_RC != 0 )); then
        t_fail "fixture server create failed: $(_snip "$OSC_ERR")"
        return 1
    fi
    _cp_wait_status "$name" "ACTIVE"
}

# Bounded poll of `server show` status. Fails the case on timeout.
_cp_wait_status() {
    local name="$1" want="$2" tries="${3:-15}" i=0 st=""
    while (( i < tries )); do
        OSC_QUIET=1 osc server show "$name"
        if (( OSC_RC == 0 )); then
            st=$(printf '%s' "$OSC_OUT" | jq -r '.status')
            [[ "$st" == "$want" ]] && return 0
        fi
        sleep 1
        i=$(( i + 1 ))
    done
    t_fail "server '$name' did not reach status $want within ${tries}s (last: ${st:-none})"
    return 1
}

# Assert the named action was recorded in the server's action history
# (os-instance-actions) — the observable effect of lock/unlock/reboot/... .
_cp_assert_event() {
    local name="$1" action="$2"
    osc server event list "$name"
    assert_rc 0
    assert_jq "[.[] | select(.Action == \"$action\")] | length >= 1"
}

# Negative: a server command against a nonexistent server must exit non-zero
# with an error message on stderr. Args: everything after `openstack server`.
_cp_neg_server() {
    osc server "$@" "$CP_MISSING"
    if (( OSC_RC == 0 )); then
        t_fail "'server $*' against a nonexistent server must exit non-zero"
    else
        _cp_assert_err_nonempty
    fi
}

# The CLI's client-side "resource not found" path prints a message on stderr
# and exits non-zero; assert presence, not exact wording.
_cp_assert_err_nonempty() {
    if [[ -z "$OSC_ERR" ]]; then
        t_fail "expected an error message on stderr for: ${LAST_CMD:-?}"
    fi
}

# Ensure a keypair exists (used by list/show/delete cases).
_cp_key_ensure() {
    local name="$1"
    OSC_QUIET=1 osc keypair delete "$name"
    osc keypair create "$name" --public-key "$CP_PUBKEY_FILE"
    if (( OSC_RC != 0 )); then
        t_fail "fixture keypair create failed: $(_snip "$OSC_ERR")"
        return 1
    fi
}

# Ensure an aggregate is gone (delete tolerates absence — the CLI resolves
# names, but id-delete keeps the purge deterministic when a name is reused).
_cp_agg_purge() {
    local name="$1" id
    OSC_QUIET=1 osc aggregate list
    id=$(printf '%s' "$OSC_OUT" | jq -r --arg n "$name" '.[] | select(.Name == $n) | .ID')
    [[ -n "$id" && "$id" != "null" ]] && OSC_QUIET=1 osc aggregate delete "$id"
    return 0
}

# Ensure a server group exists. The create API accepts both the legacy
# pre-2.63 'policies' array and the 2.64+ 'policy' string (SPEC-028 fix), so
# the fixture uses the current client shape.
_cp_sg_ensure() {
    local name="$1"
    OSC_QUIET=1 osc server group delete "$name"
    OSC_QUIET=1 osc server group create "$name" --policy anti-affinity
    if (( OSC_RC != 0 )); then
        t_fail "fixture server group create failed: $(_snip "$OSC_ERR")"
        return 1
    fi
}

# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------

t_begin compute.server.create
S="cli-conf-cp-create"
_cp_server_purge "$S"
osc server create "$S" --image "$CP_IMAGE" --flavor "$CP_FLAVOR" --network "$CP_NETWORK"
assert_rc 0
# Assert against the create response itself, before any polling clobbers OSC_OUT.
assert_jq '.name' "$S"
assert_jq '.status' "ACTIVE"
assert_jq '.id'
assert_jq_type '.addresses' object
assert_jq '.addresses["cli-conf-net"] | length' 1
assert_jq '.addresses["cli-conf-net"][0] | startswith("10.99.0.")' "true"
assert_jq '.flavor | startswith("m1.tiny (")' "true"
assert_jq '.image | startswith("cli-conf-image (")' "true"
assert_jq_type '.security_groups' array
_cp_wait_status "$S" "ACTIVE"
osc server show "$S"
assert_jq '.status' "ACTIVE"
assert_jq '.flavor | startswith("m1.tiny (")' "true"
assert_jq '.image | startswith("cli-conf-image (")' "true"
_cp_server_purge "$S"
t_end

t_begin compute.server.list
S="cli-conf-cp-list"
_cp_server_ensure "$S"
osc server list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select(.Name == \"$S\")] | length" 1
assert_jq "[.[] | select(.Name == \"$S\")][0].Status" "ACTIVE"
assert_jq "[.[] | select(.Name == \"$S\")][0].Networks[\"cli-conf-net\"] | length" 1
assert_jq_type "[.[] | select(.Name == \"$S\")][0].ID" string
_cp_server_purge "$S"
t_end

t_begin compute.server.show
S="cli-conf-cp-show"
_cp_server_ensure "$S"
osc server show "$S"
assert_rc 0
assert_jq '.name' "$S"
assert_jq '.status' "ACTIVE"
assert_jq_type '.id' string
assert_jq_type '.project_id' string
assert_jq_type '.created' string
assert_jq_type '.addresses' object
assert_jq '.addresses["cli-conf-net"] | length' 1
assert_jq '.addresses["cli-conf-net"][0] | startswith("10.99.0.")' "true"
assert_jq '.flavor | startswith("m1.tiny (")' "true"
assert_jq '.image | startswith("cli-conf-image (")' "true"
assert_jq_type '.security_groups' array
_cp_server_purge "$S"
t_end

t_begin compute.server.set
S="cli-conf-cp-set"
R="cli-conf-cp-set-renamed"
_cp_server_ensure "$S"
osc server set --name "$R" "$S"
assert_rc 0
osc server show "$R"
assert_rc 0
assert_jq '.name' "$R"
_cp_server_purge "$R"
t_end

t_begin compute.server.delete
S="cli-conf-cp-del"
_cp_server_ensure "$S"
osc server delete --wait "$S"
assert_rc 0
osc server show "$S"
if (( OSC_RC == 0 )); then
    t_fail "server show after delete must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

# ---------------------------------------------------------------------------
# Server actions
# ---------------------------------------------------------------------------

t_begin compute.server.action.stop
S="cli-conf-cp-stop"
_cp_server_ensure "$S"
osc server stop "$S"
assert_rc 0
_cp_wait_status "$S" "SHUTOFF"
_cp_assert_event "$S" "stop"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.start
S="cli-conf-cp-start"
_cp_server_ensure "$S"
osc server stop "$S"
_cp_wait_status "$S" "SHUTOFF"
osc server start "$S"
assert_rc 0
_cp_wait_status "$S" "ACTIVE"
_cp_assert_event "$S" "start"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.reboot
S="cli-conf-cp-reboot"
_cp_server_ensure "$S"
osc server reboot --hard "$S"
assert_rc 0
_cp_wait_status "$S" "ACTIVE"
_cp_assert_event "$S" "reboot"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.pause
S="cli-conf-cp-pause"
_cp_server_ensure "$S"
osc server pause "$S"
assert_rc 0
_cp_wait_status "$S" "PAUSED"
_cp_assert_event "$S" "pause"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.unpause
S="cli-conf-cp-unpause"
_cp_server_ensure "$S"
osc server pause "$S"
_cp_wait_status "$S" "PAUSED"
osc server unpause "$S"
assert_rc 0
_cp_wait_status "$S" "ACTIVE"
_cp_assert_event "$S" "unpause"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.suspend
S="cli-conf-cp-suspend"
_cp_server_ensure "$S"
osc server suspend "$S"
assert_rc 0
_cp_wait_status "$S" "SUSPENDED"
_cp_assert_event "$S" "suspend"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.resume
S="cli-conf-cp-resume"
_cp_server_ensure "$S"
osc server suspend "$S"
_cp_wait_status "$S" "SUSPENDED"
osc server resume "$S"
assert_rc 0
_cp_wait_status "$S" "ACTIVE"
_cp_assert_event "$S" "resume"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.lock
S="cli-conf-cp-lock"
_cp_server_ensure "$S"
osc server lock "$S"
assert_rc 0
# The locked flag is surfaced in `server show` (SPEC-028 fix); the action
# history is the independent record of the transition.
osc server show "$S"
assert_rc 0
assert_jq '.locked' true
_cp_assert_event "$S" "lock"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.unlock
S="cli-conf-cp-unlock"
_cp_server_ensure "$S"
osc server lock "$S"
osc server unlock "$S"
assert_rc 0
osc server show "$S"
assert_rc 0
assert_jq '.locked' false
_cp_assert_event "$S" "unlock"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.shelve
S="cli-conf-cp-shelve"
_cp_server_ensure "$S"
osc server shelve "$S"
assert_rc 0
_cp_wait_status "$S" "SHELVED"
_cp_assert_event "$S" "shelve"
_cp_server_purge "$S"
t_end

t_begin compute.server.action.unshelve
S="cli-conf-cp-unshelve"
_cp_server_ensure "$S"
osc server shelve "$S"
_cp_wait_status "$S" "SHELVED"
osc server unshelve "$S"
assert_rc 0
_cp_wait_status "$S" "ACTIVE"
_cp_assert_event "$S" "unshelve"
_cp_server_purge "$S"
t_end

# ---------------------------------------------------------------------------
# Server image / backup creation
# ---------------------------------------------------------------------------

t_begin compute.server.image.create
S="cli-conf-cp-snap"
_cp_server_ensure "$S"
osc server image create --name "cli-conf-cp-snapimg" "$S"
assert_rc 0
assert_jq '.name' "cli-conf-cp-snapimg"
assert_jq '.status' "active"
IMG_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
assert_jq_type '.id' string
OSC_QUIET=1 osc image delete "$IMG_ID"
_cp_server_purge "$S"
t_end

t_begin compute.server.backup.create
S="cli-conf-cp-bak"
_cp_server_ensure "$S"
osc server backup create --name "cli-conf-cp-bakimg" "$S" --rotate 1
assert_rc 0
# Upstream Nova names the backup image exactly the requested name — that
# is what the CLI's own post-create lookup (and every client's) depends on.
osc image list
assert_jq '[.[] | select((.Name // .name) == "cli-conf-cp-bakimg")] | length' 1
# Reclaim the backup image(s), whichever name they got.
osc image list
for img in $(printf '%s' "$OSC_OUT" | jq -r '.[] | select((.Name // .name) | startswith("cli-conf-cp-bakimg")) | .ID // .id'); do
    OSC_QUIET=1 osc image delete "$img"
done
_cp_server_purge "$S"
t_end

# ---------------------------------------------------------------------------
# Consoles
# ---------------------------------------------------------------------------

t_begin compute.console.log.show
S="cli-conf-cp-clog"
_cp_server_ensure "$S"
osc console log show "$S" # formatter-less command; osc() retries without -f json
assert_rc 0
assert_contains "Console output"
_cp_server_purge "$S"
t_end

t_begin compute.console.url.show
S="cli-conf-cp-curl"
_cp_server_ensure "$S"
osc console url show "$S"
if (( OSC_RC == 0 )); then
    t_fail "console url show unexpectedly succeeded — the stub-mode console refusal may be fixed; convert this case to assert the URL shape"
else
    assert_err_contains "501"
    t_known_failure "console proxy is refused in stub mode: POST /v2.1/servers/{id}/remote-consoles answers 501 'console unavailable: nova is running in stub mode, there is no guest to display' — a live proxy is required for a console URL"
fi
_cp_server_purge "$S"
t_end

# ---------------------------------------------------------------------------
# Flavors (custom flavors only — the seeded m1.* are never modified)
# ---------------------------------------------------------------------------

t_begin compute.flavor.list
osc flavor list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "m1.tiny")] | length' 1
assert_jq '[.[] | select((.Name // .name) == "m1.tiny")][0].RAM' "512"
assert_jq '[.[] | select((.Name // .name) == "m1.tiny")][0].VCPUs' "1"
t_end

t_begin compute.flavor.show
osc flavor show m1.tiny
assert_rc 0
assert_jq '.name' "m1.tiny"
assert_jq '.ram' "512"
assert_jq '.vcpus' "1"
assert_jq '.disk' "1"
assert_jq_type '.id' string
t_end

t_begin compute.flavor.create
F="cli-conf-cp-flv"
OSC_QUIET=1 osc flavor delete "$F"
osc flavor create "$F" --ram 256 --disk 2 --vcpus 2
assert_rc 0
assert_jq '.name' "$F"
assert_jq '.ram' "256"
assert_jq '.disk' "2"
assert_jq '.vcpus' "2"
assert_jq_type '.id' string
osc flavor delete "$F"
assert_rc 0
t_end

t_begin compute.flavor.set
F="cli-conf-cp-flvset"
OSC_QUIET=1 osc flavor delete "$F"
osc flavor create "$F" --ram 256 --disk 1 --vcpus 1 >/dev/null
osc flavor set --property cp-marker=yes "$F" # formatter-less; osc() retries without -f json
assert_rc 0
osc flavor show "$F"
assert_rc 0
assert_jq '.properties["cp-marker"]' "yes"
osc flavor delete "$F"
assert_rc 0
t_end

t_begin compute.flavor.delete
F="cli-conf-cp-flvdel"
OSC_QUIET=1 osc flavor delete "$F"
osc flavor create "$F" --ram 256 --disk 1 --vcpus 1 >/dev/null
osc flavor delete "$F"
assert_rc 0
osc flavor show "$F"
if (( OSC_RC == 0 )); then
    t_fail "flavor show after delete must exit non-zero"
fi
t_end

# ---------------------------------------------------------------------------
# Keypairs
# ---------------------------------------------------------------------------

t_begin compute.keypair.create
K="cli-conf-cp-key"
_cp_key_ensure "$K"
assert_jq '.name' "$K"
assert_jq '.fingerprint' "$CP_KEY_FINGERPRINT"
assert_jq '.type' "ssh"
assert_jq_type '.user_id' string
osc keypair delete "$K"
assert_rc 0
t_end

t_begin compute.keypair.list
K="cli-conf-cp-keylist"
_cp_key_ensure "$K"
osc keypair list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$K\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$K\")][0].Fingerprint" "$CP_KEY_FINGERPRINT"
osc keypair delete "$K"
assert_rc 0
t_end

t_begin compute.keypair.show
K="cli-conf-cp-keyshow"
_cp_key_ensure "$K"
osc keypair show "$K"
assert_rc 0
assert_jq '.name' "$K"
assert_jq '.fingerprint' "$CP_KEY_FINGERPRINT"
assert_jq '.type' "ssh"
osc keypair delete "$K"
assert_rc 0
t_end

t_begin compute.keypair.delete
K="cli-conf-cp-keydel"
_cp_key_ensure "$K"
osc keypair delete "$K"
assert_rc 0
osc keypair show "$K"
if (( OSC_RC == 0 )); then
    t_fail "keypair show after delete must exit non-zero"
fi
t_end

# ---------------------------------------------------------------------------
# Server groups
# ---------------------------------------------------------------------------

t_begin compute.servergroup.create
G="cli-conf-cp-sg"
osc server group create "$G" --policy anti-affinity
assert_rc 0
assert_jq '.name' "$G"
assert_jq_type '.id' string
assert_jq_type '.project_id' string
# Microversion 2.64+ clients read the policy string; legacy ones the array.
# The server answers with both, so accept either spelling of the value.
assert_jq '(.policy // .policies[0])' anti-affinity
osc server group delete "$G"
assert_rc 0
t_end

t_begin compute.servergroup.list
G="cli-conf-cp-sglist"
_cp_sg_ensure "$G"
osc server group list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$G\")] | length" 1
assert_jq_type "[.[] | select((.Name // .name) == \"$G\")][0].ID" string
osc server group delete "$G"
assert_rc 0
t_end

t_begin compute.servergroup.show
G="cli-conf-cp-sgshow"
_cp_sg_ensure "$G"
osc server group show "$G"
assert_rc 0
assert_jq '.name' "$G"
assert_jq_type '.id' string
assert_jq_type '.project_id' string
osc server group delete "$G"
assert_rc 0
t_end

t_begin compute.servergroup.delete
G="cli-conf-cp-sgdel"
_cp_sg_ensure "$G"
osc server group delete "$G"
assert_rc 0
osc server group show "$G"
if (( OSC_RC == 0 )); then
    t_fail "server group show after delete must exit non-zero"
fi
t_end

# ---------------------------------------------------------------------------
# Read-only operator surfaces
# ---------------------------------------------------------------------------

t_begin compute.availabilityzone.list
osc availability zone list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select(.["Zone Name"] == "nova")] | length >= 1'
t_end

t_begin compute.hypervisor.list
osc hypervisor list
assert_rc 0
assert_jq_type '.' array
assert_jq_gt 'length' 0
assert_jq_type '.[0]["Hypervisor Hostname"]' string
t_end

t_begin compute.usage.list
osc usage list
assert_rc 0
assert_jq_type '.' array
assert_jq 'all(.[]; (.Project | type) == "string")' "true"
t_end

# ---------------------------------------------------------------------------
# Aggregates (admin)
# ---------------------------------------------------------------------------

t_begin compute.aggregate.list
A="cli-conf-cp-aglist"
_cp_agg_purge "$A"
osc aggregate create "$A"
assert_rc 0
osc aggregate list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$A\")] | length" 1
_cp_agg_purge "$A"
t_end

t_begin compute.aggregate.create
A="cli-conf-cp-agg"
_cp_agg_purge "$A"
osc aggregate create "$A"
assert_rc 0
assert_jq '.name' "$A"
assert_jq_type '.id' string
assert_jq_type '.created_at' string
assert_jq_type '.hosts' array
_cp_agg_purge "$A"
t_end

t_begin compute.aggregate.delete
A="cli-conf-cp-aggdel"
_cp_agg_purge "$A"
osc aggregate create "$A"
assert_rc 0
# The documented CLI shape is name-or-ID; exercise the name path.
osc aggregate delete "$A"
assert_rc 0
osc aggregate list
assert_jq "[.[] | select(.Name == \"$A\")] | length" 0
t_end

# ---------------------------------------------------------------------------
# Server migrations (admin, read-only)
# ---------------------------------------------------------------------------

t_begin compute.server.migration.list
S="cli-conf-cp-mig"
_cp_server_ensure "$S"
osc server migration list --server "$S"
assert_rc 0
assert_jq_type '.' array
_cp_server_purge "$S"
t_end

# ---------------------------------------------------------------------------
# Negative paths (tier B)
# ---------------------------------------------------------------------------

t_begin compute.server.create.neg
osc server create "cli-conf-cp-neg" --image "$CP_IMAGE" --flavor "no.such.flavor" --network "$CP_NETWORK"
if (( OSC_RC == 0 )); then
    t_fail "server create with a nonexistent flavor must exit non-zero"
else
    _cp_assert_err_nonempty
fi
osc server create "cli-conf-cp-neg" --image "no.such.image" --flavor "$CP_FLAVOR" --network "$CP_NETWORK"
if (( OSC_RC == 0 )); then
    t_fail "server create with a nonexistent image must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.server.show.neg
_cp_neg_server show
t_end

t_begin compute.server.set.neg
_cp_neg_server set --name "cli-conf-cp-neg"
t_end

t_begin compute.server.delete.neg
_cp_neg_server delete --wait
t_end

t_begin compute.server.action.stop.neg
_cp_neg_server stop
t_end

t_begin compute.server.action.start.neg
_cp_neg_server start
t_end

t_begin compute.server.action.reboot.neg
_cp_neg_server reboot --hard
t_end

t_begin compute.server.action.pause.neg
_cp_neg_server pause
t_end

t_begin compute.server.action.unpause.neg
_cp_neg_server unpause
t_end

t_begin compute.server.action.suspend.neg
_cp_neg_server suspend
t_end

t_begin compute.server.action.resume.neg
_cp_neg_server resume
t_end

t_begin compute.server.action.lock.neg
_cp_neg_server lock
t_end

t_begin compute.server.action.unlock.neg
_cp_neg_server unlock
t_end

t_begin compute.server.action.shelve.neg
_cp_neg_server shelve
t_end

t_begin compute.server.action.unshelve.neg
_cp_neg_server unshelve
t_end

t_begin compute.server.image.create.neg
osc server image create --name "cli-conf-cp-negimg" "$CP_MISSING"
if (( OSC_RC == 0 )); then
    t_fail "server image create on a nonexistent server must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.console.log.show.neg
osc console log show "$CP_MISSING"
if (( OSC_RC == 0 )); then
    t_fail "console log show on a nonexistent server must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.flavor.show.neg
osc flavor show "no.such.flavor"
if (( OSC_RC == 0 )); then
    t_fail "flavor show of a nonexistent flavor must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.flavor.delete.neg
osc flavor delete "no.such.flavor"
if (( OSC_RC == 0 )); then
    t_fail "flavor delete of a nonexistent flavor must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.keypair.show.neg
osc keypair show "cli-conf-cp-nokey"
if (( OSC_RC == 0 )); then
    t_fail "keypair show of a nonexistent keypair must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.keypair.delete.neg
osc keypair delete "cli-conf-cp-nokey"
if (( OSC_RC == 0 )); then
    t_fail "keypair delete of a nonexistent keypair must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.servergroup.show.neg
osc server group show "cli-conf-cp-nogroup"
if (( OSC_RC == 0 )); then
    t_fail "server group show of a nonexistent group must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.servergroup.delete.neg
osc server group delete "cli-conf-cp-nogroup"
if (( OSC_RC == 0 )); then
    t_fail "server group delete of a nonexistent group must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

t_begin compute.aggregate.delete.neg
osc aggregate delete "cli-conf-cp-noagg"
if (( OSC_RC == 0 )); then
    t_fail "aggregate delete of a nonexistent aggregate must exit non-zero"
else
    _cp_assert_err_nonempty
fi
t_end

suite_end
