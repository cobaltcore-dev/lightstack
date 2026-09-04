#!/bin/bash
#
# Compose database identity must be overridable (cobaltcore-dev/cloud-storage#575)
#
# PostgreSQL honours POSTGRES_DB / POSTGRES_USER only while initialising an empty data
# directory. A Compose stack that keeps its postgres volume across an upgrade therefore
# keeps the database and role it was first created with — so if the stack hard-codes a new
# identity, the application cannot authenticate and the stack will not come up. Worse, the
# healthcheck is pg_isready, which does not authenticate and still reports healthy.
#
# The identity is read from POSTGRES_DB / POSTGRES_USER / POSTGRES_PASSWORD (the variables
# .env.example already documents), defaulting to the current names. This test renders each
# stack with `compose config` and checks the defaults, then checks that an override reaches
# every place the identity appears — including the application's DSN, which is the part a
# half-parameterised file would get wrong.
#
# Usage: ./test/compose_db_identity_test.sh
#

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0

log_test() { echo -e "${BLUE}[TEST]${NC} $1"; }
# Plain assignment, not ((x++)): that returns 1 while the counter is still 0.
log_pass() { echo -e "${GREEN}[PASS]${NC} $1"; TESTS_PASSED=$((TESTS_PASSED + 1)); }
log_fail() { echo -e "${RED}[FAIL]${NC} $1"; TESTS_FAILED=$((TESTS_FAILED + 1)); }

if ! docker compose version >/dev/null 2>&1; then
    echo -e "${RED}[FAIL]${NC} 'docker compose' is required to render the stacks"
    exit 1
fi
if ! python3 -c "import yaml" >/dev/null 2>&1; then
    echo -e "${RED}[FAIL]${NC} python3 with PyYAML is required to read the rendered stacks"
    exit 1
fi

# Stacks that keep a postgres volume, so their identity survives an upgrade.
# The rgw-test stack and the CI service container start from an empty data directory every
# time and are deliberately not listed.
STACKS="
deployments/docker-compose.yml
deployments/docker/docker-compose.yaml
deployments/docker-compose-horizon.yml
deployments/horizon/docker-compose.yaml
"

# Report the effective identity of a rendered stack as shell-ish lines. Absent values are
# reported as '-' so a missing key is visible rather than empty.
read_identity() {
    python3 -c '
import sys, yaml

doc = yaml.safe_load(sys.stdin) or {}
services = doc.get("services") or {}

def env_of(service):
    raw = (services.get(service) or {}).get("environment") or {}
    if isinstance(raw, list):                      # "- KEY=value" form
        out = {}
        for item in raw:
            key, _, value = str(item).partition("=")
            out[key] = value
        return out
    return {k: ("" if v is None else str(v)) for k, v in raw.items()}

pg = env_of("postgres")
print("DB=%s" % pg.get("POSTGRES_DB", "-"))
print("USER=%s" % pg.get("POSTGRES_USER", "-"))
print("PASSWORD=%s" % pg.get("POSTGRES_PASSWORD", "-"))

health = ((services.get("postgres") or {}).get("healthcheck") or {}).get("test") or []
if isinstance(health, list):
    health = " ".join(str(h) for h in health)
print("HEALTHCHECK=%s" % health)

# The application DSN, wherever the app service is defined in this stack.
dsn = "-"
for name in ("lightstack", "app"):
    value = env_of(name).get("LIGHTSTACK_DB_URL")
    if value:
        dsn = value
        break
print("DSN=%s" % dsn)
'
}

render() {
    # docker-compose-horizon.yml declares LIGHTSTACK_JWT_SECRET as mandatory; supply a value
    # so rendering does not fail for an unrelated reason.
    LIGHTSTACK_JWT_SECRET="${LIGHTSTACK_JWT_SECRET:-render-only}" \
        timeout 120 docker compose -f "$1" config 2>/dev/null
}

field() { echo "$1" | sed -n "s/^$2=//p"; }

# --- a stack with nothing set must behave exactly as it does today -------------
test_defaults() {
    local stack want="lightstack"
    for stack in $STACKS; do
        log_test "defaults unchanged: $stack"
        local out identity db user health dsn
        out="$(render "$stack")"
        if [ -z "$out" ]; then
            log_fail "$stack did not render"; continue
        fi
        identity="$(echo "$out" | read_identity)"
        db="$(field "$identity" DB)"
        user="$(field "$identity" USER)"
        health="$(field "$identity" HEALTHCHECK)"
        dsn="$(field "$identity" DSN)"

        if [ "$db" != "$want" ] || [ "$user" != "$want" ]; then
            log_fail "$stack default identity is db=$db user=$user, expected $want/$want"; continue
        fi
        case "$health" in
            *"-U $want"*) ;;
            *) log_fail "$stack healthcheck does not check $want: $health"; continue ;;
        esac
        if [ "$dsn" != "-" ] && [ "$dsn" != "postgres://$want:secret@postgres:5432/$want?sslmode=disable" ]; then
            log_fail "$stack default DSN is $dsn"; continue
        fi
        log_pass "$stack defaults to $want"
    done
}

# --- the upgrade case: the retained volume's identity must reach every consumer ---
test_override_reaches_every_consumer() {
    local stack
    for stack in $STACKS; do
        log_test "override reaches every consumer: $stack"
        local out identity db user password health dsn
        out="$(POSTGRES_DB=keepme POSTGRES_USER=keepme POSTGRES_PASSWORD=keepmepw render "$stack")"
        if [ -z "$out" ]; then
            log_fail "$stack did not render with the override"; continue
        fi
        identity="$(echo "$out" | read_identity)"
        db="$(field "$identity" DB)"
        user="$(field "$identity" USER)"
        password="$(field "$identity" PASSWORD)"
        health="$(field "$identity" HEALTHCHECK)"
        dsn="$(field "$identity" DSN)"

        if [ "$db" != "keepme" ]; then
            log_fail "$stack ignored POSTGRES_DB (rendered '$db') — an upgrade onto its volume would fail"; continue
        fi
        if [ "$user" != "keepme" ]; then
            log_fail "$stack ignored POSTGRES_USER (rendered '$user')"; continue
        fi
        if [ "$password" != "keepmepw" ]; then
            log_fail "$stack ignored POSTGRES_PASSWORD (rendered '$password')"; continue
        fi
        case "$health" in
            *"-U keepme"*) ;;
            *) log_fail "$stack healthcheck still checks the default user: $health"; continue ;;
        esac
        # The DSN is the half-parameterised trap: the server would come up with the retained
        # identity while the application still asked for the new one.
        if [ "$dsn" != "-" ] && [ "$dsn" != "postgres://keepme:keepmepw@postgres:5432/keepme?sslmode=disable" ]; then
            log_fail "$stack DSN did not follow the override: $dsn"; continue
        fi
        log_pass "$stack honours the retained identity"
    done
}

main() {
    echo ""
    echo "Compose database identity"
    echo ""

    test_defaults
    test_override_reaches_every_consumer

    echo ""
    echo -e "${GREEN}Tests Passed: $TESTS_PASSED${NC}"
    echo -e "${RED}Tests Failed: $TESTS_FAILED${NC}"
    echo ""
    [ "$TESTS_FAILED" -eq 0 ]
}

main "$@"
