#!/bin/bash
# Comprehensive Lightstack API Contract Testing Script
# Tests all services against OpenStack 2025.2 compatibility

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Derive the test directory from this script's own location, so the script
# works from any checkout/user/machine.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Configuration
export OS_AUTH_URL=${OS_AUTH_URL:-http://localhost:35357/v3}
export OS_USERNAME=${OS_USERNAME:-admin}
export OS_PASSWORD=${OS_PASSWORD:-secret}
export OS_PROJECT_NAME=${OS_PROJECT_NAME:-default}
export OS_USER_DOMAIN_NAME=${OS_USER_DOMAIN_NAME:-Default}
export OS_PROJECT_DOMAIN_NAME=${OS_PROJECT_DOMAIN_NAME:-Default}

echo "============================="
echo "Lightstack API Contract Test Suite"
echo "============================="
echo "Auth URL: $OS_AUTH_URL"
echo ""

# Check if Lightstack is running
echo "Checking Lightstack availability..."
if ! curl -sf "$OS_AUTH_URL" > /dev/null 2>&1; then
    echo -e "${RED}ERROR: Lightstack is not running at $OS_AUTH_URL${NC}"
    echo "Start Lightstack with: docker compose -f deployments/docker-compose.yml up -d"
    exit 1
fi
echo -e "${GREEN}✓ Lightstack is running${NC}"
echo ""

# Test authentication
echo "Testing authentication..."
if ! openstack token issue > /dev/null 2>&1; then
    echo -e "${RED}ERROR: Authentication failed${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Authentication successful${NC}"
echo ""

# Run every Go package under test/contract, including the root package.
cd "$SCRIPT_DIR/contract"
TEST_RC=0
TEST_OUTPUT=$(go test -json -count=1 -timeout=10m ./...) || TEST_RC=$?

# go test -json writes structured events to stdout. Keep stderr separate: module
# download and compiler diagnostics must not corrupt the JSON event stream.
PASS_COUNT=$(printf '%s\n' "$TEST_OUTPUT" | jq -r 'select(.Action == "pass" and .Test != null) | .Test' | wc -l | tr -d ' ')
FAIL_COUNT=$(printf '%s\n' "$TEST_OUTPUT" | jq -r 'select(.Action == "fail" and .Test != null) | .Test' | wc -l | tr -d ' ')
TOTAL_TESTS=$((PASS_COUNT + FAIL_COUNT))
BROKEN_SUITES=0
if [ "$TEST_RC" -ne 0 ] || [ "$TOTAL_TESTS" -eq 0 ]; then
    BROKEN_SUITES=1
    echo "ERROR: contract packages did not complete a passing test run (exit $TEST_RC)" >&2
fi
if [ "$FAIL_COUNT" -gt 0 ]; then
    echo "Failed tests:" >&2
    printf '%s\n' "$TEST_OUTPUT" | jq -r 'select(.Action == "fail" and .Test != null) | .Package + "." + .Test' >&2
fi

echo "============================="
echo "Test Summary"
echo "============================="
echo "Total Tests: $TOTAL_TESTS"
echo -e "Passed: ${GREEN}$PASS_COUNT${NC}"
echo -e "Failed: ${RED}$FAIL_COUNT${NC}"

if [ "$TOTAL_TESTS" -gt 0 ]; then
    PASS_RATE=$(awk "BEGIN {printf \"%.1f\", ($PASS_COUNT/$TOTAL_TESTS)*100}")
else
    PASS_RATE=0.0
fi
echo "Pass Rate: $PASS_RATE%"

if [ "$FAIL_COUNT" -eq 0 ] && [ "$BROKEN_SUITES" -eq 0 ]; then
    echo -e "${GREEN}✓ All contract tests passed!${NC}"
    exit 0
fi
echo -e "${YELLOW}⚠ Some contract tests failed${NC}"
exit 1
