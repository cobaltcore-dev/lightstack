#!/bin/bash
#
# Endpoint-hostname forward migration (cobaltcore-dev/cloud-storage#575)
#
# Migration 052 seeded endpoints.url with the product's Docker hostname. The rename
# edited 052 in place, which reaches fresh databases only: golang-migrate is
# version-tracked, so an install already past 52 keeps its pre-rename rows, and
# substituteURLTemplates (internal/keystone/auth.go) matches only the new token — those
# rows are then served verbatim and the catalog advertises a host that need not resolve.
# The forward migration fixes the existing rows. This test proves both directions.
#
# Needs a PostgreSQL it may create and drop a scratch database in. The database named in
# the URL is never touched.
#
#   LIGHTSTACK_TEST_DB_URL='postgres://user:pw@127.0.0.1:5432/postgres' \
#     ./test/endpoint_hostname_migration_test.sh
#
# sslmode=disable is appended when the URL does not set sslmode — the driver otherwise
# requires TLS, which a local scratch database does not have.
#

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

# The pre-rename catalog host token. Stated here rather than read out of the migration:
# a test that derives its expectation from the code under test cannot fail.
LEGACY_HOST="${LIGHTSTACK_LEGACY_HOST:-o3k}"
CURRENT_HOST="lightstack"

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
# Plain assignment, not ((x++)): the latter returns 1 while the counter is still 0,
# which under `set -e` would kill the run on the first pass.
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; TESTS_PASSED=$((TESTS_PASSED + 1)); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; TESTS_FAILED=$((TESTS_FAILED + 1)); }

if [ -z "${LIGHTSTACK_TEST_DB_URL:-}" ]; then
    echo -e "${RED}[FAIL]${NC} LIGHTSTACK_TEST_DB_URL is not set — refusing to guess a database."
    echo "  e.g. LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@127.0.0.1:5432/postgres'"
    exit 1
fi
for tool in psql go; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo -e "${RED}[FAIL]${NC} $tool is required"
        exit 1
    fi
done

# A local scratch database has no TLS; the driver demands it unless told otherwise.
case "$LIGHTSTACK_TEST_DB_URL" in
    *sslmode=*) ;;
    *\?*) LIGHTSTACK_TEST_DB_URL="${LIGHTSTACK_TEST_DB_URL}&sslmode=disable" ;;
    *)      LIGHTSTACK_TEST_DB_URL="${LIGHTSTACK_TEST_DB_URL}?sslmode=disable" ;;
esac

# Swap the database name in a postgres:// URL, preserving any query string.
db_url_with() {
    local url="$1" newdb="$2" base query=""
    base="${url%%\?*}"
    if [ "$base" != "$url" ]; then
        query="?${url#*\?}"
    fi
    echo "${base%/*}/${newdb}${query}"
}

SCRATCH_DB="lightstack_mig_test_$$"
ADMIN_URL="$(db_url_with "$LIGHTSTACK_TEST_DB_URL" postgres)"
SCRATCH_URL="$(db_url_with "$LIGHTSTACK_TEST_DB_URL" "$SCRATCH_DB")"

cleanup() {
    psql "$ADMIN_URL" -q -c "DROP DATABASE IF EXISTS \"$SCRATCH_DB\" WITH (FORCE);" >/dev/null 2>&1
    rm -f "${MIGRATE_BIN:-}"
}
trap cleanup EXIT

q() { psql "$SCRATCH_URL" -At -c "$1" 2>/dev/null; }

# Built once: the suite calls the migrator a dozen times, and `go run` would relink
# the binary on every one of them.
MIGRATE_BIN="$(mktemp -u "/tmp/lightstack-migrate-test-$$-XXXX")"
if ! go build -o "$MIGRATE_BIN" ./cmd/lightstack-migrate; then
    echo -e "${RED}[FAIL]${NC} could not build ./cmd/lightstack-migrate"
    exit 1
fi

migrate() { LIGHTSTACK_DB_URL="$SCRATCH_URL" "$MIGRATE_BIN" "$@" >/dev/null 2>&1; }

# force/down/goto/reset prompt for confirmation, and a cancelled prompt still exits 0 —
# so feed the answer, then verify the version actually moved rather than trusting $?.
migrate_confirm() { echo yes | LIGHTSTACK_DB_URL="$SCRATCH_URL" "$MIGRATE_BIN" "$@" >/dev/null 2>&1; }

schema_version() {
    LIGHTSTACK_DB_URL="$SCRATCH_URL" "$MIGRATE_BIN" status 2>/dev/null |
        sed -n 's/.*Current Version: *\([0-9][0-9]*\).*/\1/p' | head -1
}

# Rewind the recorded version so the forward migration is the only thing left to apply.
rewind_to() {
    local want="$1" got
    migrate_confirm force "$want"
    got="$(schema_version)"
    [ "$got" = "$want" ]
}

# Rows still pointing at the pre-rename host. Must be 0 after any upgrade path.
count_legacy_rows() { q "SELECT count(*) FROM endpoints WHERE url LIKE '%//${LEGACY_HOST}:%';"; }
count_current_rows() { q "SELECT count(*) FROM endpoints WHERE url LIKE '%//${CURRENT_HOST}:%';"; }

setup_scratch_db() {
    psql "$ADMIN_URL" -q -c "DROP DATABASE IF EXISTS \"$SCRATCH_DB\" WITH (FORCE);" >/dev/null 2>&1
    if ! psql "$ADMIN_URL" -q -c "CREATE DATABASE \"$SCRATCH_DB\";" >/dev/null 2>&1; then
        echo -e "${RED}[FAIL]${NC} could not create scratch database $SCRATCH_DB"
        exit 1
    fi
}

# --- a fresh install must end up on the current host, with no old rows ---------
test_fresh_install() {
    log_test "fresh install: migrating to head leaves no pre-rename endpoint rows"
    setup_scratch_db
    if ! migrate up; then
        log_fail "migrate up failed on a fresh database"; return
    fi
    local legacy current
    legacy="$(count_legacy_rows)"
    current="$(count_current_rows)"
    if [ "$legacy" != "0" ]; then
        log_fail "fresh install has $legacy row(s) on the pre-rename host"; return
    fi
    if [ "${current:-0}" -lt 5 ]; then
        log_fail "expected at least 5 rows on $CURRENT_HOST, got ${current:-0}"; return
    fi
    log_pass "fresh install clean ($current rows on $CURRENT_HOST)"
}

# --- the case the reviewer raised: a database that already ran the old 052 ------
test_upgrade_rewrites_existing_rows() {
    log_test "upgrade: an existing database's pre-rename rows are rewritten"
    setup_scratch_db
    if ! migrate up; then
        log_fail "migrate up failed on a fresh database"; return
    fi
    # Make it look like an install that ran 052 before the rename, and rewind the
    # recorded version so the forward migration is the only thing left to apply.
    q "UPDATE endpoints SET url = REPLACE(url, '//${CURRENT_HOST}:', '//${LEGACY_HOST}:');" >/dev/null
    local before
    before="$(count_legacy_rows)"
    if [ "${before:-0}" -lt 5 ]; then
        log_fail "fixture did not produce pre-rename rows (got ${before:-0})"; return
    fi
    if ! rewind_to 62; then
        log_fail "could not rewind the schema version to 62 (now $(schema_version))"; return
    fi
    if ! migrate up; then
        log_fail "migrate up failed on the simulated existing database"; return
    fi
    local after current
    after="$(count_legacy_rows)"
    current="$(count_current_rows)"
    if [ "$after" != "0" ]; then
        log_fail "$after row(s) still on the pre-rename host after upgrading"; return
    fi
    if [ "${current:-0}" -lt "$before" ]; then
        log_fail "rewrote $before row(s) into only ${current:-0} on $CURRENT_HOST"; return
    fi
    log_pass "rewrote $before pre-rename row(s)"
}

# --- an endpoint an operator created by hand must be fixed too ------------------
test_rewrites_non_seeded_rows() {
    log_test "upgrade: a hand-created endpoint row on the old host is rewritten too"
    setup_scratch_db
    if ! migrate up; then
        log_fail "migrate up failed on a fresh database"; return
    fi
    q "INSERT INTO services (id, type, name, description, enabled)
       VALUES ('00000000-0000-0000-0000-0000000009f1', 'custom', 'hand-made', 'operator created', true)
       ON CONFLICT DO NOTHING;" >/dev/null
    q "INSERT INTO endpoints (service_id, interface, url, region)
       VALUES ('00000000-0000-0000-0000-0000000009f1', 'public', 'http://${LEGACY_HOST}:9999/v1', 'RegionOne');" >/dev/null
    if [ "$(count_legacy_rows)" = "0" ]; then
        log_fail "fixture row was not inserted"; return
    fi
    if ! rewind_to 62; then
        log_fail "could not rewind the schema version to 62 (now $(schema_version))"; return
    fi
    if ! migrate up; then
        log_fail "re-running the migration failed"; return
    fi
    if [ "$(count_legacy_rows)" != "0" ]; then
        log_fail "hand-created row left on the pre-rename host (migration is scoped too narrowly)"; return
    fi
    local url
    url="$(q "SELECT url FROM endpoints WHERE service_id = '00000000-0000-0000-0000-0000000009f1';")"
    if [ "$url" != "http://${CURRENT_HOST}:9999/v1" ]; then
        log_fail "hand-created row became '$url', expected http://${CURRENT_HOST}:9999/v1"; return
    fi
    log_pass "hand-created row rewritten, port and path preserved"
}

# --- re-running must not corrupt what it already fixed -------------------------
test_idempotent() {
    log_test "upgrade: applying the migration twice changes nothing the second time"
    setup_scratch_db
    if ! migrate up; then
        log_fail "migrate up failed on a fresh database"; return
    fi
    local first
    first="$(q "SELECT string_agg(url, ',' ORDER BY url) FROM endpoints;")"
    if ! rewind_to 62; then
        log_fail "could not rewind the schema version to 62 (now $(schema_version))"; return
    fi
    if ! migrate up; then
        log_fail "re-applying the migration failed"; return
    fi
    local second
    second="$(q "SELECT string_agg(url, ',' ORDER BY url) FROM endpoints;")"
    if [ "$first" != "$second" ]; then
        log_fail "re-applying changed the rows:"$'\n'"  before: $first"$'\n'"  after:  $second"; return
    fi
    log_pass "idempotent"
}

# --- down must undo it, so the migration is reversible ------------------------
test_down_restores_legacy_host() {
    log_test "down: rolling the migration back restores the pre-rename host"
    setup_scratch_db
    if ! migrate up; then
        log_fail "migrate up failed on a fresh database"; return
    fi
    migrate_confirm down
    local version
    version="$(schema_version)"
    if [ "$version" != "62" ]; then
        log_fail "down did not roll 069 back (version is now $version)"; return
    fi
    local legacy
    legacy="$(count_legacy_rows)"
    if [ "${legacy:-0}" -lt 5 ]; then
        log_fail "down left only ${legacy:-0} row(s) on the pre-rename host"; return
    fi
    log_pass "down restored $legacy row(s)"
}

main() {
    echo ""
    echo "Endpoint-hostname forward migration"
    echo "  scratch database: $SCRATCH_DB"
    echo ""

    test_fresh_install
    test_upgrade_rewrites_existing_rows
    test_rewrites_non_seeded_rows
    test_idempotent
    test_down_restores_legacy_host

    echo ""
    echo -e "${GREEN}Tests Passed: $TESTS_PASSED${NC}"
    echo -e "${RED}Tests Failed: $TESTS_FAILED${NC}"
    echo ""
    [ "$TESTS_FAILED" -eq 0 ]
}

main "$@"
