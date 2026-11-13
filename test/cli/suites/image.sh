#!/bin/bash
# image.sh — SPEC-028 tier A/B conformance cases for Glance (Image service).
#
# Covers manifest/image.json: image lifecycle (create from a local file, list
# with filters, show, set base properties, save with content verification,
# delete) and the image sharing member family (add / list / get / remove),
# against the fixture project `cli-conf`.
#
# Every case creates only resources named `cli-conf-im-<slug>` and removes
# them before it ends, so the suite is rerun-safe.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin image
# Standalone rerun-safety: this suite references the shared fixture
# project cli-conf by name. bin/run.sh ensures it and its teardown sweeps
# it, so running this suite alone after a sweep re-ensures here instead of
# failing with confusing missing-resource errors.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/fixtures.sh"
fixtures_require

# ---------------------------------------------------------------------------
# Suite-local fixtures: one 64 KiB file of zeros, uploaded by the create cases
# and compared byte-for-byte by the save case. MD5 (Glance's checksum) is
# computed once. Removed by the EXIT trap (suite_cleanup, common.sh), so an
# early exit or a killed run cannot leak it either.
# ---------------------------------------------------------------------------
IM_TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/cli-conf-im.XXXXXX")"
suite_cleanup() {
    rm -rf "$IM_TMP_DIR"
}
IM_IMG_FILE="$IM_TMP_DIR/im64k.bin"
IM_IMG_MD5=""
dd if=/dev/zero of="$IM_IMG_FILE" bs=1024 count=64 2>/dev/null
IM_IMG_MD5="$(md5sum "$IM_IMG_FILE" | awk '{print $1}')"

# The sharing cases need the second fixture project (by ID: `image member get`
# passes its argument verbatim into the member URL and does not resolve names).
IM_SHARE_PROJECT="cli-conf"
IM_SHARE_PROJECT_ID=""
OSC_QUIET=1 osc project show "$IM_SHARE_PROJECT"
if (( OSC_RC == 0 )); then
    IM_SHARE_PROJECT_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
else
    log_error "fixture project '$IM_SHARE_PROJECT' not found — sharing cases will fail: $(_snip "$OSC_ERR")"
fi

# Best-effort leftover removal (rerun safety): a delete of a missing image is
# allowed to fail silently.
_im_cleanup_image() {
    OSC_QUIET=1 osc image delete "$1"
}

# Create a fresh image for a case. Assumes the leftover with the same name was
# removed just before. Leaves OSC_RC / OSC_OUT from the create call.
_im_create() {
    osc image create "$1" \
        --disk-format raw --container-format bare --file "$IM_IMG_FILE"
}

# --- image.create -----------------------------------------------------------
t_begin image.create
_im_cleanup_image cli-conf-im-create
_im_create cli-conf-im-create
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' cli-conf-im-create
assert_jq '.id'
# disk/container format come back exactly as requested (stub storage stores
# bytes as-is, so no raw normalization applies).
assert_jq '.disk_format' raw
assert_jq '.container_format' bare
assert_jq '.visibility' private
# The create response itself is the pre-upload record (status "queued", CLI
# convention); the upload the CLI performs right after must flip it active.
assert_jq '.status'
osc image show cli-conf-im-create
assert_rc 0
assert_jq '.status' active
assert_jq '.size' 65536
assert_jq '.checksum' "$IM_IMG_MD5"
osc image delete cli-conf-im-create
assert_rc 0
t_end

# --- image.show --------------------------------------------------------------
t_begin image.show
_im_cleanup_image cli-conf-im-show
_im_create cli-conf-im-show
assert_rc 0
osc image show cli-conf-im-show
assert_rc 0
assert_jq_type '.' object
assert_jq '.name' cli-conf-im-show
assert_jq '.status' active
assert_jq '.disk_format' raw
assert_jq '.container_format' bare
assert_jq '.size' 65536
assert_jq '.checksum' "$IM_IMG_MD5"
assert_jq '.visibility' private
assert_jq_type '.min_disk' number
assert_jq_type '.min_ram' number
assert_jq '.created_at'
assert_jq '.updated_at'
# Show again by ID (the canonical Glance lookup key).
IM_SHOW_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
osc image show "$IM_SHOW_ID"
assert_rc 0
assert_jq '.name' cli-conf-im-show
osc image delete "$IM_SHOW_ID"
assert_rc 0
t_end

# --- image.show (negative) ---------------------------------------------------
t_begin image.show.neg
osc image show cli-conf-im-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "show of a nonexistent image must not exit 0"
elif [[ -z "$OSC_ERR" ]]; then
    t_fail "non-zero exit but no error message on stderr"
fi
t_end

# --- image.list --------------------------------------------------------------
t_begin image.list
_im_cleanup_image cli-conf-im-list
_im_create cli-conf-im-list
assert_rc 0
osc image list
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "cli-conf-im-list")] | length' 1
# --status filter: our image is active, so a queued filter must exclude it.
osc image list --status active
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "cli-conf-im-list")] | length' 1
osc image list --status queued
assert_rc 0
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "cli-conf-im-list")] | length' 0
osc image delete cli-conf-im-list
assert_rc 0
t_end

# --- image.list (negative) ---------------------------------------------------
t_begin image.list.neg
# An unknown marker id is refused with 400 by the server (it must not be
# silently ignored — that would hide pagination bugs).
osc image list --marker 00000000-0000-0000-0000-000000000000
if (( OSC_RC == 0 )); then
    t_fail "list with an unknown marker id must not exit 0"
elif [[ -z "$OSC_ERR" ]]; then
    t_fail "non-zero exit but no error message on stderr"
fi
t_end

# --- image.set ---------------------------------------------------------------
t_begin image.set
_im_cleanup_image cli-conf-im-set
_im_cleanup_image cli-conf-im-set-renamed
_im_create cli-conf-im-set
assert_rc 0
osc image set cli-conf-im-set --name cli-conf-im-set-renamed --min-disk 1 --min-ram 256
assert_rc 0
osc image show cli-conf-im-set-renamed
assert_rc 0
assert_jq '.name' cli-conf-im-set-renamed
assert_jq '.min_disk' 1
assert_jq '.min_ram' 256
# Visibility is a PATCH-writable base property too.
osc image set cli-conf-im-set-renamed --shared
assert_rc 0
osc image show cli-conf-im-set-renamed
assert_rc 0
assert_jq '.visibility' shared
osc image delete cli-conf-im-set-renamed
assert_rc 0
t_end

# --- image.set (negative) ----------------------------------------------------
t_begin image.set.neg
osc image set cli-conf-im-does-not-exist --name still-nothing
if (( OSC_RC == 0 )); then
    t_fail "set on a nonexistent image must not exit 0"
elif [[ -z "$OSC_ERR" ]]; then
    t_fail "non-zero exit but no error message on stderr"
fi
t_end

# --- image.save --------------------------------------------------------------
t_begin image.save
_im_cleanup_image cli-conf-im-save
_im_create cli-conf-im-save
assert_rc 0
IM_SAVE_DL="$IM_TMP_DIR/im64k.downloaded"
osc image save cli-conf-im-save --file "$IM_SAVE_DL"
assert_rc 0
# The roundtrip MUST be byte-identical to what was uploaded.
if [[ ! -f "$IM_SAVE_DL" ]]; then
    t_fail "image save did not write '$IM_SAVE_DL'"
elif ! cmp -s "$IM_IMG_FILE" "$IM_SAVE_DL"; then
    t_fail "downloaded bytes differ from uploaded file (cmp failed)"
fi
# And the downloaded bytes match the image's recorded checksum.
if [[ -f "$IM_SAVE_DL" ]]; then
    DL_MD5="$(md5sum "$IM_SAVE_DL" | awk '{print $1}')"
    if [[ "$DL_MD5" != "$IM_IMG_MD5" ]]; then
        t_fail "md5 of downloaded file ($DL_MD5) != md5 of uploaded file ($IM_IMG_MD5)"
    fi
fi
rm -f "$IM_SAVE_DL"
osc image delete cli-conf-im-save
assert_rc 0
t_end

# --- image.delete ------------------------------------------------------------
t_begin image.delete
_im_cleanup_image cli-conf-im-delete
_im_create cli-conf-im-delete
assert_rc 0
osc image delete cli-conf-im-delete
assert_rc 0
# Deletion is proven by the show failing afterwards.
osc image show cli-conf-im-delete
if (( OSC_RC == 0 )); then
    t_fail "image still exists after delete"
fi
t_end

# --- image.delete (negative) -------------------------------------------------
t_begin image.delete.neg
osc image delete cli-conf-im-does-not-exist
if (( OSC_RC == 0 )); then
    t_fail "delete of a nonexistent image must not exit 0"
elif [[ -z "$OSC_ERR" ]]; then
    t_fail "non-zero exit but no error message on stderr"
fi
t_end

# --- image.member.add --------------------------------------------------------
t_begin image.member.add
_im_cleanup_image cli-conf-im-madd
_im_create cli-conf-im-madd
assert_rc 0
if [[ -z "$IM_SHARE_PROJECT_ID" ]]; then
    t_fail "fixture project cli-conf id unknown (project show failed earlier)"
else
    osc image set cli-conf-im-madd --shared
    assert_rc 0
    osc image add project cli-conf-im-madd "$IM_SHARE_PROJECT"
    assert_rc 0
    assert_jq_type '.' object
    assert_jq '.image_id'
    assert_jq '.member_id' "$IM_SHARE_PROJECT_ID"
    assert_jq '.status' pending
    assert_jq '.created_at'
fi
osc image delete cli-conf-im-madd
assert_rc 0
t_end

# --- image.member.list -------------------------------------------------------
t_begin image.member.list
_im_cleanup_image cli-conf-im-mlist
_im_create cli-conf-im-mlist
assert_rc 0
if [[ -z "$IM_SHARE_PROJECT_ID" ]]; then
    t_fail "fixture project cli-conf id unknown (project show failed earlier)"
else
    IM_MLIST_ID="$(printf '%s' "$OSC_OUT" | jq -r '.id')"
    osc image add project cli-conf-im-mlist "$IM_SHARE_PROJECT"
    assert_rc 0
    osc image member list cli-conf-im-mlist
    assert_rc 0
    assert_jq_type '.' array
    assert_jq '[.[] | select((."Member ID" // .member_id) == "'"$IM_SHARE_PROJECT_ID"'")] | length' 1
    assert_jq '[.[] | select((."Member ID" // .member_id) == "'"$IM_SHARE_PROJECT_ID"'")][0] ."Status"' pending
    assert_jq '[.[] | select((."Member ID" // .member_id) == "'"$IM_SHARE_PROJECT_ID"'")][0] ."Image ID"' "$IM_MLIST_ID"
fi
osc image delete cli-conf-im-mlist
assert_rc 0
t_end

# --- image.member.get --------------------------------------------------------
t_begin image.member.get
_im_cleanup_image cli-conf-im-mget
_im_create cli-conf-im-mget
assert_rc 0
if [[ -z "$IM_SHARE_PROJECT_ID" ]]; then
    t_fail "fixture project cli-conf id unknown (project show failed earlier)"
else
    osc image add project cli-conf-im-mget "$IM_SHARE_PROJECT"
    assert_rc 0
    # The member argument must be the project ID: the CLI passes it verbatim
    # into the member URL and does not resolve project names.
    osc image member get cli-conf-im-mget "$IM_SHARE_PROJECT_ID"
    assert_rc 0
    assert_jq_type '.' object
    assert_jq '.member_id' "$IM_SHARE_PROJECT_ID"
    assert_jq '.status' pending
    assert_jq '.image_id'
    assert_jq '.created_at'
    assert_jq '.updated_at'
fi
osc image delete cli-conf-im-mget
assert_rc 0
t_end

# --- image.member.remove -----------------------------------------------------
t_begin image.member.remove
_im_cleanup_image cli-conf-im-mremove
_im_create cli-conf-im-mremove
assert_rc 0
if [[ -z "$IM_SHARE_PROJECT_ID" ]]; then
    t_fail "fixture project cli-conf id unknown (project show failed earlier)"
else
    osc image add project cli-conf-im-mremove "$IM_SHARE_PROJECT"
    assert_rc 0
    # remove project resolves the project by name or ID.
    osc image remove project cli-conf-im-mremove "$IM_SHARE_PROJECT"
    assert_rc 0
    osc image member list cli-conf-im-mremove
    assert_rc 0
    assert_jq_type '.' array
    assert_jq '[.[] | select((."Member ID" // .member_id) == "'"$IM_SHARE_PROJECT_ID"'")] | length' 0
fi
osc image delete cli-conf-im-mremove
assert_rc 0
t_end

suite_end
