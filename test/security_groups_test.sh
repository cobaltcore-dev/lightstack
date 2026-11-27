#!/bin/bash
# Security Groups Test Suite

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
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

# Get token, and the project it is scoped to. The token is the seeded administrator's, whose
# listings span every project, as upstream's (SPEC-033 FR-057), so they are narrowed to this one.
AUTH_RESPONSE=$(curl -i -s -X POST "http://localhost:35357/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}')
TOKEN=$(echo "$AUTH_RESPONSE" | grep -i "X-Subject-Token:" | awk '{print $2}' | tr -d '\r')
PROJECT_ID=$(echo "$AUTH_RESPONSE" | tr -d '\r' | sed '1,/^$/d' | jq -r '.token.project.id')

log_pass "Got authentication token"

# Test 1: List security groups (should include default)
log_test "List security groups"
SG_LIST=$(curl -s -X GET "http://localhost:9696/v2.0/security-groups?project_id=$PROJECT_ID" \
    -H "X-Auth-Token: $TOKEN")

DEFAULT_EXISTS=$(echo "$SG_LIST" | jq -r '.security_groups[] | select(.name=="default") | .id' 2>/dev/null)

if [ -n "$DEFAULT_EXISTS" ]; then
    log_pass "Default security group exists"
else
    log_fail "Default security group not found"
fi

# Test 2: Create security group
log_test "Create security group"
SG_RESPONSE=$(curl -s -X POST "http://localhost:9696/v2.0/security-groups" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{
        "security_group": {
            "name": "test-sg",
            "description": "Test security group"
        }
    }')

SG_ID=$(echo "$SG_RESPONSE" | jq -r '.security_group.id')

if [ -n "$SG_ID" ] && [ "$SG_ID" != "null" ]; then
    log_pass "Created security group: $SG_ID"
else
    log_fail "Failed to create security group"
    echo "Response: $SG_RESPONSE"
fi

# Test 3: Get security group details
log_test "Get security group details"
SG_DETAILS=$(curl -s -X GET "http://localhost:9696/v2.0/security-groups/$SG_ID" \
    -H "X-Auth-Token: $TOKEN")

SG_NAME=$(echo "$SG_DETAILS" | jq -r '.security_group.name')

if [ "$SG_NAME" == "test-sg" ]; then
    log_pass "Retrieved security group details"
else
    log_fail "Failed to get security group details"
fi

# Test 4: Create security group rule (allow SSH)
log_test "Create security group rule (allow SSH ingress)"
RULE_RESPONSE=$(curl -s -X POST "http://localhost:9696/v2.0/security-group-rules" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{
        \"security_group_rule\": {
            \"security_group_id\": \"$SG_ID\",
            \"direction\": \"ingress\",
            \"ethertype\": \"IPv4\",
            \"protocol\": \"tcp\",
            \"port_range_min\": 22,
            \"port_range_max\": 22,
            \"remote_ip_prefix\": \"0.0.0.0/0\"
        }
    }")

RULE_ID=$(echo "$RULE_RESPONSE" | jq -r '.security_group_rule.id')

if [ -n "$RULE_ID" ] && [ "$RULE_ID" != "null" ]; then
    log_pass "Created SSH ingress rule: $RULE_ID"
else
    log_fail "Failed to create security group rule"
    echo "Response: $RULE_RESPONSE"
fi

# Test 5: Create another rule (allow HTTP)
log_test "Create security group rule (allow HTTP ingress)"
RULE2_RESPONSE=$(curl -s -X POST "http://localhost:9696/v2.0/security-group-rules" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{
        \"security_group_rule\": {
            \"security_group_id\": \"$SG_ID\",
            \"direction\": \"ingress\",
            \"ethertype\": \"IPv4\",
            \"protocol\": \"tcp\",
            \"port_range_min\": 80,
            \"port_range_max\": 80
        }
    }")

RULE2_ID=$(echo "$RULE2_RESPONSE" | jq -r '.security_group_rule.id')

if [ -n "$RULE2_ID" ] && [ "$RULE2_ID" != "null" ]; then
    log_pass "Created HTTP ingress rule: $RULE2_ID"
else
    log_fail "Failed to create HTTP rule"
fi

# Test 6: Create egress rule (allow all)
log_test "Create security group rule (allow all egress)"
RULE3_RESPONSE=$(curl -s -X POST "http://localhost:9696/v2.0/security-group-rules" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{
        \"security_group_rule\": {
            \"security_group_id\": \"$SG_ID\",
            \"direction\": \"egress\",
            \"ethertype\": \"IPv4\"
        }
    }")

RULE3_ID=$(echo "$RULE3_RESPONSE" | jq -r '.security_group_rule.id')

if [ -n "$RULE3_ID" ] && [ "$RULE3_ID" != "null" ]; then
    log_pass "Created egress rule: $RULE3_ID"
else
    log_fail "Failed to create egress rule"
fi

# Test 7: List security group rules
log_test "List security group rules"
RULES=$(curl -s -X GET "http://localhost:9696/v2.0/security-group-rules" \
    -H "X-Auth-Token: $TOKEN")

RULE_COUNT=$(echo "$RULES" | jq "[.security_group_rules[] | select(.security_group_id==\"$SG_ID\")] | length")

if [ "$RULE_COUNT" -ge 3 ]; then
    log_pass "Found $RULE_COUNT rules for security group"
else
    log_fail "Expected at least 3 rules, found $RULE_COUNT"
fi

# Test 8: Verify rules in security group details
log_test "Verify rules appear in security group details"
SG_WITH_RULES=$(curl -s -X GET "http://localhost:9696/v2.0/security-groups/$SG_ID" \
    -H "X-Auth-Token: $TOKEN")

EMBEDDED_RULE_COUNT=$(echo "$SG_WITH_RULES" | jq '.security_group.security_group_rules | length')

if [ "$EMBEDDED_RULE_COUNT" -ge 3 ]; then
    log_pass "Security group has $EMBEDDED_RULE_COUNT embedded rules"
else
    log_fail "Expected at least 3 embedded rules, found $EMBEDDED_RULE_COUNT"
fi

# Test 9: Delete a security group rule
log_test "Delete security group rule"
HTTP_CODE=$(curl -s -w "%{http_code}" -o /dev/null -X DELETE "http://localhost:9696/v2.0/security-group-rules/$RULE_ID" \
    -H "X-Auth-Token: $TOKEN")

if [ "$HTTP_CODE" == "204" ]; then
    log_pass "Deleted security group rule (HTTP 204)"
else
    log_fail "Failed to delete rule (HTTP $HTTP_CODE)"
fi

# Test 10: Verify rule was deleted
log_test "Verify rule was deleted"
sleep 1
REMAINING_RULES=$(curl -s -X GET "http://localhost:9696/v2.0/security-group-rules" \
    -H "X-Auth-Token: $TOKEN")

DELETED_RULE_EXISTS=$(echo "$REMAINING_RULES" | jq -r ".security_group_rules[] | select(.id==\"$RULE_ID\") | .id")

if [ -z "$DELETED_RULE_EXISTS" ]; then
    log_pass "Rule successfully deleted"
else
    log_fail "Rule still exists after deletion"
fi

# Test 11: Try to delete default security group (should fail)
# Upstream refuses a non-administrator's delete of `default` with 409
# SecurityGroupCannotRemoveDefault, even when no port carries it; an administrator
# may delete an unused one (SPEC-033 FR-024). This script's token is the seeded
# administrator's, so the delete is made as a member — of a fresh project, whose `default` no
# port carries: a group a port carries is refused as SecurityGroupInUse first, as upstream
# checks use before the name, so only the body's type tells FR-024's refusal apart
# (SPEC-033 T236).
log_test "Try to delete default security group (should fail)"
MEMBER_NAME="sg-test-member-$$-$RANDOM"
MEMBER_PROJECT_ID=$(curl -s -X POST "http://localhost:35357/v3/projects" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"project":{"name":"'"$MEMBER_NAME"'","domain_id":"default"}}' \
    | jq -r '.project.id')
MEMBER_ID=$(curl -s -X POST "http://localhost:35357/v3/users" \
    -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"user":{"name":"'"$MEMBER_NAME"'","password":"sg-test-member","domain_id":"default"}}' \
    | jq -r '.user.id')
trap 'curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/users/$MEMBER_ID" -H "X-Auth-Token: $TOKEN"; curl -s -o /dev/null -X DELETE "http://localhost:35357/v3/projects/$MEMBER_PROJECT_ID" -H "X-Auth-Token: $TOKEN"' EXIT
MEMBER_ROLE_ID=$(curl -s -X GET "http://localhost:35357/v3/roles" \
    -H "X-Auth-Token: $TOKEN" | jq -r '.roles[] | select(.name=="member") | .id')
curl -s -o /dev/null -X PUT \
    "http://localhost:35357/v3/projects/$MEMBER_PROJECT_ID/users/$MEMBER_ID/roles/$MEMBER_ROLE_ID" \
    -H "X-Auth-Token: $TOKEN"
MEMBER_TOKEN=$(curl -i -s -X POST "http://localhost:35357/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"'"$MEMBER_NAME"'","password":"sg-test-member","domain":{"name":"Default"}}}},"scope":{"project":{"id":"'"$MEMBER_PROJECT_ID"'"}}}}' \
    | grep -i "X-Subject-Token:" | awk '{print $2}' | tr -d '\r')
# Listing the project's groups gives it its default, as upstream's does.
MEMBER_DEFAULT_ID=$(curl -s -X GET "http://localhost:9696/v2.0/security-groups" \
    -H "X-Auth-Token: $MEMBER_TOKEN" | jq -r '.security_groups[] | select(.name=="default") | .id' 2>/dev/null)

if [ -z "$MEMBER_TOKEN" ] || [ -z "$MEMBER_DEFAULT_ID" ]; then
    log_fail "Could not get a member's token and the fresh project's default group to try the delete with"
else
    RESPONSE=$(curl -s -w "\n%{http_code}" -X DELETE "http://localhost:9696/v2.0/security-groups/$MEMBER_DEFAULT_ID" \
        -H "X-Auth-Token: $MEMBER_TOKEN")
    HTTP_CODE=$(echo "$RESPONSE" | tail -n1)
    ERROR_TYPE=$(echo "$RESPONSE" | sed '$d' | jq -r '.NeutronError.type' 2>/dev/null)

    if [ "$HTTP_CODE" == "409" ] && [ "$ERROR_TYPE" == "SecurityGroupCannotRemoveDefault" ]; then
        log_pass "Correctly rejected a member's deletion of the default security group (HTTP $HTTP_CODE, $ERROR_TYPE)"
    else
        log_fail "Should have rejected a member's default group deletion with 409 SecurityGroupCannotRemoveDefault (got HTTP $HTTP_CODE, $ERROR_TYPE)"
    fi
fi

# Test 12: Delete custom security group
log_test "Delete custom security group"
HTTP_CODE=$(curl -s -w "%{http_code}" -o /dev/null -X DELETE "http://localhost:9696/v2.0/security-groups/$SG_ID" \
    -H "X-Auth-Token: $TOKEN")

if [ "$HTTP_CODE" == "204" ]; then
    log_pass "Deleted security group (HTTP 204)"
else
    log_fail "Failed to delete security group (HTTP $HTTP_CODE)"
fi

# Test 13: Verify security group was deleted
log_test "Verify security group was deleted"
sleep 1
DELETED_SG=$(curl -s -X GET "http://localhost:9696/v2.0/security-groups/$SG_ID" \
    -H "X-Auth-Token: $TOKEN" \
    -w "\n%{http_code}")

HTTP_CODE=$(echo "$DELETED_SG" | tail -1)

if [ "$HTTP_CODE" == "404" ]; then
    log_pass "Security group no longer exists (HTTP 404)"
else
    log_fail "Security group still exists (HTTP $HTTP_CODE)"
fi

# Print summary
echo ""
echo "========================================"
echo "     SECURITY GROUPS TEST SUMMARY"
echo "========================================"
echo -e "Total Tests:  $TOTAL_TESTS"
echo -e "${GREEN}Passed:       $TESTS_PASSED${NC}"
echo -e "${RED}Failed:       $TESTS_FAILED${NC}"
echo "========================================"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}✅ ALL SECURITY GROUP TESTS PASSED${NC}"
    exit 0
else
    echo -e "${RED}❌ SOME TESTS FAILED${NC}"
    exit 1
fi
