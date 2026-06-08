#!/bin/bash
# SPEC-007 WS-2 — Admin-password bootstrap integration test.
#
# Prerequisites: O3K running at $O3K_URL (default http://localhost:35357),
# started with keystone.admin_password set. Configure expectations via env:
#   O3K_URL             base Keystone URL (default http://localhost:35357)
#   O3K_ADMIN_USER      configured admin name        (default: admin)
#   O3K_ADMIN_PASSWORD  configured admin password    (default: secret, dev)
#   O3K_LOG_FILE        optional O3K log file to scan for password leakage
#
# Verifies (quickstart §WS-2): the configured password authenticates; the
# well-known 'secret' is rejected when the configured password is non-default;
# for a custom admin_user the seeded admin/secret is disabled; the password is
# never written to the log file.

set -u
O3K_URL="${O3K_URL:-http://localhost:35357}"
ADMIN_USER="${O3K_ADMIN_USER:-admin}"
ADMIN_PASSWORD="${O3K_ADMIN_PASSWORD:-secret}"
GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
PASS=0; FAIL=0

auth_status() { # user pass -> HTTP status code
  curl -s -o /dev/null -w '%{http_code}' -X POST "$O3K_URL/v3/auth/tokens" \
    -H 'Content-Type: application/json' \
    -d "{\"auth\":{\"identity\":{\"methods\":[\"password\"],\"password\":{\"user\":{\"name\":\"$1\",\"domain\":{\"name\":\"Default\"},\"password\":\"$2\"}}}}}"
}
check() { # description expected actual
  if [ "$2" = "$3" ]; then echo -e "${GREEN}✓${NC} $1"; PASS=$((PASS+1));
  else echo -e "${RED}✗${NC} $1 (expected $2, got $3)"; FAIL=$((FAIL+1)); fi
}

echo "== SPEC-007 WS-2: admin-password bootstrap =="
check "configured admin password authenticates" 201 "$(auth_status "$ADMIN_USER" "$ADMIN_PASSWORD")"

if [ "$ADMIN_PASSWORD" != "secret" ]; then
  check "well-known 'secret' is rejected" 401 "$(auth_status "$ADMIN_USER" secret)"
fi

if [ "$ADMIN_USER" != "admin" ]; then
  check "seeded admin/secret disabled (custom admin_user)" 401 "$(auth_status admin secret)"
fi

if [ -n "${O3K_LOG_FILE:-}" ] && [ -f "${O3K_LOG_FILE}" ]; then
  if grep -qF -- "$ADMIN_PASSWORD" "$O3K_LOG_FILE"; then
    echo -e "${RED}✗${NC} admin password found in log file"; FAIL=$((FAIL+1));
  else
    echo -e "${GREEN}✓${NC} admin password absent from log file"; PASS=$((PASS+1));
  fi
fi

echo "Passed: $PASS  Failed: $FAIL"
[ "$FAIL" -eq 0 ]
