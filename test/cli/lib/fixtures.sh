#!/bin/bash
# fixtures.sh — idempotent shared fixtures + crash-safe sweep (SPEC-028).
#
# Locked API:
#   fixtures_ensure     create-or-get the shared fixtures (project, image,
#                       network+subnet); sweeps leftovers from a crashed prior
#                       run first. Populates FIX_PROJECT_ID, FIX_IMAGE_ID,
#                       FIX_NETWORK_ID, FIX_SUBNET_ID.
#   fixtures_teardown   delete every resource whose name starts with the fixture
#                       prefix (including the fixtures themselves), best-effort.
#
# Conventions every suite MUST follow:
#   - resources created by a case are named  cli-conf-<slug>-<unique>
#     so the sweep can always reclaim them;
#   - the sweep deletes by NAME PREFIX ONLY — never delete by bare ID.

# Guard against double-sourcing.
[[ -n "${_CLI_FIXTURES_SOURCED:-}" ]] && return 0
_CLI_FIXTURES_SOURCED=1

# shellcheck source=common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

FIX_PREFIX="cli-conf"
FIX_PROJECT="$FIX_PREFIX"
FIX_IMAGE="$FIX_PREFIX-image"
FIX_NETWORK="$FIX_PREFIX-net"
FIX_SUBNET="$FIX_PREFIX-subnet"

# ---------------------------------------------------------------------------
# Sweep helpers
# ---------------------------------------------------------------------------

# _ids_by_name_prefix — read list JSON from stdin, print the IDs of entries
# whose Name/name starts with $FIX_PREFIX, one per line.
_ids_by_name_prefix() {
    jq -r --arg p "$FIX_PREFIX" \
        '.[] | select((.Name // .name // "") | startswith($p)) | (.ID // .id)' 2>/dev/null
}

# _sweep_list <label> <list-args...>: run the list command and echo its JSON
# (or nothing when the list itself fails). Args are passed through
# individually — never as one quoted string.
_sweep_list() {
    local label="$1"; shift
    OSC_QUIET=1 osc "$@"
    if (( OSC_RC != 0 )); then
        log_warn "sweep: could not list $label (skipping): $(_snip "$OSC_ERR")"
        printf '' # empty
        return
    fi
    printf '%s' "$OSC_OUT"
}

# _delete_all <label> <json-list-output> <delete-args-template-with-{id}>
_delete_all() {
    # _delete_all <label> <json> <template> [<template>...]
    # Each template is "verb kind {id}"; they are tried in order and the
    # first that succeeds wins, so a sweep can fall back (plain volume
    # delete first, --force for the in-use leftovers, for instance).
    local label="$1" json="$2"
    shift 2
    local templates=("$@")
    local id count=0
    local -a del_args
    while IFS= read -r id; do
        [[ -z "$id" || "$id" == "null" ]] && continue
        local deleted=1
        for tmpl in "${templates[@]}"; do
            # Split "delete <kind> {id}" into argv words; {id} is a UUID and
            # never contains whitespace.
            read -ra del_args <<<"${tmpl/\{id\}/$id}"
            osc "${del_args[@]}"
            if (( OSC_RC == 0 )); then
                deleted=0
                break
            fi
        done
        if (( deleted != 0 )); then
            log_warn "sweep: failed to delete $label $id: $(_snip "$OSC_ERR")"
        else
            count=$(( count + 1 ))
        fi
    done < <(printf '%s' "$json" | _ids_by_name_prefix)
    (( count > 0 )) && log_info "sweep: deleted $count $label"
}

# ---------------------------------------------------------------------------
# Sweep — order matters: dependents before their owners.
# ---------------------------------------------------------------------------

fixtures_sweep() {
    log_info "sweeping leftover resources with prefix '$FIX_PREFIX-*'"

    local out

    # Compute: servers first (they hold ports and volumes).
    out=$(_sweep_list servers server list)
    _delete_all servers "$out" "server delete --wait {id}"

    # Block storage: snapshots/backups reference volumes; volumes may be in-use.
    out=$(_sweep_list snapshots volume snapshot list)
    _delete_all snapshots "$out" "snapshot delete {id}"
    out=$(_sweep_list backups volume backup list)
    _delete_all backups "$out" "backup delete {id}"
    out=$(_sweep_list volumes volume list)
    # Plain delete handles the available volumes; --force is the fallback for
    # in-use ones (os-force-delete, a fallback only because this Lightstack
    # build does not implement it — see the suite's known-failing ledger).
    _delete_all volumes "$out" "volume delete {id}" "volume delete --force {id}"

    # Network: ports, subnets, routers, then networks. Floating IPs are NOT
    # swept (they carry no name); suites that allocate them must release them.
    out=$(_sweep_list ports port list)
    _delete_all ports "$out" "port delete {id}"
    out=$(_sweep_list subnets subnet list)
    _delete_all subnets "$out" "subnet delete {id}"
    out=$(_sweep_list routers router list)
    _delete_all routers "$out" "router delete {id}"
    out=$(_sweep_list networks network list)
    _delete_all networks "$out" "network delete {id}"
    out=$(_sweep_list "security groups" security group list)
    _delete_all "security groups" "$out" "security group delete {id}"

    # Compute auxiliaries.
    out=$(_sweep_list keypairs keypair list)
    _delete_all keypairs "$out" "keypair delete {id}"
    out=$(_sweep_list "server groups" server group list)
    _delete_all "server groups" "$out" "server group delete {id}"

    # Images.
    out=$(_sweep_list images image list)
    _delete_all images "$out" "image delete {id}"

    # Custom flavors created by cases (seeded m1.* never match the prefix).
    out=$(_sweep_list flavors flavor list --all)
    _delete_all flavors "$out" "flavor delete {id}"

    # Identity: users/groups/roles/projects last (they may own the above).
    out=$(_sweep_list users user list)
    _delete_all users "$out" "user delete {id}"
    out=$(_sweep_list groups group list)
    _delete_all groups "$out" "group delete {id}"
    out=$(_sweep_list projects project list)
    _delete_all projects "$out" "project delete {id}"
}

fixtures_teardown() {
    fixtures_sweep
}

# ---------------------------------------------------------------------------
# Ensure — create-or-get, crash-safe.
# ---------------------------------------------------------------------------

# _probe: an existence check whose expected 404 must not warn.
_probe() { OSC_QUIET=1 osc "$@"; }

_ensure_project() {
    _probe project show "$FIX_PROJECT"
    if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
        FIX_PROJECT_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
        return 0
    fi
    osc project create "$FIX_PROJECT" --description "SPEC-028 CLI conformance fixtures"
    if (( OSC_RC != 0 )); then
        log_error "cannot create fixture project: $(_snip "$OSC_ERR")"
        return 1
    fi
    # create output shape differs across CLI versions; re-show for the id.
    _probe project show "$FIX_PROJECT"
    FIX_PROJECT_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
    log_info "fixture project $FIX_PROJECT ($FIX_PROJECT_ID)"
}

_ensure_image() {
    _probe image show "$FIX_IMAGE"
    if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
        FIX_IMAGE_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
        return 0
    fi
    local img
    img=$(mktemp "${TMPDIR:-/tmp}/cli-conf-image.XXXXXX.img")
    # 64 KiB of zeros: enough to be a real stored object in every Glance mode,
    # small enough to be instant, and never downloaded from anywhere.
    dd if=/dev/zero of="$img" bs=1024 count=64 2>/dev/null
    osc image create "$FIX_IMAGE" \
        --disk-format raw --container-format bare --private --file "$img"
    local rc=$OSC_RC
    rm -f "$img"
    if (( rc != 0 )); then
        log_error "cannot create fixture image: $(_snip "$OSC_ERR")"
        return 1
    fi
    _probe image show "$FIX_IMAGE"
    FIX_IMAGE_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
    log_info "fixture image $FIX_IMAGE ($FIX_IMAGE_ID)"
}

_ensure_network() {
    _probe network show "$FIX_NETWORK"
    if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
        FIX_NETWORK_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
    else
        osc network create "$FIX_NETWORK"
        if (( OSC_RC != 0 )); then
            log_error "cannot create fixture network: $(_snip "$OSC_ERR")"
            return 1
        fi
        osc network show "$FIX_NETWORK"
        FIX_NETWORK_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
        log_info "fixture network $FIX_NETWORK ($FIX_NETWORK_ID)"
    fi

    _probe subnet show "$FIX_SUBNET"
    if (( OSC_RC == 0 )) && [[ -n "$OSC_OUT" ]]; then
        FIX_SUBNET_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
        return 0
    fi
    osc subnet create "$FIX_SUBNET" \
        --network "$FIX_NETWORK" --subnet-range 10.99.0.0/24
    if (( OSC_RC != 0 )); then
        log_error "cannot create fixture subnet: $(_snip "$OSC_ERR")"
        return 1
    fi
    _probe subnet show "$FIX_SUBNET"
    FIX_SUBNET_ID=$(printf '%s' "$OSC_OUT" | jq -r '.id')
    log_info "fixture subnet $FIX_SUBNET ($FIX_SUBNET_ID)"
}

fixtures_ensure() {
    log_info "ensuring fixtures (prefix '$FIX_PREFIX')"

    # Auth sanity: everything below is meaningless without it.
    osc token issue
    if (( OSC_RC != 0 )); then
        log_error "authentication failed against $OS_AUTH_URL (user $OS_USERNAME): $(_snip "$OSC_ERR")"
        return 1
    fi

    # Crash recovery: a previous crashed run may have left half-created
    # fixtures behind. Sweep first so ensure is deterministic.
    fixtures_sweep

    _ensure_project || return 1
    _ensure_image || return 1
    _ensure_network || return 1

    # Flavors are seeded by migration 002; warn (not fail) if absent —
    # server-dependent cases will fail loudly on their own.
    _probe flavor show m1.tiny
    if (( OSC_RC != 0 )); then
        log_warn "flavor m1.tiny not found — server cases will fail; is the database seeded?"
    fi
}
