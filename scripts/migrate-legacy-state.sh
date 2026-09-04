#!/bin/bash
#
# Lightstack — one-time migration of a pre-rename node's local state
#
# The product rename (cobaltcore-dev/cloud-storage#575) moved the node-local state root.
# pkg/paths.Base is a compile-time constant, so an upgraded binary writes to the new
# root and stops seeing the image bases, volume files, instance overlays, cloud-init
# ISOs and dnsmasq state a pre-rename install left behind. Nothing persisted a path —
# every location is derived from the resource id — so the data only has to move once.
#
# Reports what it would do and exits; pass --apply to actually move anything.
#
# Usage: sudo ./scripts/migrate-legacy-state.sh [--apply]
#

set -euo pipefail

# The one place the pre-rename product name is spelled out. Everything else is derived
# from it, and the test harness overrides the roots directly.
LEGACY_NAME="${LIGHTSTACK_LEGACY_NAME:-o3k}"

# Node-local state (data). These move.
STATE_ROOT="${LIGHTSTACK_STATE_ROOT:-/var/lib/lightstack}"
LEGACY_STATE_ROOT="${LIGHTSTACK_LEGACY_STATE_ROOT:-/var/lib/$LEGACY_NAME}"
RUN_ROOT="${LIGHTSTACK_RUN_ROOT:-/var/run/lightstack}"
LEGACY_RUN_ROOT="${LIGHTSTACK_LEGACY_RUN_ROOT:-/var/run/$LEGACY_NAME}"

# Install layout. These are only reported — moving a checkout, rewriting a unit file or
# renaming a system account is not something this script should decide for an operator.
LEGACY_INSTALL_ROOT="${LIGHTSTACK_LEGACY_INSTALL_ROOT:-/opt/$LEGACY_NAME}"
LEGACY_ETC_DIR="/etc/$LEGACY_NAME"
LEGACY_ENV_FILE="/etc/default/$LEGACY_NAME"
LEGACY_BINARY="/usr/local/bin/$LEGACY_NAME"
LEGACY_UNIT="$LEGACY_NAME.service"

# Service account the migrated state must end up owned by. `mv` is a rename, so it keeps the
# previous account's UID/GID, while the unit installed today runs as another — the service
# would then be unable to write its own state. Read from the installed unit when there is
# one, so a host running under a deliberately different account is not overridden.
SERVICE_USER="${LIGHTSTACK_SERVICE_USER:-}"
SERVICE_GROUP="${LIGHTSTACK_SERVICE_GROUP:-}"
INSTALLED_UNIT="${LIGHTSTACK_INSTALLED_UNIT:-/etc/systemd/system/lightstack.service}"

# Legacy iptables chains. Both prefixes are derived from LEGACY_NAME (upper-cased): the
# pre-rename namers produced "<NAME>-SG-<id>" for security groups and "<NAME>-NSI-<ip>" for
# the per-fixed-IP north-south ingress filters.
LEGACY_UPPER="$(printf '%s' "$LEGACY_NAME" | tr '[:lower:]' '[:upper:]')"
LEGACY_CHAIN_PREFIXES="${LEGACY_UPPER}-SG- ${LEGACY_UPPER}-NSI-"
IPTABLES_CMD="${LIGHTSTACK_IPTABLES:-iptables}"

# Mirrors pkg/paths/paths.go — the subdirs the product derives paths under.
STATE_SUBDIRS="images volumes instances cloud-init dhcp"

APPLY=false
# Set when this run actually moved something, so the re-own step stays out of the way on a
# host with nothing to migrate.
MIGRATED=false
# Set when a path exists under both roots. The run is not a success while one remains.
CONFLICTS=false
# Set when a legacy firewall rule or chain could not be removed. A stale ingress chain sits
# ahead of the current one in FORWARD and ends in DROP, so leaving one behind while
# reporting success lets automation continue against a host that still filters on the old
# rules.
FIREWALL_DIRTY=false

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_warning() { echo -e "${YELLOW}[WARNING]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; }

show_help() {
    resolve_service_account
    cat <<EOF
Lightstack legacy state migration

Moves a pre-rename node's local state into the current state root, merging file by file so
a partially migrated directory is completed rather than skipped. Nothing already at the
destination is overwritten; a path present under both roots is reported and makes the run
exit non-zero, because the script cannot choose between them.

Usage: sudo $0 [--apply]

  --apply     Perform the migration. Without it, the script only reports its plan.
  --help,-h   This message.

Moves:
  $LEGACY_STATE_ROOT  ->  $STATE_ROOT   ($STATE_SUBDIRS)
  $LEGACY_RUN_ROOT  ->  $RUN_ROOT

After moving, the state is handed to the service account the installed unit runs as
($SERVICE_USER here). A move is a rename and keeps the previous account's UID/GID, so
without this the new service cannot write its own state. If that account does not exist the
script prints the chown to run later rather than failing.

Removes (needs root): pre-rename iptables chains $LEGACY_CHAIN_PREFIXES and the jumps to
them. The north-south ones are still reachable from FORWARD on a host that has not
rebooted, and they end in DROP — so they keep deciding what reaches a VM while the current
chains sit unreachable behind them.

Reports only (never moved): $LEGACY_INSTALL_ROOT, $LEGACY_ETC_DIR, $LEGACY_ENV_FILE,
$LEGACY_BINARY, $LEGACY_UNIT, the $LEGACY_NAME system account.
EOF
}

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --apply) APPLY=true; shift ;;
            --help|-h) show_help; exit 0 ;;
            *) log_error "Unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

# Only the real host paths need root; the test harness points every root at a fixture.
require_root() {
    if [ -n "${LIGHTSTACK_STATE_ROOT:-}" ]; then
        return
    fi
    if [ "$(id -u)" -ne 0 ]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi
}

# Moving state out from under a running service loses whatever it writes next.
require_services_stopped() {
    if ! command -v systemctl >/dev/null 2>&1; then
        return
    fi
    local unit
    for unit in lightstack.service "$LEGACY_UNIT"; do
        if systemctl is-active --quiet "$unit" 2>/dev/null; then
            log_error "$unit is still active — stop it before migrating:"
            log_error "  systemctl stop $unit"
            exit 1
        fi
    done
}

# Merge $1 into $2 file by file, never overwriting anything already at the destination.
#
# Per FILE, not per directory. `mv -n` refuses a whole directory whose name already exists
# at the destination, so a single partially-copied instance directory would strand every
# child the destination happens to lack — permanently, because each re-run refuses it
# again. That state is not exotic: the chart mounts the two roots as separate volumes, so
# rename() returns EXDEV and every move there is an interruptible copy.
#
# Anything still in $1 afterwards is a path that exists on both sides. The script cannot
# choose between them, so those are collected and reported rather than silently abandoned.
move_dir_contents() {
    local src="$1" dst="$2"
    if [ -z "$(ls -A "$src" 2>/dev/null)" ]; then
        return
    fi
    mkdir -p "$dst"

    # Mirror the directory structure first so every file has somewhere to land.
    local rel
    find "$src" -mindepth 1 -type d | while IFS= read -r dir; do
        rel="${dir#"$src"/}"
        mkdir -p "$dst/$rel"
    done

    # Then move the files themselves; -n keeps whatever the destination already holds.
    find "$src" -mindepth 1 \( -type f -o -type l \) | while IFS= read -r file; do
        rel="${file#"$src"/}"
        mv -n "$file" "$dst/$rel" 2>/dev/null || true
    done

    # Emptied directories are just scaffolding; drop them so leftovers stand out.
    find "$src" -mindepth 1 -type d -empty -delete 2>/dev/null || true
}

migrate_state_root() {
    if [ ! -d "$LEGACY_STATE_ROOT" ]; then
        log_info "No legacy state root at $LEGACY_STATE_ROOT — nothing to migrate"
        return
    fi

    local sub
    for sub in $STATE_SUBDIRS; do
        if [ ! -d "$LEGACY_STATE_ROOT/$sub" ]; then
            continue
        fi
        local count
        count="$(find "$LEGACY_STATE_ROOT/$sub" -mindepth 1 -maxdepth 1 | wc -l)"
        if [ "$count" -eq 0 ]; then
            continue
        fi
        if [ "$APPLY" = true ]; then
            log_info "Moving $count entries: $LEGACY_STATE_ROOT/$sub -> $STATE_ROOT/$sub"
            move_dir_contents "$LEGACY_STATE_ROOT/$sub" "$STATE_ROOT/$sub"
            MIGRATED=true
        else
            log_info "Would move $count entries: $LEGACY_STATE_ROOT/$sub -> $STATE_ROOT/$sub"
        fi
    done
}

migrate_run_root() {
    if [ ! -d "$LEGACY_RUN_ROOT" ]; then
        return
    fi
    if [ -z "$(ls -A "$LEGACY_RUN_ROOT" 2>/dev/null)" ]; then
        return
    fi
    if [ "$APPLY" = true ]; then
        log_info "Moving runtime state: $LEGACY_RUN_ROOT -> $RUN_ROOT"
        move_dir_contents "$LEGACY_RUN_ROOT" "$RUN_ROOT"
        MIGRATED=true
    else
        log_info "Would move runtime state: $LEGACY_RUN_ROOT -> $RUN_ROOT"
    fi
}

# Remove the pre-rename iptables chains and the jumps that reference them.
#
# The security-group chains are unreferenced — nothing ever jumped to them — so they are
# only litter. The north-south ingress chains are the reason this matters: they are jumped
# from FORWARD and end in DROP, and the current code appends its own jump *after* whatever
# is already there. On a host that has not rebooted since the upgrade the stale chain
# therefore still decides what reaches a VM, while every rule written since lands in a chain
# traffic never reaches.
#
# Jumps go before chains: iptables refuses to delete a referenced chain ("Resource busy").
migrate_legacy_iptables() {
    # Only touch a firewall on a real root run, or when the command has been pointed
    # elsewhere (the test harness). Never guess on somebody's live ruleset.
    if [ -z "${LIGHTSTACK_IPTABLES:-}" ] && [ "$(id -u)" -ne 0 ]; then
        log_warning "Not root — skipping the legacy iptables chain check"
        return
    fi
    if ! command -v "$IPTABLES_CMD" >/dev/null 2>&1; then
        log_info "No iptables here — no legacy chains to remove"
        return
    fi

    local ruleset
    ruleset="$("$IPTABLES_CMD" -t filter -S 2>/dev/null || true)"
    if [ -z "$ruleset" ]; then
        return
    fi

    local prefix chains chain jumps rule
    chains=""
    for prefix in $LEGACY_CHAIN_PREFIXES; do
        chains="$chains $(echo "$ruleset" | sed -n "s/^-N \($prefix[^ ]*\).*/\1/p")"
    done

    for chain in $chains; do
        if [ -z "$chain" ]; then
            continue
        fi
        jumps="$(echo "$ruleset" | grep -E -- "^-A [A-Za-z0-9_-]+ .*-j $chain\$" || true)"
        if [ "$APPLY" != true ]; then
            log_info "Would remove legacy chain $chain and $(printf '%s' "$jumps" | grep -c . || true) jump(s) to it"
            continue
        fi
        log_info "Removing legacy chain $chain"
        echo "$jumps" | while read -r rule; do
            if [ -z "$rule" ]; then
                continue
            fi
            # Word-splitting is intended: the rule spec becomes separate arguments.
            # shellcheck disable=SC2086
            if ! "$IPTABLES_CMD" -t filter -D ${rule#-A } >/dev/null 2>&1; then
                log_error "could not remove jump: ${rule#-A }"
                FIREWALL_DIRTY=true
            fi
        done
        # Not tolerated: a chain that still holds its old rules and is still reachable is
        # the case where traffic keeps being decided by the pre-rename ruleset.
        if ! "$IPTABLES_CMD" -t filter -F "$chain" >/dev/null 2>&1; then
            log_error "could not flush chain $chain — it may still be filtering traffic"
            FIREWALL_DIRTY=true
        fi
        if ! "$IPTABLES_CMD" -t filter -X "$chain" >/dev/null 2>&1; then
            log_error "could not delete chain $chain — is a jump to it still present?"
            FIREWALL_DIRTY=true
        fi
    done

    verify_no_legacy_firewall_rules
}

# A command can return 0 and still leave a rule in place, so the only trustworthy check is
# to look again. Anything still carrying a legacy prefix is named and fails the run.
verify_no_legacy_firewall_rules() {
    local ruleset survivors prefix
    ruleset="$("$IPTABLES_CMD" -t filter -S 2>/dev/null || true)"
    if [ -z "$ruleset" ]; then
        return
    fi
    survivors=""
    for prefix in $LEGACY_CHAIN_PREFIXES; do
        survivors="$survivors$(echo "$ruleset" | grep -E -- "(^-N $prefix|-j $prefix)" || true)
"
    done
    survivors="$(printf '%s' "$survivors" | grep -v '^$' || true)"
    if [ -n "$survivors" ]; then
        FIREWALL_DIRTY=true
        log_error "Legacy firewall rules are still present after cleanup:"
        echo "$survivors" | sed 's/^/  /'
    fi
}

# Resolve the account the state should belong to: an explicit override, else the installed
# unit's User=/Group=, else the product default.
resolve_service_account() {
    if [ -z "$SERVICE_USER" ] && [ -f "$INSTALLED_UNIT" ]; then
        SERVICE_USER="$(sed -n 's/^User=//p' "$INSTALLED_UNIT" | head -1)"
        SERVICE_GROUP="${SERVICE_GROUP:-$(sed -n 's/^Group=//p' "$INSTALLED_UNIT" | head -1)}"
    fi
    SERVICE_USER="${SERVICE_USER:-lightstack}"
    SERVICE_GROUP="${SERVICE_GROUP:-$SERVICE_USER}"
}

# Hand the migrated state to that account. Only when the account exists: nothing in this
# repository creates it, so on a host where it is absent the right move is to say what to
# run rather than invent an owner or fail a migration that otherwise succeeded.
reown_migrated_state() {
    if [ "$MIGRATED" != true ]; then
        return
    fi
    resolve_service_account

    local target
    target="$SERVICE_USER:$SERVICE_GROUP"
    if ! id "$SERVICE_USER" >/dev/null 2>&1; then
        log_warning "Service account $SERVICE_USER does not exist — migrated state still belongs"
        log_warning "to the account that wrote it. Once the account exists, run:"
        echo "  chown -R $target $STATE_ROOT $RUN_ROOT"
        return
    fi

    log_info "Re-owning migrated state to $target"
    local root
    for root in "$STATE_ROOT" "$RUN_ROOT"; do
        if [ ! -d "$root" ]; then
            continue
        fi
        if ! chown -R "$target" "$root"; then
            log_warning "could not re-own $root to $target — run: chown -R $target $root"
        fi
    done

    log_warning "VM disk access: qemu may run under its own account. If a guest fails to start"
    log_warning "after this, check the overlay under $STATE_ROOT/instances is reachable by it."
}

# Any file still under the legacy root after the merge exists at the destination too. The
# script will not pick a winner, and reporting success while data sits in an abandoned root
# is what lets automation carry on believing the migration finished.
report_state_leftovers() {
    if [ ! -d "$LEGACY_STATE_ROOT" ]; then
        return
    fi
    local leftovers
    leftovers="$(find "$LEGACY_STATE_ROOT" -mindepth 1 \( -type f -o -type l \) 2>/dev/null || true)"
    if [ -n "$leftovers" ]; then
        CONFLICTS=true
        log_error "These paths exist under BOTH roots, so nothing was overwritten:"
        echo "$leftovers" | sed "s|^$LEGACY_STATE_ROOT/|  |"
        log_error "Compare each against $STATE_ROOT, keep the one you want, remove the other."
    fi
}

# The install layout is the operator's to change: a git checkout, a unit file and a
# system account are not safely reversible from a script mid-upgrade.
report_install_layout() {
    local found=false
    local path
    for path in "$LEGACY_INSTALL_ROOT" "$LEGACY_INSTALL_ROOT-backups" \
                "$LEGACY_ETC_DIR" "$LEGACY_ENV_FILE" "$LEGACY_BINARY"; do
        if [ -e "$path" ]; then
            found=true
            log_warning "Legacy install path still present: $path"
        fi
    done
    if command -v systemctl >/dev/null 2>&1 &&
       systemctl list-unit-files "$LEGACY_UNIT" >/dev/null 2>&1 &&
       [ -n "$(systemctl list-unit-files "$LEGACY_UNIT" --no-legend 2>/dev/null)" ]; then
        found=true
        log_warning "Legacy systemd unit still installed: $LEGACY_UNIT"
    fi
    if id "$LEGACY_NAME" >/dev/null 2>&1; then
        found=true
        log_warning "Legacy system account still present: $LEGACY_NAME"
    fi

    if [ "$found" = true ]; then
        echo ""
        log_info "These are left alone on purpose. Finish the rename by hand:"
        echo "  systemctl disable --now $LEGACY_UNIT"
        echo "  mv $LEGACY_INSTALL_ROOT /opt/lightstack        # git-checkout installs"
        echo "  mv $LEGACY_ETC_DIR /etc/lightstack             # packaged installs"
        echo "  mv $LEGACY_ETC_DIR/$LEGACY_NAME.yaml /etc/lightstack/lightstack.yaml"
        echo "  mv $LEGACY_ENV_FILE /etc/default/lightstack"
        echo "  cp deployments/systemd/lightstack.service /etc/systemd/system/"
        echo "  systemctl daemon-reload && systemctl enable --now lightstack.service"
        echo ""
        log_info "See docs/UPGRADING.md for the full list."
    fi
}

main() {
    parse_args "$@"
    require_root

    if [ "$APPLY" != true ]; then
        log_warning "Dry run — nothing will be moved. Re-run with --apply to migrate."
    fi
    echo ""

    if [ "$APPLY" = true ]; then
        require_services_stopped
    fi

    migrate_state_root
    migrate_run_root
    reown_migrated_state
    migrate_legacy_iptables
    echo ""
    report_state_leftovers
    report_install_layout

    if [ "$APPLY" != true ]; then
        return 0
    fi
    if [ "$CONFLICTS" = true ] || [ "$FIREWALL_DIRTY" = true ]; then
        log_error "Legacy state migration INCOMPLETE — resolve the items above and re-run."
        return 1
    fi
    log_success "Legacy state migration complete"
}

main "$@"
