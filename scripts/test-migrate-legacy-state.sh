#!/bin/bash
#
# Test suite for scripts/migrate-legacy-state.sh (cobaltcore-dev/cloud-storage#575).
# Drives the migration against fixture roots — no root, no systemd, no real host state.
#
# Usage: ./scripts/test-migrate-legacy-state.sh
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIGRATE="$SCRIPT_DIR/migrate-legacy-state.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
# Plain arithmetic assignment, not ((x++)): the latter returns 1 when the variable is
# still 0, which under `set -e` kills the run on the very first pass.
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; TESTS_PASSED=$((TESTS_PASSED + 1)); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; TESTS_FAILED=$((TESTS_FAILED + 1)); }

# A fixture pair of roots, populated like a real pre-rename node.
make_fixture() {
    FIX="$(mktemp -d "/tmp/lightstack-migrate-test-$$-XXXX")"
    LEG="$FIX/var/lib/legacy"
    NEW="$FIX/var/lib/lightstack"
    LEG_RUN="$FIX/var/run/legacy"
    NEW_RUN="$FIX/var/run/lightstack"
    mkdir -p "$LEG/images" "$LEG/volumes" "$LEG/instances/iid-1" "$LEG/cloud-init" "$LEG/dhcp" "$LEG_RUN"
    echo base    > "$LEG/images/image-abc.raw"
    echo vol     > "$LEG/volumes/volume-xyz.qcow2"
    echo disk    > "$LEG/instances/iid-1/disk.qcow2"
    echo console > "$LEG/instances/iid-1/console.log"
    echo iso     > "$LEG/cloud-init/iid-1.iso"
    echo lease   > "$LEG/dhcp/net-1.leases"
    echo sock    > "$LEG_RUN/metadata.sock"
}

# Install a fake iptables that records every invocation and answers -S from a fixture.
# The real binary is never involved: the migration only touches iptables when
# LIGHTSTACK_IPTABLES is set (or it runs as root), so the host's rules stay untouched.
make_fake_iptables() {
    IPT_LOG="$FIX/iptables.log"
    IPT_RULESET="$FIX/ruleset.txt"
    : > "$IPT_LOG"
    cat > "$IPT_RULESET" <<'RULES'
-P INPUT ACCEPT
-P FORWARD ACCEPT
-N LS-NSI-10-0-0-5
-N OLDP-NSI-10-0-0-5
-N OLDP-SG-abcd1234
-N SOMEONE-ELSE
-A FORWARD -d 10.0.0.5/32 -i eth0 -j OLDP-NSI-10-0-0-5
-A FORWARD -d 10.0.0.5/32 -i eth0 -j LS-NSI-10-0-0-5
-A FORWARD -j SOMEONE-ELSE
-A OLDP-NSI-10-0-0-5 -j DROP
RULES
    mkdir -p "$FIX/bin"
    # Stateful: -D/-F/-X actually edit the fixture, so a final re-scan can distinguish a
    # cleanup that worked from one that did not. IPT_FAIL_OPS injects failures.
    cat > "$FIX/bin/iptables" <<'STUB'
#!/bin/sh
echo "$*" >> "$IPT_LOG"

op=""; chain=""
for a in "$@"; do
    case "$a" in
        -S|-D|-F|-X) [ -z "$op" ] && op="$a"; continue ;;
    esac
    if [ -n "$op" ] && [ -z "$chain" ] && [ "$op" != "-S" ]; then chain="$a"; fi
done

case " $IPT_FAIL_OPS " in
    *" $op "*) echo "iptables: fake failure for $op" >&2; exit 1 ;;
esac

case "$op" in
    -S) cat "$IPT_RULESET"; exit 0 ;;
    -X) grep -v "^-N $chain\$" "$IPT_RULESET" > "$IPT_RULESET.t" && mv "$IPT_RULESET.t" "$IPT_RULESET" ;;
    -F) grep -v "^-A $chain " "$IPT_RULESET" > "$IPT_RULESET.t" && mv "$IPT_RULESET.t" "$IPT_RULESET" ;;
    -D) spec="$(echo "$*" | sed 's/^-t [a-z]* -D //')"
        grep -v -- "^-A $spec\$" "$IPT_RULESET" > "$IPT_RULESET.t" && mv "$IPT_RULESET.t" "$IPT_RULESET" ;;
esac
exit 0
STUB
    chmod +x "$FIX/bin/iptables"
}

# Run the migration with the fake iptables wired in. LEGACY_NAME is overridden to match
# the fixture's chain prefixes.
run_migrate_ipt() {
    LIGHTSTACK_IPTABLES="$FIX/bin/iptables" \
    LIGHTSTACK_LEGACY_NAME=oldp \
    IPT_LOG="$IPT_LOG" IPT_RULESET="$IPT_RULESET" IPT_FAIL_OPS="${IPT_FAIL_OPS:-}" \
        run_migrate "$@"
}

# Mutating iptables calls recorded by the fake (everything except the -S reads).
ipt_mutations() {
    grep -vE '(^| )-S( |$)' "$IPT_LOG" 2>/dev/null || true
}

# Run the migration against the current fixture. Extra args are passed through.
run_migrate() {
    LIGHTSTACK_STATE_ROOT="$NEW" \
    LIGHTSTACK_LEGACY_STATE_ROOT="$LEG" \
    LIGHTSTACK_RUN_ROOT="$NEW_RUN" \
    LIGHTSTACK_LEGACY_RUN_ROOT="$LEG_RUN" \
    LIGHTSTACK_LEGACY_INSTALL_ROOT="$FIX/opt/legacy" \
    PATH="$FIX/bin:$PATH" \
        "$MIGRATE" "$@"
}

drop_fixture() { rm -rf "$FIX"; }

# --- the migration must not touch anything until it is told to -----------------
test_dry_run_changes_nothing() {
    log_test "dry run reports the plan and moves nothing"
    make_fixture
    if ! out="$(run_migrate 2>&1)"; then
        log_fail "dry run exited non-zero: $out"; drop_fixture; return
    fi
    if [ -f "$NEW/images/image-abc.raw" ]; then
        log_fail "dry run moved data"; drop_fixture; return
    fi
    if [ ! -f "$LEG/images/image-abc.raw" ]; then
        log_fail "dry run removed data from the legacy root"; drop_fixture; return
    fi
    if ! echo "$out" | grep -qi "dry run"; then
        log_fail "dry run did not say so: $out"; drop_fixture; return
    fi
    log_pass "dry run is inert"
    drop_fixture
}

# --- --apply moves every node-local artifact the product derives a path for ----
test_apply_moves_state() {
    log_test "--apply moves all five state subdirs and the runtime dir"
    make_fixture
    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    local missing=""
    for f in images/image-abc.raw volumes/volume-xyz.qcow2 \
             instances/iid-1/disk.qcow2 instances/iid-1/console.log \
             cloud-init/iid-1.iso dhcp/net-1.leases; do
        [ -f "$NEW/$f" ] || missing="$missing $f"
    done
    [ -f "$NEW_RUN/metadata.sock" ] || missing="$missing (run)metadata.sock"
    if [ -n "$missing" ]; then
        log_fail "not migrated:$missing"; drop_fixture; return
    fi
    log_pass "state and runtime dirs migrated"
    drop_fixture
}

# --- an operator who already started fresh must not lose the newer file --------
test_apply_does_not_clobber() {
    log_test "--apply keeps the destination file and fails loudly on the conflict"
    make_fixture
    mkdir -p "$NEW/images"
    echo KEEP-ME > "$NEW/images/image-abc.raw"
    # A path present on BOTH sides cannot be resolved by the script, so it must not be
    # reported as a clean migration — automation would carry on with data left behind.
    if out="$(run_migrate --apply 2>&1)"; then
        log_fail "exited 0 despite an unresolved conflict: $out"; drop_fixture; return
    fi
    if [ "$(cat "$NEW/images/image-abc.raw")" != "KEEP-ME" ]; then
        log_fail "clobbered the file already under the new root"; drop_fixture; return
    fi
    if [ ! -f "$LEG/images/image-abc.raw" ]; then
        log_fail "discarded the legacy copy instead of leaving it behind"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "images/image-abc.raw"; then
        log_fail "did not name the conflicting path: $out"; drop_fixture; return
    fi
    # Everything else must still have moved — one conflict may not block the rest.
    if [ ! -f "$NEW/volumes/volume-xyz.qcow2" ]; then
        log_fail "one conflict stopped the other subdirs from migrating"; drop_fixture; return
    fi
    log_pass "destination kept, conflict named, non-zero exit, rest still migrated"
    drop_fixture
}

# --- the defect the review found: a partially-copied directory must MERGE, not be skipped
test_merges_partially_migrated_directory() {
    log_test "--apply merges into a partially migrated directory instead of skipping it"
    make_fixture
    # Exactly what an interrupted cross-mount copy leaves: the instance dir exists at the
    # destination holding only some of its children.
    mkdir -p "$NEW/instances/iid-1"
    cp "$LEG/instances/iid-1/console.log" "$NEW/instances/iid-1/console.log"
    rm "$LEG/instances/iid-1/console.log"

    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "--apply exited non-zero with a resolvable merge: $out"; drop_fixture; return
    fi
    if [ ! -f "$NEW/instances/iid-1/disk.qcow2" ]; then
        log_fail "the missing child was left behind — the directory was skipped, not merged"; drop_fixture; return
    fi
    if [ ! -f "$NEW/instances/iid-1/console.log" ]; then
        log_fail "the already-present child was lost"; drop_fixture; return
    fi
    log_pass "partially migrated directory merged"
    drop_fixture
}

# --- nested depth must merge too, not just one level ---
test_merges_nested_directories() {
    log_test "--apply merges directories nested more than one level deep"
    make_fixture
    mkdir -p "$LEG/instances/iid-2/sub/deeper" "$NEW/instances/iid-2/sub"
    echo deep > "$LEG/instances/iid-2/sub/deeper/file"
    echo kept > "$NEW/instances/iid-2/sub/kept"

    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    if [ ! -f "$NEW/instances/iid-2/sub/deeper/file" ]; then
        log_fail "nested file was not merged"; drop_fixture; return
    fi
    if [ ! -f "$NEW/instances/iid-2/sub/kept" ]; then
        log_fail "existing nested file was lost"; drop_fixture; return
    fi
    log_pass "nested merge works"
    drop_fixture
}

# --- an interrupted upgrade gets re-run; that must be safe ---------------------
test_apply_is_idempotent() {
    log_test "--apply twice is a clean no-op the second time"
    make_fixture
    run_migrate --apply >/dev/null 2>&1
    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "second --apply exited non-zero: $out"; drop_fixture; return
    fi
    if [ ! -f "$NEW/images/image-abc.raw" ]; then
        log_fail "second run lost migrated data"; drop_fixture; return
    fi
    log_pass "idempotent"
    drop_fixture
}

# --- a fresh install has no legacy root; the script must be a silent success ----
test_no_legacy_root() {
    log_test "no legacy root at all exits 0 without creating anything"
    make_fixture
    rm -rf "$LEG" "$LEG_RUN"
    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "exited non-zero with nothing to do: $out"; drop_fixture; return
    fi
    if [ -d "$NEW" ]; then
        log_fail "created the new root with nothing to migrate"; drop_fixture; return
    fi
    log_pass "nothing to migrate is not an error"
    drop_fixture
}

# --- the install layout is the operator's call, not the script's ---------------
test_install_layout_reported_not_moved() {
    log_test "a legacy install root is reported, never moved"
    make_fixture
    mkdir -p "$FIX/opt/legacy/bin"
    echo binary > "$FIX/opt/legacy/bin/legacy"
    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    if [ ! -f "$FIX/opt/legacy/bin/legacy" ]; then
        log_fail "moved the install root"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "$FIX/opt/legacy"; then
        log_fail "did not report the legacy install root: $out"; drop_fixture; return
    fi
    log_pass "install root reported, untouched"
    drop_fixture
}

# --- moving state out from under a running service would corrupt it ------------
test_refuses_while_service_active() {
    log_test "refuses to migrate while a Lightstack service is still active"
    make_fixture
    mkdir -p "$FIX/bin"
    cat > "$FIX/bin/systemctl" <<'STUB'
#!/bin/sh
# Stub: every unit looks active.
[ "$1" = "is-active" ] && exit 0
exit 0
STUB
    chmod +x "$FIX/bin/systemctl"
    if out="$(run_migrate --apply 2>&1)"; then
        log_fail "migrated while the service was active"; drop_fixture; return
    fi
    if [ -f "$NEW/images/image-abc.raw" ]; then
        log_fail "moved data despite refusing"; drop_fixture; return
    fi
    log_pass "refused with the service running"
    drop_fixture
}

# --- the legacy chains are host firewall state; a dry run must not touch them ----
test_iptables_dry_run_reports_only() {
    log_test "dry run names the legacy chains without changing any"
    make_fixture; make_fake_iptables
    if ! out="$(run_migrate_ipt 2>&1)"; then
        log_fail "dry run exited non-zero: $out"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "OLDP-NSI-10-0-0-5"; then
        log_fail "dry run did not report the legacy chain: $out"; drop_fixture; return
    fi
    if [ -n "$(ipt_mutations)" ]; then
        log_fail "dry run issued mutating iptables calls:"$'\n'"$(ipt_mutations)"; drop_fixture; return
    fi
    log_pass "dry run is inert"
    drop_fixture
}

# --- order is load-bearing: a chain with a jump still pointing at it cannot be
# --- deleted ("Resource busy"), so the jump has to go first ----------------------
test_iptables_deletes_jump_before_chain() {
    log_test "--apply removes the jump before deleting the chain"
    make_fixture; make_fake_iptables
    if ! out="$(run_migrate_ipt --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    local mutations del_line x_line
    mutations="$(ipt_mutations)"
    del_line="$(echo "$mutations" | grep -n -- '-D FORWARD .* -j OLDP-NSI-10-0-0-5' | head -1 | cut -d: -f1)"
    x_line="$(echo "$mutations" | grep -n -- '-X OLDP-NSI-10-0-0-5' | head -1 | cut -d: -f1)"
    if [ -z "$del_line" ]; then
        log_fail "the jump to the legacy chain was never deleted:"$'\n'"$mutations"; drop_fixture; return
    fi
    if [ -z "$x_line" ]; then
        log_fail "the legacy chain was never deleted:"$'\n'"$mutations"; drop_fixture; return
    fi
    if [ "$del_line" -ge "$x_line" ]; then
        log_fail "deleted the chain before its jump (would fail with Resource busy):"$'\n'"$mutations"; drop_fixture; return
    fi
    if ! echo "$mutations" | grep -q -- '-X OLDP-SG-abcd1234'; then
        log_fail "the unreferenced legacy chain was left behind:"$'\n'"$mutations"; drop_fixture; return
    fi
    log_pass "jump removed first, then both legacy chains"
    drop_fixture
}

# --- the current chains and anything else on the host must be left alone ---------
test_iptables_leaves_other_chains_alone() {
    log_test "--apply touches neither the current chains nor foreign ones"
    make_fixture; make_fake_iptables
    if ! out="$(run_migrate_ipt --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    local mutations
    mutations="$(ipt_mutations)"
    if echo "$mutations" | grep -q 'LS-NSI-10-0-0-5'; then
        log_fail "touched the current chain:"$'\n'"$mutations"; drop_fixture; return
    fi
    if echo "$mutations" | grep -q 'SOMEONE-ELSE'; then
        log_fail "touched a chain that is not ours:"$'\n'"$mutations"; drop_fixture; return
    fi
    log_pass "only the legacy chains were touched"
    drop_fixture
}

# --- nothing to clean up is not an error ----------------------------------------
test_iptables_no_legacy_chains() {
    log_test "a host with no legacy chains sees no iptables writes"
    make_fixture; make_fake_iptables
    grep -v OLDP- "$IPT_RULESET" > "$IPT_RULESET.tmp" && mv "$IPT_RULESET.tmp" "$IPT_RULESET"
    if ! out="$(run_migrate_ipt --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    if [ -n "$(ipt_mutations)" ]; then
        log_fail "wrote to iptables with nothing to clean:"$'\n'"$(ipt_mutations)"; drop_fixture; return
    fi
    log_pass "no legacy chains, no writes"
    drop_fixture
}

# --- the guard that keeps a non-root test run away from the host's real firewall --
test_iptables_skipped_without_override_or_root() {
    log_test "skips the firewall step when not root and not overridden"
    make_fixture
    if ! out="$(run_migrate --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    if ! echo "$out" | grep -qi "iptables"; then
        log_fail "said nothing about skipping the firewall step: $out"; drop_fixture; return
    fi
    log_pass "firewall step skipped, host untouched"
    drop_fixture
}

# --- the move is a rename, so it keeps the previous account's UID/GID; the new unit runs
# --- as a different account and would not be able to write its own state ---------------
test_reowns_migrated_state() {
    log_test "--apply re-owns the migrated state to the service account"
    make_fixture
    # Give the legacy file a group that is NOT the service account's, so a passing
    # assertion means the chown actually ran rather than the fixture already matching.
    local other_group
    other_group="$(id -Gn | tr ' ' '\n' | grep -vx "$(id -gn)" | head -1)"
    if [ -z "$other_group" ]; then
        log_fail "need a secondary group to make this test meaningful"; drop_fixture; return
    fi
    chgrp -R "$other_group" "$LEG" "$LEG_RUN"
    if [ "$(stat -c '%G' "$LEG/images/image-abc.raw")" != "$other_group" ] ||
       [ "$(stat -c '%G' "$LEG_RUN/metadata.sock")" != "$other_group" ]; then
        log_fail "could not set up the differing group on both legacy roots"; drop_fixture; return
    fi

    if ! out="$(LIGHTSTACK_SERVICE_USER="$(id -un)" run_migrate --apply 2>&1)"; then
        log_fail "--apply exited non-zero: $out"; drop_fixture; return
    fi
    if [ ! -f "$NEW/images/image-abc.raw" ]; then
        log_fail "state was not migrated at all"; drop_fixture; return
    fi
    local ownership
    ownership="$(stat -c '%U:%G' "$NEW/images/image-abc.raw")"
    if [ "$ownership" != "$(id -un):$(id -gn)" ]; then
        log_fail "migrated file is $ownership, expected $(id -un):$(id -gn) — chown did not run"; drop_fixture; return
    fi
    # The runtime root is migrated as well, so it has to be re-owned as well.
    ownership="$(stat -c '%U:%G' "$NEW_RUN/metadata.sock")"
    if [ "$ownership" != "$(id -un):$(id -gn)" ]; then
        log_fail "migrated runtime state is $ownership — the run root was not re-owned"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "Re-owning migrated state"; then
        log_fail "did not report the re-own: $out"; drop_fixture; return
    fi
    log_pass "migrated state re-owned to the service account"
    drop_fixture
}

# --- nothing in the repo creates that account, so its absence must not fail the run -----
test_reports_when_service_account_missing() {
    log_test "a missing service account is reported with the command, not an error"
    make_fixture
    local other_group
    other_group="$(id -Gn | tr ' ' '\n' | grep -vx "$(id -gn)" | head -1)"
    if [ -z "$other_group" ]; then
        log_fail "need a secondary group to make this test meaningful"; drop_fixture; return
    fi
    chgrp -R "$other_group" "$LEG" "$LEG_RUN"
    if ! out="$(LIGHTSTACK_SERVICE_USER=lightstack-no-such-account run_migrate --apply 2>&1)"; then
        log_fail "--apply failed because the account is absent: $out"; drop_fixture; return
    fi
    if [ ! -f "$NEW/images/image-abc.raw" ]; then
        log_fail "the missing account stopped the state move"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "lightstack-no-such-account"; then
        log_fail "did not name the missing account: $out"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "chown"; then
        log_fail "did not print the chown command to run by hand: $out"; drop_fixture; return
    fi
    # Specifically the guard's message. Without the existence check the script instead
    # attempts the chown and reports a generic failure, which is a worse diagnostic for
    # the same end state.
    if ! echo "$out" | grep -q "does not exist"; then
        log_fail "did not identify the missing account as the reason: $out"; drop_fixture; return
    fi
    # The distinguishing check: with no such account, ownership must be untouched. Without
    # it this test passes whether or not the guard exists, because the warning text itself
    # contains both the account name and the word chown.
    if [ "$(stat -c '%G' "$NEW/images/image-abc.raw")" != "$other_group" ]; then
        log_fail "ownership was changed despite the account not existing"; drop_fixture; return
    fi
    log_pass "missing account reported, migration still succeeded, ownership untouched"
    drop_fixture
}

# --- a dry run must not touch ownership any more than it touches the files --------------
test_dry_run_does_not_reown() {
    log_test "dry run does not change ownership"
    make_fixture
    local other_group
    other_group="$(id -Gn | tr ' ' '\n' | grep -vx "$(id -gn)" | head -1)"
    if [ -z "$other_group" ]; then
        log_fail "need a secondary group to make this test meaningful"; drop_fixture; return
    fi
    # Watch the destination root, which is what the re-own step targets — watching the
    # legacy root would pass no matter what, since the chown never points there.
    mkdir -p "$NEW/images"
    echo pre-existing > "$NEW/images/keep.raw"
    chgrp -R "$other_group" "$NEW"

    if ! out="$(LIGHTSTACK_SERVICE_USER="$(id -un)" run_migrate 2>&1)"; then
        log_fail "dry run exited non-zero: $out"; drop_fixture; return
    fi
    if [ "$(stat -c '%G' "$NEW/images/keep.raw")" != "$other_group" ]; then
        log_fail "dry run re-owned the destination root"; drop_fixture; return
    fi
    if echo "$out" | grep -q "Re-owning migrated state"; then
        log_fail "dry run reported a re-own it must not perform: $out"; drop_fixture; return
    fi
    log_pass "dry run left ownership alone"
    drop_fixture
}

# --- a cleanup that cannot finish must not be reported as a finished migration --------
test_iptables_failure_fails_the_run() {
    log_test "a failed jump deletion fails the run and names the rule"
    make_fixture; make_fake_iptables
    if out="$(IPT_FAIL_OPS="-D" run_migrate_ipt --apply 2>&1)"; then
        log_fail "exited 0 despite being unable to remove a legacy jump: $out"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "OLDP-NSI-10-0-0-5"; then
        log_fail "did not name the surviving rule: $out"; drop_fixture; return
    fi
    log_pass "failed jump deletion is a non-zero exit, rule named"
    drop_fixture
}

# --- the flush failure is the one that actually lets stale rules keep deciding ---------
test_iptables_flush_failure_is_not_ignored() {
    log_test "a failed chain flush fails the run"
    make_fixture; make_fake_iptables
    if out="$(IPT_FAIL_OPS="-F" run_migrate_ipt --apply 2>&1)"; then
        log_fail "exited 0 despite being unable to flush a legacy chain: $out"; drop_fixture; return
    fi
    if ! echo "$out" | grep -qi "flush\|OLDP-"; then
        log_fail "said nothing about the chain it could not flush: $out"; drop_fixture; return
    fi
    log_pass "flush failure is no longer swallowed"
    drop_fixture
}

# --- a command can return 0 and still leave a rule; only a re-scan proves it gone ------
test_iptables_rescan_catches_survivors() {
    log_test "a legacy chain that survives a silent cleanup is caught by the re-scan"
    make_fixture; make_fake_iptables
    # -X reports success but changes nothing: exactly the case a return code cannot see.
    sed -i 's|^    -X) grep -v|    -X) true \&\& grep -v|' "$FIX/bin/iptables"
    sed -i 's|^    -X) true && grep -v "\^-N \$chain\\$" "\$IPT_RULESET".*|    -X) : ;;|' "$FIX/bin/iptables"
    if out="$(run_migrate_ipt --apply 2>&1)"; then
        log_fail "exited 0 while a legacy chain survived: $out"; drop_fixture; return
    fi
    if ! echo "$out" | grep -q "OLDP-"; then
        log_fail "did not name the surviving chain: $out"; drop_fixture; return
    fi
    log_pass "re-scan caught the survivor"
    drop_fixture
}

# --- and a clean cleanup must still succeed -------------------------------------------
test_iptables_clean_cleanup_succeeds() {
    log_test "a cleanup that removes everything exits 0"
    make_fixture; make_fake_iptables
    if ! out="$(run_migrate_ipt --apply 2>&1)"; then
        log_fail "clean cleanup exited non-zero: $out"; drop_fixture; return
    fi
    if grep -q "^-N OLDP-" "$IPT_RULESET"; then
        log_fail "legacy chains still in the ruleset after a clean run"; drop_fixture; return
    fi
    log_pass "clean cleanup succeeds"
    drop_fixture
}

main() {
    echo ""
    echo "Testing $(basename "$MIGRATE")"
    echo ""
    if [ ! -x "$MIGRATE" ]; then
        echo -e "${RED}[FAIL]${NC} $MIGRATE is missing or not executable"
        exit 1
    fi

    test_dry_run_changes_nothing
    test_apply_moves_state
    test_apply_does_not_clobber
    test_merges_partially_migrated_directory
    test_merges_nested_directories
    test_apply_is_idempotent
    test_no_legacy_root
    test_install_layout_reported_not_moved
    test_refuses_while_service_active
    test_iptables_dry_run_reports_only
    test_iptables_deletes_jump_before_chain
    test_iptables_leaves_other_chains_alone
    test_iptables_no_legacy_chains
    test_iptables_skipped_without_override_or_root
    test_iptables_clean_cleanup_succeeds
    test_iptables_failure_fails_the_run
    test_iptables_flush_failure_is_not_ignored
    test_iptables_rescan_catches_survivors
    test_reowns_migrated_state
    test_reports_when_service_account_missing
    test_dry_run_does_not_reown

    echo ""
    echo -e "${GREEN}Tests Passed: $TESTS_PASSED${NC}"
    echo -e "${RED}Tests Failed: $TESTS_FAILED${NC}"
    echo ""
    [ "$TESTS_FAILED" -eq 0 ]
}

main "$@"
