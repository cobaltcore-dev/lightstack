#!/bin/bash
# SPEC-007 WS-3 — Configurable catalog scheme integration test.
#
# Prerequisites: Lightstack running at $LIGHTSTACK_URL, started with LIGHTSTACK_ENDPOINT_SCHEME and
# LIGHTSTACK_ENDPOINT_HOST. Configure via env:
#   LIGHTSTACK_URL             base Keystone URL (default http://localhost:35357)
#   LIGHTSTACK_ADMIN_USER      (default: admin)
#   LIGHTSTACK_ADMIN_PASSWORD  (default: secret)
#   EXPECTED_SCHEME     scheme Lightstack was started with: http|https (default http)
#
# Verifies (quickstart §WS-3): every advertised service-catalog endpoint URL
# uses EXPECTED_SCHEME, and a resource self-link does too. Requires `jq`.

set -u
LIGHTSTACK_URL="${LIGHTSTACK_URL:-http://localhost:35357}"
ADMIN_USER="${LIGHTSTACK_ADMIN_USER:-admin}"
ADMIN_PASSWORD="${LIGHTSTACK_ADMIN_PASSWORD:-secret}"
EXPECTED_SCHEME="${EXPECTED_SCHEME:-http}"
GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
PASS=0; FAIL=0

resp=$(curl -s -X POST "$LIGHTSTACK_URL/v3/auth/tokens" -H 'Content-Type: application/json' \
  -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"$ADMIN_USER\",\"domain\":{\"name\":\"Default\"},\"password\":\"$ADMIN_PASSWORD\"}}},\"scope\":{\"project\":{\"name\":\"default\",\"domain\":{\"name\":\"Default\"}}}}}")

echo "== SPEC-007 WS-3: catalog scheme (expecting ${EXPECTED_SCHEME}://) =="

urls=$(echo "$resp" | jq -r '.token.catalog[].endpoints[].url' 2>/dev/null)
if [ -z "$urls" ]; then
  echo -e "${RED}✗${NC} no catalog URLs returned (auth failed?)"; exit 1
fi

bad=$(echo "$urls" | grep -vE "^${EXPECTED_SCHEME}://" || true)
if [ -z "$bad" ]; then
  echo -e "${GREEN}✓${NC} all catalog URLs use ${EXPECTED_SCHEME}://"; PASS=$((PASS+1))
else
  echo -e "${RED}✗${NC} some catalog URLs are not ${EXPECTED_SCHEME}://:"; echo "$bad"; FAIL=$((FAIL+1))
fi

# Self-link spot check: Nova server list 'links' (FR-013a). Best-effort.
nova_url=$(echo "$resp" | jq -r '.token.catalog[] | select(.type=="compute") | .endpoints[0].url' 2>/dev/null)
token=$(echo "$resp" | grep -i -o 'x-subject-token: [^[:space:]]*' 2>/dev/null | awk '{print $2}')
if [ -n "$nova_url" ] && [ -n "$token" ]; then
  links=$(curl -s -H "X-Auth-Token: $token" "$nova_url/servers" | jq -r '.. | .href? // empty' 2>/dev/null)
  if [ -n "$links" ] && echo "$links" | grep -vqE "^${EXPECTED_SCHEME}://"; then
    echo -e "${RED}✗${NC} a self-link is not ${EXPECTED_SCHEME}://"; FAIL=$((FAIL+1))
  else
    echo -e "${GREEN}✓${NC} self-links use ${EXPECTED_SCHEME}:// (or none present)"; PASS=$((PASS+1))
  fi
fi

echo "Passed: $PASS  Failed: $FAIL"
[ "$FAIL" -eq 0 ]
