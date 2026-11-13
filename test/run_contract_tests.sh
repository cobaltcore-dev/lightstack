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

# Run contract tests for each service
SERVICES=("keystone" "nova" "cinder" "neutron" "glance")
TOTAL_PASS=0
TOTAL_FAIL=0
TOTAL_TESTS=0
BROKEN_SUITES=0

for SERVICE in "${SERVICES[@]}"; do
    echo "============================="
    echo "Testing $SERVICE"
    echo "============================="

    cd "$SCRIPT_DIR/contract/$SERVICE"

    # Run tests with timeout and JSON output.
    # `|| true`: a non-zero go test exit (failing tests) must not abort the
    # script under `set -e` — the counts below decide the final exit code.
    TEST_RC=0
    TEST_OUTPUT=$(go test -json -count=1 -timeout=3m 2>&1) || TEST_RC=$?

    # Count results. `|| true`: jq exits non-zero if go test mixed non-JSON
    # stderr (e.g. build errors) into the stream; the counts we did get are
    # still what we want to report, and zero matches is a normal outcome.
    PASS_COUNT=$(echo "$TEST_OUTPUT" | jq -r 'select(.Action == "pass" and .Test != null) | .Test' | wc -l | tr -d ' ' || true)
    FAIL_COUNT=$(echo "$TEST_OUTPUT" | jq -r 'select(.Action == "fail" and .Test != null) | .Test' | wc -l | tr -d ' ' || true)
    if [ "$TEST_RC" -ne 0 ] || [ "$((PASS_COUNT + FAIL_COUNT))" -eq 0 ]; then
        BROKEN_SUITES=$((BROKEN_SUITES + 1))
        echo "ERROR: $SERVICE did not complete a passing test run (exit $TEST_RC)"
    fi

    TOTAL_PASS=$((TOTAL_PASS + PASS_COUNT))
    TOTAL_FAIL=$((TOTAL_FAIL + FAIL_COUNT))
    TOTAL_TESTS=$((TOTAL_TESTS + PASS_COUNT + FAIL_COUNT))

    if [ "$FAIL_COUNT" -eq 0 ]; then
        echo -e "${GREEN}✓ All $PASS_COUNT tests passed${NC}"
    else
        echo -e "${YELLOW}⚠ $PASS_COUNT passed, $FAIL_COUNT failed${NC}"
        echo ""
        echo "Failed tests:"
        echo "$TEST_OUTPUT" | jq -r 'select(.Action == "fail" and .Test != null) | .Test' || true
    fi
    echo ""
done

# Summary
echo "============================="
echo "Test Summary"
echo "============================="
echo "Total Tests: $TOTAL_TESTS"
echo -e "Passed: ${GREEN}$TOTAL_PASS${NC}"
echo -e "Failed: ${RED}$TOTAL_FAIL${NC}"

# Guard the division: if every test was skipped TOTAL_TESTS is 0 and awk
# would abort with "division by zero" (fatal under `set -e`).
if [ "$TOTAL_TESTS" -gt 0 ]; then
    PASS_RATE=$(awk "BEGIN {printf \"%.1f\", ($TOTAL_PASS/$TOTAL_TESTS)*100}")
else
    PASS_RATE=0.0
fi
echo "Pass Rate: $PASS_RATE%"

if [ "$TOTAL_FAIL" -eq 0 ] && [ "$BROKEN_SUITES" -eq 0 ]; then
    echo -e "${GREEN}✓ All tests passed!${NC}"
    exit 0
else
    echo -e "${YELLOW}⚠ Some tests failed${NC}"
    exit 1
fi
