#!/bin/bash
# Quick test of critical Horizon functionality

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

PASSED=0
FAILED=0

# Both helpers end in an assignment, whose status is always 0 -- so neither can trip `set -e`.
pass() { echo -e "   ${GREEN}✓ PASS${NC}${1:+ - $1}"; PASSED=$((PASSED + 1)); }
fail() { echo -e "   ${RED}✗ FAIL${NC}${1:+ - $1}"; FAILED=$((FAILED + 1)); }

# Host running Lightstack. Defaults to localhost so this works against the docker-compose stack;
# override for a remote host, e.g. LIGHTSTACK_HOST=10.2.199.101 (SPEC-018 T032).
LIGHTSTACK_HOST="${LIGHTSTACK_HOST:-localhost}"
# Admin credential: the hatch-armed dev stacks keep the seeded admin/secret;
# the horizon stack requires a real LIGHTSTACK_ADMIN_PASSWORD (BootstrapAdmin
# replaces the seeded hash with it), so prefer OS_PASSWORD / that variable.
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${OS_PASSWORD:-${LIGHTSTACK_ADMIN_PASSWORD:-secret}}}"

echo "Testing critical Horizon endpoints..."
echo ""

# Get auth token
TOKEN=$(curl -s -X POST http://${LIGHTSTACK_HOST}:35357/v3/auth/tokens \
  -H "Content-Type: application/json" \
  -d '{
    "auth": {
      "identity": {
        "methods": ["password"],
        "password": {
          "user": {"name": "admin", "domain": {"name": "Default"}, "password": "'"${ADMIN_PASSWORD:-${LIGHTSTACK_ADMIN_PASSWORD:-secret}}"'"}
        }
      },
      "scope": {"project": {"name": "default", "domain": {"name": "Default"}}}
    }
  }' | jq -r '.token.catalog[] | select(.type == "network") | .endpoints[0].url')

AUTH_TOKEN=$(curl -s -i -X POST http://${LIGHTSTACK_HOST}:35357/v3/auth/tokens \
  -H "Content-Type: application/json" \
  -d '{
    "auth": {
      "identity": {
        "methods": ["password"],
        "password": {
          "user": {"name": "admin", "domain": {"name": "Default"}, "password": "'"${ADMIN_PASSWORD:-${LIGHTSTACK_ADMIN_PASSWORD:-secret}}"'"}
        }
      },
      "scope": {"project": {"name": "default", "domain": {"name": "Default"}}}
    }
  }' | grep -i "^X-Subject-Token:" | cut -d' ' -f2 | tr -d '\r')

if [ -z "$AUTH_TOKEN" ]; then
    echo -e "${RED}✗ FAIL${NC} - no X-Subject-Token from Keystone; every check below would read as a 401"
    exit 1
fi

PROJECT_ID=$(curl -s -X POST http://${LIGHTSTACK_HOST}:35357/v3/auth/tokens \
  -H "Content-Type: application/json" \
  -d '{
    "auth": {
      "identity": {
        "methods": ["password"],
        "password": {
          "user": {"name": "admin", "domain": {"name": "Default"}, "password": "'"${ADMIN_PASSWORD:-${LIGHTSTACK_ADMIN_PASSWORD:-secret}}"'"}
        }
      },
      "scope": {"project": {"name": "default", "domain": {"name": "Default"}}}
    }
  }' | jq -r '.token.project.id')

echo "1. Service Catalog - Neutron endpoint"
echo "   Expected: <scheme>://<host>:9696 with no path suffix (notably no /v2.0)"
echo "   Got: $TOKEN"
# The regression guarded here is a path suffix on the catalog URL, not the host. The advertised
# host comes from the server's LIGHTSTACK_ENDPOINT_HOST (internal/keystone/auth.go:697, default
# "localhost"), which is unrelated to the LIGHTSTACK_HOST this script connects to -- so asserting a
# literal host would fail on every deployment except docker-compose-horizon.yml.
if [[ "$TOKEN" =~ ^https?://[^/]+:9696/?$ ]]; then
    pass
else
    fail
fi
echo ""

echo "2. Nova - List servers with addresses field"
SERVERS=$(curl -s -H "X-Auth-Token: $AUTH_TOKEN" \
  "http://${LIGHTSTACK_HOST}:8774/v2.1/$PROJECT_ID/servers/detail")
if echo "$SERVERS" | jq -e '.servers[0].addresses' > /dev/null 2>&1; then
    pass "addresses field present"
elif echo "$SERVERS" | jq -e '.servers | length == 0' > /dev/null 2>&1; then
    pass "no servers (addresses field will be added when servers exist)"
else
    fail "addresses field missing"
    echo "   Response: $SERVERS" | head -c 200
fi
echo ""

echo "3. Neutron - Get quota details"
QUOTA=$(curl -s -w "\n%{http_code}" -H "X-Auth-Token: $AUTH_TOKEN" \
  "http://${LIGHTSTACK_HOST}:9696/v2.0/quotas/$PROJECT_ID/details")
STATUS=$(echo "$QUOTA" | tail -1)
BODY=$(echo "$QUOTA" | head -n -1)

if [ "$STATUS" = "200" ]; then
    pass "HTTP 200"
    echo "   Response:"
    printf '%s' "$BODY" | jq '.quota' | head -10
else
    fail "HTTP $STATUS"
    echo "   Response: $BODY"
fi
echo ""

echo "4. Neutron - List networks"
NETWORKS=$(curl -s -w "\n%{http_code}" -H "X-Auth-Token: $AUTH_TOKEN" \
  "http://${LIGHTSTACK_HOST}:9696/v2.0/networks")
STATUS=$(echo "$NETWORKS" | tail -1)

if [ "$STATUS" = "200" ]; then
    pass "HTTP 200"
else
    fail "HTTP $STATUS"
fi
echo ""

echo "5. Neutron - List routers"
ROUTERS=$(curl -s -w "\n%{http_code}" -H "X-Auth-Token: $AUTH_TOKEN" \
  "http://${LIGHTSTACK_HOST}:9696/v2.0/routers")
STATUS=$(echo "$ROUTERS" | tail -1)

if [ "$STATUS" = "200" ]; then
    pass "HTTP 200"
else
    fail "HTTP $STATUS"
fi
echo ""

echo "6. Neutron - List floating IPs"
FIPS=$(curl -s -w "\n%{http_code}" -H "X-Auth-Token: $AUTH_TOKEN" \
  "http://${LIGHTSTACK_HOST}:9696/v2.0/floatingips")
STATUS=$(echo "$FIPS" | tail -1)

if [ "$STATUS" = "200" ]; then
    pass "HTTP 200"
else
    fail "HTTP $STATUS"
fi
echo ""

echo "================================"
echo "Critical endpoints test complete"
echo "Passed: $PASSED  Failed: $FAILED"
echo "================================"

[ "$FAILED" -eq 0 ] || exit 1
