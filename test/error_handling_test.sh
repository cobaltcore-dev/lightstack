#!/bin/bash
# Error Handling Standardization Test Suite

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0
TOTAL_TESTS=0

log_test() {
    ((TOTAL_TESTS++))
    echo -e "${YELLOW}[TEST $TOTAL_TESTS]${NC} $1"
}

log_pass() {
    echo -e "${GREEN}✓${NC} $1"
    ((TESTS_PASSED++))
}

log_fail() {
    echo -e "${RED}✗${NC} $1"
    ((TESTS_FAILED++))
}

# Get authentication token
TOKEN=$(curl -i -s -X POST "http://localhost:35357/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' \
    | grep -i "X-Subject-Token:" | awk '{print $2}' | tr -d '\r')

if [ -z "$TOKEN" ]; then
    echo -e "${RED}Failed to get authentication token${NC}"
    exit 1
fi

echo "Got authentication token"

# Server create refuses an image that does not exist or is not active, so the test boots
# from one it creates and uploads a few bytes to.
IMAGE_ID=$(curl -s -X POST "http://localhost:9292/v2/images" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"name": "error-handling-test-image", "disk_format": "raw", "container_format": "bare"}' | jq -r '.id')
curl -s -o /dev/null -X PUT "http://localhost:9292/v2/images/$IMAGE_ID/file" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/octet-stream" \
    --data-binary "lightstack test image"
echo ""

# Test 1: 404 Not Found - itemNotFound format
log_test "404 Not Found uses itemNotFound format"
RESPONSE=$(curl -s -X GET "http://localhost:8774/v2.1/servers/00000000-0000-0000-0000-999999999999" \
    -H "X-Auth-Token: $TOKEN")

if echo "$RESPONSE" | jq -e '.itemNotFound' > /dev/null 2>&1; then
    CODE=$(echo "$RESPONSE" | jq -r '.itemNotFound.code')
    MESSAGE=$(echo "$RESPONSE" | jq -r '.itemNotFound.message')
    if [ "$CODE" == "404" ]; then
        log_pass "404 uses itemNotFound format (code: $CODE)"
    else
        log_fail "404 format present but code incorrect: $CODE"
    fi
else
    log_fail "404 does not use itemNotFound format"
    echo "Response: $RESPONSE" | head -3
fi

# Test 2: 400 Bad Request - badRequest format
log_test "400 Bad Request uses badRequest format"
RESPONSE=$(curl -s -X POST "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"invalid":"data"}')

if echo "$RESPONSE" | jq -e '.badRequest' > /dev/null 2>&1; then
    CODE=$(echo "$RESPONSE" | jq -r '.badRequest.code')
    if [ "$CODE" == "400" ]; then
        log_pass "400 uses badRequest format (code: $CODE)"
    else
        log_fail "400 format present but code incorrect: $CODE"
    fi
else
    log_fail "400 does not use badRequest format"
    echo "Response: $RESPONSE" | head -3
fi

# Test 3: a server create over quota - 403 with the forbidden format, as upstream
log_test "Server create over quota answers 403 forbidden"
# First set a very low quota
curl -s -X PUT "http://localhost:8774/v2.1/os-quota-sets/00000000-0000-0000-0000-000000000002" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"quota_set":{"instances":0}}' > /dev/null

# Try to create an instance (should fail with 403)
RESPONSE=$(curl -s -X POST "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
        "server": {
            "name": "quota-test-vm",
            "flavorRef": "00000000-0000-0000-0000-000000000010",
            "imageRef": "'"$IMAGE_ID"'"
        }
    }')

if echo "$RESPONSE" | jq -e '.forbidden.message | startswith("Quota exceeded for instances: ")' > /dev/null 2>&1; then
    log_pass "403 quota error returned"
else
    log_fail "403 quota error format incorrect"
fi

# Reset quota
curl -s -X PUT "http://localhost:8774/v2.1/os-quota-sets/00000000-0000-0000-0000-000000000002" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"quota_set":{"instances":10}}' > /dev/null

# Test 4: 401 Unauthorized format
log_test "401 Unauthorized uses unauthorized format"
RESPONSE=$(curl -s -X GET "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: invalid-token")

if echo "$RESPONSE" | jq -e '.unauthorized // .error' > /dev/null 2>&1; then
    log_pass "401 unauthorized error returned"
else
    log_fail "401 format incorrect"
fi

# Test 5: Error message structure
log_test "Errors have message and code fields"
RESPONSE=$(curl -s -X GET "http://localhost:8774/v2.1/servers/non-existent-id" \
    -H "X-Auth-Token: $TOKEN")

# Parenthesised deliberately: jq's | binds looser than and, so
#   has("itemNotFound") and .itemNotFound | has("message")
# parses as (has(...) and .itemNotFound) | has("message") -- a BOOLEAN piped into
# has() -- which errors with "Cannot check whether boolean has a string key" and made
# this check unpassable regardless of what the API returned.
HAS_MESSAGE=$(echo "$RESPONSE" | jq 'has("itemNotFound") and (.itemNotFound | has("message"))')
HAS_CODE=$(echo "$RESPONSE" | jq 'has("itemNotFound") and (.itemNotFound | has("code"))')

if [ "$HAS_MESSAGE" == "true" ] && [ "$HAS_CODE" == "true" ]; then
    log_pass "Errors contain message and code fields"
else
    log_fail "Errors missing required fields"
fi

# Test 6: Consistent error format across services
log_test "Error format consistent across Nova/Neutron/Cinder"

# Nova 404
NOVA_404=$(curl -s -X GET "http://localhost:8774/v2.1/servers/00000000-0000-0000-0000-999999999999" \
    -H "X-Auth-Token: $TOKEN")

# Neutron 404
NEUTRON_404=$(curl -s -X GET "http://localhost:9696/v2.0/networks/00000000-0000-0000-0000-999999999999" \
    -H "X-Auth-Token: $TOKEN")

# Cinder 404
CINDER_404=$(curl -s -X GET "http://localhost:8776/v3/00000000-0000-0000-0000-000000000002/volumes/00000000-0000-0000-0000-999999999999" \
    -H "X-Auth-Token: $TOKEN")

NOVA_FORMAT=$(echo "$NOVA_404" | jq 'has("itemNotFound") or has("error")')
NEUTRON_FORMAT=$(echo "$NEUTRON_404" | jq 'has("NeutronError") or has("error")')
CINDER_FORMAT=$(echo "$CINDER_404" | jq 'has("itemNotFound") or has("error")')

if [ "$NOVA_FORMAT" == "true" ] && [ "$NEUTRON_FORMAT" == "true" ] && [ "$CINDER_FORMAT" == "true" ]; then
    log_pass "All services use structured error format"
else
    log_fail "Inconsistent error formats across services"
fi

# Test 7: HTTP status codes match error codes
log_test "HTTP status codes match error body codes"
RESPONSE=$(curl -i -s -X GET "http://localhost:8774/v2.1/servers/non-existent-id" \
    -H "X-Auth-Token: $TOKEN")

HTTP_CODE=$(echo "$RESPONSE" | grep "HTTP/" | awk '{print $2}')
BODY=$(echo "$RESPONSE" | grep -A 100 "^{" | head -20)
BODY_CODE=$(echo "$BODY" | jq -r '.itemNotFound.code // .error.code // "unknown"')

if [ "$HTTP_CODE" == "404" ] && [ "$BODY_CODE" == "404" ]; then
    log_pass "HTTP status code matches error body code (404)"
else
    log_fail "Status code mismatch (HTTP: $HTTP_CODE, Body: $BODY_CODE)"
fi

# Test 8: Error messages are user-friendly
log_test "Error messages are descriptive and user-friendly"
RESPONSE=$(curl -s -X GET "http://localhost:8774/v2.1/servers/00000000-0000-0000-0000-999999999999" \
    -H "X-Auth-Token: $TOKEN")

MESSAGE=$(echo "$RESPONSE" | jq -r '.itemNotFound.message // .error.message // ""')

if [[ "$MESSAGE" =~ "could not be found" ]] || [[ "$MESSAGE" =~ "not found" ]]; then
    log_pass "Error message is user-friendly: $MESSAGE"
else
    log_fail "Error message not user-friendly: $MESSAGE"
fi

# Test 9: No sensitive information in errors
log_test "Errors don't expose sensitive information"
RESPONSE=$(curl -s -X GET "http://localhost:8774/v2.1/servers" \
    -H "X-Auth-Token: invalid-token")

if echo "$RESPONSE" | grep -qi "database\|sql\|password\|secret\|stack trace"; then
    log_fail "Error exposes sensitive information"
else
    log_pass "No sensitive information in error responses"
fi

# Test 10: 500 errors use computeFault format
log_test "Internal server errors use computeFault format (if implemented)"
# This test verifies the error structure exists even if hard to trigger
if grep -q "computeFault" internal/common/errors.go 2>/dev/null; then
    log_pass "computeFault error type defined for 500 errors"
else
    log_fail "computeFault error type not found"
fi

curl -s -X DELETE "http://localhost:9292/v2/images/$IMAGE_ID" -H "X-Auth-Token: $TOKEN" > /dev/null

# Print summary
echo ""
echo "========================================"
echo "   ERROR HANDLING TEST SUMMARY"
echo "========================================"
echo -e "Total Tests:  $TOTAL_TESTS"
echo -e "${GREEN}Passed:       $TESTS_PASSED${NC}"
echo -e "${RED}Failed:       $TESTS_FAILED${NC}"
echo "========================================"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}✅ ALL ERROR HANDLING TESTS PASSED${NC}"
    echo ""
    echo "Error Format Summary:"
    echo "  - 404: itemNotFound"
    echo "  - 400: badRequest"
    echo "  - 401: unauthorized"
    echo "  - 403: forbidden"
    echo "  - 409: conflict / conflictingRequest"
    echo "  - 413: overLimit (block-storage quota)"
    echo "  - 500: computeFault"
    echo "  - 503: serviceUnavailable"
    exit 0
else
    echo -e "${RED}❌ SOME TESTS FAILED${NC}"
    echo ""
    echo "Note: Some failures expected if error standardization"
    echo "      has not been fully applied to all services yet."
    exit 1
fi
