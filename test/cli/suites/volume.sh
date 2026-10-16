#!/bin/bash
# volume.sh — SPEC-028 tier A/B conformance cases for Cinder (block storage).
#
# One case per manifest/volume.json entry: tier A uses the manifest id, tier B
# uses <id>.neg. Every resource is named cli-conf-vo-* and removed by the case
# that created it, so the suite is rerun-safe when run standalone:
#
#     bash test/cli/suites/volume.sh
#
# Known-failing cases (t_known_failure) record today's product truth with the
# precise divergence in the reason string; see manifest/volume.json.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin volume
# Standalone rerun-safety: this suite references the shared fixtures
# (cli-conf, cli-conf-image, cli-conf-net) by name. bin/run.sh ensures them
# and its teardown sweeps them, so running this suite alone after a sweep
# re-ensures here instead of failing with confusing missing-resource errors.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/fixtures.sh"
fixtures_require

VO_TYPE="ceph-rbd" # seeded shared volume type (migration 002)
QPROJECT="cli-conf" # shared fixture project for the admin quota cases

# ---------------------------------------------------------------------------
# Quiet best-effort cleanup helpers. Rerun-safety: every case pre-cleans the
# exact names it is about to create; all of these tolerate "not found".
# ---------------------------------------------------------------------------

# _vo_vol_id <name> — print the volume id, or nothing.
_vo_vol_id() {
    OSC_QUIET=1 osc volume show "$1"
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r '.id // empty'
}

# _vo_clean_volume <name> — delete the named volume, cancelling any pending
# transfer first (an awaiting-transfer volume refuses a plain delete).
_vo_clean_volume() {
    local vid tr
    vid=$(_vo_vol_id "$1")
    [[ -z "$vid" ]] && return 0
    OSC_QUIET=1 osc volume transfer request list
    if (( OSC_RC == 0 )); then
        while IFS= read -r tr; do
            [[ -n "$tr" ]] && OSC_QUIET=1 osc volume transfer request delete "$tr"
        done < <(printf '%s' "$OSC_OUT" |
            jq -r --arg v "$vid" '.[] | select((.Volume // .volume_id) == $v) | (.ID // .id)')
    fi
    OSC_QUIET=1 osc volume delete "$vid"
}

# _vo_snap_id <name> — print the snapshot id, or nothing (resolved via list:
# robust against the by-name GET answering 404, as upstream does, and against
# display-cased list keys alike).
_vo_snap_id() {
    OSC_QUIET=1 osc volume snapshot list
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r --arg n "$1" \
        '.[] | select((.Name // .name) == $n) | (.ID // .id) // empty'
}

_vo_clean_snapshot() {
    local id
    id=$(_vo_snap_id "$1")
    [[ -n "$id" ]] && OSC_QUIET=1 osc volume snapshot delete "$id"
}

# _vo_backup_id <name> — print the backup id, or nothing (resolved via list:
# the by-name GET /v3/backups/{name} is the divergence under test in
# volume.backup.show.byname, so it must not be used to resolve names).
_vo_backup_id() {
    OSC_QUIET=1 osc volume backup list
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r --arg n "$1" \
        '.[] | select((.Name // .name) == $n) | (.ID // .id) // empty'
}

_vo_clean_backup() {
    local id
    id=$(_vo_backup_id "$1")
    [[ -n "$id" ]] && OSC_QUIET=1 osc volume backup delete "$id"
}

# _vo_type_id <name> — print the volume type id, or nothing (via list; same
# robustness as _vo_snap_id — the by-name GET answers 404, as upstream does).
_vo_type_id() {
    OSC_QUIET=1 osc volume type list
    (( OSC_RC != 0 )) && return 0
    printf '%s' "$OSC_OUT" | jq -r --arg n "$1" \
        '.[] | select((.Name // .name) == $n) | (.ID // .id) // empty'
}

_vo_clean_type() {
    local id
    id=$(_vo_type_id "$1")
    [[ -n "$id" ]] && OSC_QUIET=1 osc volume type delete "$id"
}

# _vo_wait_available <volume> [tries] — bounded poll until the volume reports
# status "available" (stub-mode create is synchronous; this is belt and
# braces). Returns 1 without the status after <tries> one-second attempts.
_vo_wait_available() {
    local v="$1" tries="${2:-10}" i
    for (( i = 0; i < tries; i++ )); do
        OSC_QUIET=1 osc volume show "$v"
        if (( OSC_RC == 0 )) &&
            [[ "$(printf '%s' "$OSC_OUT" | jq -r '.status // empty')" == "available" ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# ===========================================================================
# Volumes
# ===========================================================================

t_begin volume.volume.create
V="cli-conf-vo-create"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
assert_jq '.name' "$V"
assert_jq '.size' 1
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.status // empty')" != "available" ]] &&
    ! _vo_wait_available "$V"; then
    t_fail "volume did not reach status available"
fi
osc volume show "$V"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$V"
assert_jq '.size' 1
assert_jq '.status' 'available'
# OSC 7 emits bootable as a string "false" from cinder's serializer
assert_jq '(.bootable == "false" or .bootable == false)' true
assert_jq_type '.attachments' array
assert_jq '.attachments | length' 0
assert_jq '.type'
assert_jq_type '.id' string
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] || t_fail "no volume id in show output"
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.create.bootable
V="cli-conf-vo-createboot"
_vo_clean_volume "$V"
osc volume create --size 1 --image cli-conf-image "$V"
assert_rc 0
assert_jq '.name' "$V"
assert_jq '.size' 1
assert_jq '(.bootable == "true" or .bootable == true)' true
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] || t_fail "no volume id in create output"
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.create.type
V="cli-conf-vo-createtype"
_vo_clean_volume "$V"
osc volume create --size 1 --type "$VO_TYPE" "$V"
assert_rc 0
assert_jq '.name' "$V"
assert_jq '.type' "$VO_TYPE"
assert_jq '.size' 1
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] || t_fail "no volume id in create output"
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.list
V="cli-conf-vo-list"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$V\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$V\")][0].Status" 'available'
assert_jq "[.[] | select((.Name // .name) == \"$V\")][0].Size" 1
assert_jq "[.[] | select((.Name // .name) == \"$V\")][0][\"Attached to\"] | type" 'array'
VID=$(_vo_vol_id "$V")
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.show
V="cli-conf-vo-show"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume show "$V"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$V"
assert_jq '.size' 1
assert_jq '.status' 'available'
assert_jq '(.bootable == "false" or .bootable == false)' true
assert_jq_type '.attachments' array
assert_jq '.attachments | length' 0
assert_jq '.type'
assert_jq_type '.id' string
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] || t_fail "no volume id in show output"
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.show.neg
osc volume show cli-conf-vo-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent volume must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin volume.volume.set
V="cli-conf-vo-set"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume set --description "set by cli-conf volume suite" "$V"
assert_rc 0
osc volume show "$V"
assert_rc 0
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.description // empty')" != "set by cli-conf volume suite" ]]; then
    t_known_failure "python-cinderclient PUTs /v3/volumes/{id} with the legacy v2 field names (display_description/display_name) and lightstack's UpdateVolume only binds description/name, so 'volume set --description' (and --name) answers 200 but persists nothing — a raw PUT carrying 'description' does update it; Cinder-compatible servers accept both aliases"
else
    assert_jq '.description' 'set by cli-conf volume suite'
fi
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.set.size
V="cli-conf-vo-extend"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume set --size 2 "$V"
assert_rc 0
osc volume show "$V"
assert_rc 0
assert_jq '.size' 2
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.set.size.neg
V="cli-conf-vo-shrink"
_vo_clean_volume "$V"
osc volume create --size 2 "$V"
assert_rc 0
# Extending to a smaller size must be refused server-side (os-extend requires
# new_size > current size).
osc volume set --size 1 "$V"
if (( OSC_RC == 0 )); then
    t_fail "extending a volume to a smaller size must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
osc volume show "$V"
assert_rc 0
assert_jq '.size' 2
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.set.property
V="cli-conf-vo-prop"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume set --property cli_conf_key=cli_conf_value "$V"
assert_rc 0
osc volume show "$V"
assert_rc 0
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.properties.cli_conf_key // empty')" != "cli_conf_value" ]]; then
    t_known_failure "volume metadata writes persist (GET /v3/volumes/{id}/metadata returns them) but GET /v3/volumes/{id} serializes metadata as {} — OSC maps metadata to 'properties', so 'volume show' never displays a property set via 'volume set --property'"
else
    assert_jq '.properties.cli_conf_key' 'cli_conf_value'
fi
osc volume unset --property cli_conf_key "$V"
assert_rc 0
VID=$(_vo_vol_id "$V")
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.set.bootable
V="cli-conf-vo-bootflag"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume set --bootable "$V"
if (( OSC_RC != 0 )); then
    t_known_failure "POST /v3/volumes/{id}/action has no os-set_bootable case — the server answers HTTP 400 'unknown action', so 'volume set --bootable' and '--non-bootable' both fail; bootable can only be set at create time (--image)"
else
    osc volume show "$V"
    assert_rc 0
    assert_jq '(.bootable == "true" or .bootable == true)' true
    osc volume set --non-bootable "$V"
    assert_rc 0
    osc volume show "$V"
    assert_rc 0
    assert_jq '(.bootable == "false" or .bootable == false)' true
fi
VID=$(_vo_vol_id "$V")
[[ -n "$VID" ]] && OSC_QUIET=1 osc volume delete "$VID"
t_end

t_begin volume.volume.delete
V="cli-conf-vo-delete"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$VID" ]] || t_fail "no volume id in create output"
osc volume delete "$V"
assert_rc 0
osc volume show "$V"
if (( OSC_RC == 0 )); then
    t_fail "volume still visible after delete"
fi
t_end

t_begin volume.volume.delete.neg
osc volume delete cli-conf-vo-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent volume must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin volume.volume.create.neg
# Invalid sizes must be refused: zero and negative.
osc volume create --size 0 cli-conf-vo-badsize
if (( OSC_RC == 0 )); then
    t_fail "volume create --size 0 must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
osc volume create --size -1 cli-conf-vo-badsize
if (( OSC_RC == 0 )); then
    t_fail "volume create --size -1 must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

# ===========================================================================
# Snapshots
# ===========================================================================

t_begin volume.snapshot.create
V="cli-conf-vo-snapcr-vol"
S="cli-conf-vo-snapcr"
_vo_clean_snapshot "$S"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume snapshot create --volume "$V" "$S"
if (( OSC_RC != 0 )); then
    t_known_failure "POST /v3/snapshots omits 'metadata' from the snapshot object; OSC 7's snapshot formatter raises KeyError('metadata') and exits 1 — the snapshot IS created server-side (asserted below via volume snapshot list)"
else
    assert_rc 0
    assert_jq '.name' "$S"
    assert_jq '.size' 1
    # Upstream Cinder also answers 202 with status 'creating' — the volume
    # becomes 'available' asynchronously. The transition is asserted below
    # via the list, after the server's create path has settled.
    assert_jq '.status' 'creating'
    assert_jq_type '.volume_id' string
fi
osc volume snapshot list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$S\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Status" 'available'
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Size" 1
SID=$(_vo_snap_id "$S")
[[ -n "$SID" ]] || t_fail "snapshot not present in list after create"
[[ -n "$SID" ]] && OSC_QUIET=1 osc volume snapshot delete "$SID"
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.snapshot.list
V="cli-conf-vo-snapls-vol"
S="cli-conf-vo-snapls"
_vo_clean_snapshot "$S"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
OSC_QUIET=1 osc volume snapshot create --volume "$V" "$S" # formatter bug, see volume.snapshot.create
osc volume snapshot list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$S\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Status" 'available'
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Size" 1
SID=$(_vo_snap_id "$S")
[[ -n "$SID" ]] && OSC_QUIET=1 osc volume snapshot delete "$SID"
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.snapshot.show
V="cli-conf-vo-snshw-vol"
S="cli-conf-vo-snshw"
_vo_clean_snapshot "$S"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
OSC_QUIET=1 osc volume snapshot create --volume "$V" "$S" # formatter bug, see volume.snapshot.create
SID=$(_vo_snap_id "$S")
[[ -n "$SID" ]] || t_fail "snapshot not present in list after create"
if [[ -n "$SID" ]]; then
    osc volume snapshot show "$SID"
    if (( OSC_RC != 0 )); then
        t_known_failure "GET /v3/snapshots/{id} omits 'metadata' from the snapshot object; OSC 7's snapshot formatter raises KeyError('metadata') and exits 1"
    else
        assert_rc 0
        assert_jq_type '.' object
        assert_jq '.name' "$S"
        assert_jq '.size' 1
        assert_jq '.status' 'available'
        assert_jq_type '.volume_id' string
    fi
    OSC_QUIET=1 osc volume snapshot delete "$SID"
fi
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.snapshot.show.neg
osc volume snapshot show cli-conf-vo-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent snapshot must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin volume.snapshot.set
V="cli-conf-vo-snsset-vol"
S="cli-conf-vo-snsset"
_vo_clean_snapshot "$S"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
OSC_QUIET=1 osc volume snapshot create --volume "$V" "$S" # formatter bug, see volume.snapshot.create
osc volume snapshot set --description "set by cli-conf volume suite" "$S"
assert_rc 0
osc volume snapshot set --property cli_conf_snap_key=cli_conf_snap_value "$S"
assert_rc 0
osc volume snapshot unset --property cli_conf_snap_key "$S"
assert_rc 0
osc volume snapshot list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$S\")][0].Description" 'set by cli-conf volume suite'
SID=$(_vo_snap_id "$S")
[[ -n "$SID" ]] && OSC_QUIET=1 osc volume snapshot delete "$SID"
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.snapshot.delete
V="cli-conf-vo-snsdel-vol"
S="cli-conf-vo-snsdel"
_vo_clean_snapshot "$S"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
OSC_QUIET=1 osc volume snapshot create --volume "$V" "$S" # formatter bug, see volume.snapshot.create
osc volume snapshot delete "$S"
assert_rc 0
osc volume snapshot list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$S\")] | length" 0
# the owning volume becomes deletable again once its snapshot is gone
osc volume delete "$V"
assert_rc 0
t_end

# ===========================================================================
# Backups
# ===========================================================================

t_begin volume.backup.create
V="cli-conf-vo-bakcr-vol"
B="cli-conf-vo-bakcr"
_vo_clean_backup "$B"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc volume backup create "$V" --name "$B"
assert_rc 0
assert_jq '.name' "$B"
assert_jq_type '.id' string
assert_jq '.volume_id' "$VID"
osc volume backup list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$B\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$B\")][0].Status" 'available'
_vo_clean_backup "$B"
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.backup.list
V="cli-conf-vo-bakls-vol"
B="cli-conf-vo-bakls"
_vo_clean_backup "$B"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume backup create "$V" --name "$B"
assert_rc 0
osc volume backup list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$B\")] | length" 1
assert_jq "[.[] | select((.Name // .name) == \"$B\")][0].Status" 'available'
assert_jq "[.[] | select((.Name // .name) == \"$B\")][0].Size" 1
_vo_clean_backup "$B"
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.backup.show
V="cli-conf-vo-bakshw-vol"
B="cli-conf-vo-bakshw"
_vo_clean_backup "$B"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc volume backup create "$V" --name "$B"
assert_rc 0
BID=$(_vo_backup_id "$B")
[[ -n "$BID" ]] || t_fail "backup not present in list after create"
if [[ -n "$BID" ]]; then
    osc volume backup show "$BID"
    assert_rc 0
    assert_jq_type '.' object
    assert_jq '.name' "$B"
    assert_jq '.status' 'available'
    assert_jq '.size' 1
    assert_jq '.volume_id' "$VID"
    OSC_QUIET=1 osc volume backup delete "$BID"
fi
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.backup.show.byname
V="cli-conf-vo-bakname-vol"
B="cli-conf-vo-bakname"
_vo_clean_backup "$B"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume backup create "$V" --name "$B"
assert_rc 0
osc volume backup show "$B"
if (( OSC_RC != 0 )); then
    t_known_failure "pinned OSC 7.0.0 client bug, not a server divergence: 'volume backup show' calls sdk get_backup() (raw GET /v3/backups/{name-or-id}) with no name fallback, while 'volume backup delete' uses find_backup() — the server correctly answers 404 for a non-UUID reference (the old 500 uuid-cast is fixed), matching real Cinder, which does not resolve names there either. Fixed client-side in OSC 7.3.0 ('Fixed the openstack volume backup show command to show a backup by name', python-openstackclient release notes); this probe self-heals when the pin is raised. By-ID show and by-name delete/list work"
else
    assert_jq '.name' "$B"
    assert_jq '.status' 'available'
fi
_vo_clean_backup "$B"
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.backup.delete
V="cli-conf-vo-bakdel-vol"
B="cli-conf-vo-bakdel"
_vo_clean_backup "$B"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
osc volume backup create "$V" --name "$B"
assert_rc 0
BID=$(_vo_backup_id "$B")
[[ -n "$BID" ]] || t_fail "backup not present in list after create"
if [[ -n "$BID" ]]; then
    osc volume backup delete "$BID"
    assert_rc 0
    osc volume backup list
    assert_rc 0
    assert_jq "[.[] | select((.Name // .name) == \"$B\")] | length" 0
fi
OSC_QUIET=1 osc volume delete "$V"
t_end

# ===========================================================================
# Volume types (admin)
# ===========================================================================

t_begin volume.type.create
T="cli-conf-vo-typecr"
_vo_clean_type "$T"
osc volume type create "$T" --description "cli-conf volume type create probe"
assert_rc 0
assert_jq '.name' "$T"
assert_jq '.description' 'cli-conf volume type create probe'
assert_jq_type '.id' string
osc volume type list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$T\")] | length" 1
_vo_clean_type "$T"
t_end

t_begin volume.type.list
T="cli-conf-vo-typels"
_vo_clean_type "$T"
osc volume type create "$T" --description "cli-conf volume type list probe"
assert_rc 0
osc volume type list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Name // .name) == \"$T\")] | length" 1
# the seeded shared type is always present
assert_jq "[.[] | select((.Name // .name) == \"$VO_TYPE\")] | length" 1
_vo_clean_type "$T"
t_end

t_begin volume.type.show
T="cli-conf-vo-typeshw"
_vo_clean_type "$T"
osc volume type create "$T" --description "cli-conf volume type show probe"
assert_rc 0
osc volume type show "$T"
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' "$T"
assert_jq '.description' 'cli-conf volume type show probe'
assert_jq '.is_public' true
assert_jq_type '.id' string
_vo_clean_type "$T"
t_end

t_begin volume.type.show.neg
osc volume type show cli-conf-vo-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent volume type must not exit 0"
else
    [[ -n "$OSC_ERR" ]] || t_fail "expected an error message on stderr"
fi
t_end

t_begin volume.type.set
T="cli-conf-vo-typeset"
_vo_clean_type "$T"
osc volume type create "$T" --description "before set"
assert_rc 0
osc volume type set --description "after set" "$T"
assert_rc 0
osc volume type show "$T"
assert_rc 0
assert_jq '.description' 'after set'
_vo_clean_type "$T"
t_end

t_begin volume.type.delete
T="cli-conf-vo-typedel"
_vo_clean_type "$T"
osc volume type create "$T" --description "cli-conf volume type delete probe"
assert_rc 0
osc volume type delete "$T"
assert_rc 0
osc volume type list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$T\")] | length" 0
t_end

t_begin volume.type.set.property
T="cli-conf-vo-tsprop"
_vo_clean_type "$T"
osc volume type create "$T" --description "extra specs probe"
assert_rc 0
osc volume type set --property cli_conf_spec=spec_value "$T"
assert_rc 0
osc volume type show "$T"
assert_rc 0
if [[ "$(printf '%s' "$OSC_OUT" | jq -r '.properties.cli_conf_spec // empty')" != "spec_value" ]]; then
    t_known_failure "type extra-spec writes persist (GET /v3/types/{id}/extra_specs returns them) but GET /v3/types/{id} omits extra_specs from the type object, so OSC's 'volume type show' reports properties: {} after 'volume type set --property'"
else
    assert_jq '.properties.cli_conf_spec' 'spec_value'
fi
osc volume type unset --property cli_conf_spec "$T"
assert_rc 0
_vo_clean_type "$T"
t_end

t_begin volume.type.create.nodesc
# Creating a type without --description used to store NULL description, which
# made GET /v3/types 500 for every caller until the type was deleted; the
# scan is null-safe now, and this case proves it stays that way (recording
# the old divergence if it ever regresses) — cleanup runs either way.
T="cli-conf-vo-nodesc"
_vo_clean_type "$T"
osc volume type create "$T"
assert_rc 0
TID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
[[ -n "$TID" ]] || t_fail "no type id in create output"
osc volume type list
if (( OSC_RC != 0 )); then
    t_known_failure "volume type create without --description stores a NULL description and GET /v3/types then fails HTTP 500 'failed to read results' (ListVolumeTypes scans description into a non-nullable string) for every caller until the type is deleted — the type itself is created fine"
else
    assert_jq "[.[] | select((.Name // .name) == \"$T\")] | length" 1
fi
# cleanup must run regardless of the outcome above: deleting the type restores
# list health, which the final assertion proves.
if [[ -n "$TID" ]]; then
    OSC_QUIET=1 osc volume type delete "$TID"
fi
osc volume type list
assert_rc 0
assert_jq "[.[] | select((.Name // .name) == \"$T\")] | length" 0
t_end

# ===========================================================================
# Quotas (admin) — set on the shared cli-conf project, always restored
# ===========================================================================

t_begin volume.quota.show
osc quota show --volume "$QPROJECT"
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select(.Resource == "volumes")] | length' 1
assert_jq '[.[] | select(.Resource == "volumes")][0].Limit | type' 'number'
assert_jq '[.[] | select(.Resource == "snapshots")] | length' 1
assert_jq '[.[] | select(.Resource == "backups")] | length' 1
t_end

t_begin volume.quota.set
osc quota show --volume "$QPROJECT"
assert_rc 0
ORIG=$(printf '%s' "$OSC_OUT" | jq -r '[.[] | select(.Resource == "volumes")][0].Limit // empty')
if ! [[ "$ORIG" =~ ^[0-9]+$ ]]; then
    t_fail "no numeric volumes quota limit for project $QPROJECT"
else
    NEW=$(( ORIG + 1 ))
    osc quota set --volumes "$NEW" "$QPROJECT"
    assert_rc 0
    osc quota show --volume "$QPROJECT"
    assert_rc 0
    assert_jq '[.[] | select(.Resource == "volumes")][0].Limit' "$NEW"
    # restore the pre-existing limit, then prove it stuck
    osc quota set --volumes "$ORIG" "$QPROJECT"
    assert_rc 0
    osc quota show --volume "$QPROJECT"
    assert_rc 0
    assert_jq '[.[] | select(.Resource == "volumes")][0].Limit' "$ORIG"
fi
t_end

# ===========================================================================
# Services (admin, read-only)
# ===========================================================================

t_begin volume.service.list
osc volume service list
assert_rc 0
assert_jq_type '.' array
assert_jq 'length > 0' true
assert_jq '[.[] | has("Binary") and has("Host") and has("Zone") and has("Status") and has("State")] | all' true
assert_jq '[.[] | select(.Binary == "cinder-volume")] | length' 1
t_end

# ===========================================================================
# Volume transfers
# ===========================================================================

t_begin volume.transfer.request
V="cli-conf-vo-transfer"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc volume transfer request create "$V"
assert_rc 0
assert_jq_type '.id' string
assert_jq '.auth_key'
assert_jq '.volume_id' "$VID"
TRID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
# a volume with a pending transfer is locked for other mutations
osc volume show "$V"
assert_rc 0
assert_jq '.status' 'awaiting-transfer'
osc volume transfer request list
assert_rc 0
assert_jq_type '.' array
assert_jq "[.[] | select((.Volume // .volume_id) == \"$VID\")] | length" 1
osc volume transfer request show "$TRID"
assert_rc 0
assert_jq '.id' "$TRID"
assert_jq '.volume_id' "$VID"
osc volume transfer request delete "$TRID"
assert_rc 0
osc volume transfer request list
assert_rc 0
assert_jq "[.[] | select((.Volume // .volume_id) == \"$VID\")] | length" 0
# cancelling the transfer releases the volume again
osc volume show "$V"
assert_rc 0
assert_jq '.status' 'available'
OSC_QUIET=1 osc volume delete "$V"
t_end

t_begin volume.transfer.accept
# Same-project accept: a cross-project accept needs a token scoped to the
# receiving project, and the admin user cannot scope to cli-conf today
# (keystone answers 403), so the endpoint is exercised with the creating
# project as acceptor.
V="cli-conf-vo-accept"
_vo_clean_volume "$V"
osc volume create --size 1 "$V"
assert_rc 0
VID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
osc volume transfer request create "$V"
assert_rc 0
TRID=$(printf '%s' "$OSC_OUT" | jq -r '.id // empty')
AUTHKEY=$(printf '%s' "$OSC_OUT" | jq -r '.auth_key // empty')
[[ -n "$AUTHKEY" ]] || t_fail "no auth_key in transfer create output"
if [[ -n "$AUTHKEY" && -n "$TRID" ]]; then
    osc volume transfer request accept "$TRID" --auth-key "$AUTHKEY"
    assert_rc 0
    osc volume show "$V"
    assert_rc 0
    assert_jq '.status' 'available'
    osc volume transfer request list
    assert_rc 0
    assert_jq "[.[] | select((.Volume // .volume_id) == \"$VID\")] | length" 0
fi
OSC_QUIET=1 osc volume delete "$V"
t_end

suite_end
