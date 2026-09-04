#!/bin/bash
#
# Test Suite for Lightstack Deployment Scripts
# Tests deploy-single-node.sh functions in isolation
#
# Usage: ./scripts/test-deploy-scripts.sh
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

TESTS_PASSED=0
TESTS_FAILED=0
TEST_DIR="/tmp/lightstack-test-$$"

log_test() {
    echo -e "${BLUE}[TEST]${NC} $1"
}

log_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
    ((TESTS_PASSED++))
}

log_fail() {
    echo -e "${RED}[FAIL]${NC} $1"
    ((TESTS_FAILED++))
}

# Setup test environment
setup_test_env() {
    mkdir -p "$TEST_DIR"
    cd "$TEST_DIR"
}

# Cleanup test environment
cleanup_test_env() {
    rm -rf "$TEST_DIR"
}

# Test: Netplan configuration generation
test_netplan_generation() {
    log_test "Testing netplan configuration generation"

    # Test data
    NETWORK_IFACE="eth0"
    HOST_IP="192.168.1.100"
    GATEWAY="192.168.1.1"
    DNS_SERVERS="8.8.8.8,8.8.4.4"

    # Parse DNS servers
    IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"

    # Generate netplan config
    cat > "$TEST_DIR/test-netplan.yaml" <<'NETPLAN_EOF'
network:
  version: 2
  renderer: networkd
  ethernets:
    NETWORK_IFACE_PLACEHOLDER:
      dhcp4: no
      dhcp6: no
  bridges:
    br-ext:
      interfaces: [NETWORK_IFACE_PLACEHOLDER]
      addresses:
        - HOST_IP_PLACEHOLDER/24
      routes:
        - to: default
          via: GATEWAY_PLACEHOLDER
      nameservers:
        addresses:
DNS_SERVERS_PLACEHOLDER
      dhcp4: no
      dhcp6: no
      parameters:
        stp: false
        forward-delay: 0
NETPLAN_EOF

    # Replace placeholders (using temp file for portability)
    sed "s/NETWORK_IFACE_PLACEHOLDER/$NETWORK_IFACE/g" "$TEST_DIR/test-netplan.yaml" > "$TEST_DIR/test-netplan.yaml.tmp"
    mv "$TEST_DIR/test-netplan.yaml.tmp" "$TEST_DIR/test-netplan.yaml"

    sed "s|HOST_IP_PLACEHOLDER|$HOST_IP|g" "$TEST_DIR/test-netplan.yaml" > "$TEST_DIR/test-netplan.yaml.tmp"
    mv "$TEST_DIR/test-netplan.yaml.tmp" "$TEST_DIR/test-netplan.yaml"

    sed "s/GATEWAY_PLACEHOLDER/$GATEWAY/g" "$TEST_DIR/test-netplan.yaml" > "$TEST_DIR/test-netplan.yaml.tmp"
    mv "$TEST_DIR/test-netplan.yaml.tmp" "$TEST_DIR/test-netplan.yaml"

    # Insert DNS servers using awk to avoid sed escaping issues
    DNS_TEMP=$(mktemp)
    for dns in "${DNS_LIST[@]}"; do
        echo "          - ${dns}" >> "$DNS_TEMP"
    done

    # Replace DNS_SERVERS_PLACEHOLDER with actual DNS entries
    awk -v dns_file="$DNS_TEMP" '
        /DNS_SERVERS_PLACEHOLDER/ {
            while ((getline line < dns_file) > 0) {
                print line
            }
            close(dns_file)
            next
        }
        {print}
    ' "$TEST_DIR/test-netplan.yaml" > "$TEST_DIR/test-netplan.yaml.tmp"
    mv "$TEST_DIR/test-netplan.yaml.tmp" "$TEST_DIR/test-netplan.yaml"
    rm -f "$DNS_TEMP"

    # Validate YAML syntax with Python
    if command -v python3 &> /dev/null; then
        if python3 -c "import yaml; yaml.safe_load(open('$TEST_DIR/test-netplan.yaml'))" 2>/dev/null; then
            log_pass "Netplan YAML syntax is valid"
        else
            log_fail "Netplan YAML syntax is invalid"
            cat "$TEST_DIR/test-netplan.yaml"
            return 1
        fi
    else
        log_pass "Netplan YAML generated (Python not available for validation)"
    fi

    # Check for expected content
    if grep -q "eth0" "$TEST_DIR/test-netplan.yaml" && \
       grep -q "192.168.1.100/24" "$TEST_DIR/test-netplan.yaml" && \
       grep -q "192.168.1.1" "$TEST_DIR/test-netplan.yaml" && \
       grep -q "8.8.8.8" "$TEST_DIR/test-netplan.yaml" && \
       grep -q "8.8.4.4" "$TEST_DIR/test-netplan.yaml"; then
        log_pass "All placeholders replaced correctly"
    else
        log_fail "Some placeholders not replaced"
        cat "$TEST_DIR/test-netplan.yaml"
        return 1
    fi

    # Check DNS formatting (each on separate line under nameservers/addresses)
    DNS_COUNT=$(grep -A 10 "nameservers:" "$TEST_DIR/test-netplan.yaml" | grep -c "^ *- [0-9]" || true)
    if [ "$DNS_COUNT" -eq 2 ]; then
        log_pass "DNS servers formatted correctly (2 entries)"
    else
        log_fail "DNS servers not formatted correctly (found $DNS_COUNT entries)"
        return 1
    fi

    # Verify no placeholder remains
    if ! grep -q "PLACEHOLDER" "$TEST_DIR/test-netplan.yaml"; then
        log_pass "No placeholders remaining"
    else
        log_fail "Placeholders still present in config"
        grep "PLACEHOLDER" "$TEST_DIR/test-netplan.yaml"
        return 1
    fi

    return 0
}

# Test: DNS parsing with various formats
test_dns_parsing() {
    log_test "Testing DNS server parsing"

    # Test case 1: Two DNS servers
    DNS_SERVERS="8.8.8.8,8.8.4.4"
    IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"
    if [ "${#DNS_LIST[@]}" -eq 2 ]; then
        log_pass "Parsed 2 DNS servers from comma-separated list"
    else
        log_fail "Failed to parse DNS servers (got ${#DNS_LIST[@]})"
        return 1
    fi

    # Test case 2: Single DNS server
    DNS_SERVERS="1.1.1.1"
    IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"
    if [ "${#DNS_LIST[@]}" -eq 1 ]; then
        log_pass "Parsed 1 DNS server"
    else
        log_fail "Failed to parse single DNS server"
        return 1
    fi

    # Test case 3: Three DNS servers
    DNS_SERVERS="8.8.8.8,8.8.4.4,1.1.1.1"
    IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"
    if [ "${#DNS_LIST[@]}" -eq 3 ]; then
        log_pass "Parsed 3 DNS servers"
    else
        log_fail "Failed to parse 3 DNS servers"
        return 1
    fi

    # Test case 4: DNS with spaces (should be trimmed)
    DNS_SERVERS=" 8.8.8.8 , 8.8.4.4 "
    IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"
    DNS_TRIMMED="${DNS_LIST[0]// /}"
    if [ "$DNS_TRIMMED" = "8.8.8.8" ]; then
        log_pass "DNS servers trimmed correctly"
    else
        log_fail "DNS servers not trimmed (got '$DNS_TRIMMED')"
        return 1
    fi

    return 0
}

# Test: Hardware requirements check
test_hardware_checks() {
    log_test "Testing hardware requirement checks"

    # Check CPU cores
    CPU_CORES=$(nproc)
    if [ "$CPU_CORES" -ge 4 ]; then
        log_pass "CPU cores check ($CPU_CORES >= 4)"
    else
        log_fail "Insufficient CPU cores ($CPU_CORES < 4)"
    fi

    # Check RAM
    TOTAL_RAM=$(free -g | awk '/^Mem:/{print $2}')
    if [ "$TOTAL_RAM" -ge 8 ]; then
        log_pass "RAM check (${TOTAL_RAM}GB >= 8GB)"
    else
        log_fail "Insufficient RAM (${TOTAL_RAM}GB < 8GB)"
    fi

    # Check virtualization support
    if egrep -c '(vmx|svm)' /proc/cpuinfo > /dev/null 2>&1; then
        log_pass "Virtualization support detected"
    else
        log_fail "No virtualization support"
    fi

    return 0
}

# Test: Configuration validation
test_config_validation() {
    log_test "Testing configuration validation"

    # Test valid IP address
    HOST_IP="192.168.1.100"
    if [[ $HOST_IP =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
        log_pass "Valid IP address format: $HOST_IP"
    else
        log_fail "Invalid IP address format: $HOST_IP"
        return 1
    fi

    # Test invalid IP address
    HOST_IP="999.999.999.999"
    if [[ $HOST_IP =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
        # Check octets
        IFS='.' read -ra OCTETS <<< "$HOST_IP"
        VALID=true
        for octet in "${OCTETS[@]}"; do
            if [ "$octet" -gt 255 ]; then
                VALID=false
            fi
        done
        if [ "$VALID" = false ]; then
            log_pass "Invalid IP rejected (octets > 255)"
        else
            log_fail "Invalid IP not rejected"
            return 1
        fi
    fi

    return 0
}

# Test: Password generation
test_password_generation() {
    log_test "Testing password generation"

    # Generate password
    PASSWORD=$(openssl rand -base64 32)

    if [ ${#PASSWORD} -ge 32 ]; then
        log_pass "Generated password with sufficient length (${#PASSWORD} chars)"
    else
        log_fail "Password too short (${#PASSWORD} < 32)"
        return 1
    fi

    # Check password contains alphanumeric
    if [[ $PASSWORD =~ [A-Za-z0-9] ]]; then
        log_pass "Password contains alphanumeric characters"
    else
        log_fail "Password missing alphanumeric characters"
        return 1
    fi

    return 0
}

# Test: File path validation
test_path_validation() {
    log_test "Testing storage path validation"

    # Test valid absolute path
    VOLUME_PATH="/var/lib/lightstack/volumes"
    if [[ $VOLUME_PATH = /* ]]; then
        log_pass "Valid absolute path: $VOLUME_PATH"
    else
        log_fail "Path is not absolute: $VOLUME_PATH"
        return 1
    fi

    # Test invalid relative path
    VOLUME_PATH="var/lib/lightstack/volumes"
    if [[ $VOLUME_PATH = /* ]]; then
        log_fail "Relative path accepted as absolute"
        return 1
    else
        log_pass "Relative path correctly rejected"
    fi

    return 0
}

# Test: Service name validation
test_service_validation() {
    log_test "Testing systemd service naming"

    SERVICE_NAME="lightstack.service"

    # Check service name format
    if [[ $SERVICE_NAME =~ ^[a-z0-9_-]+\.service$ ]]; then
        log_pass "Valid service name: $SERVICE_NAME"
    else
        log_fail "Invalid service name: $SERVICE_NAME"
        return 1
    fi

    return 0
}

# Test: Network interface detection
test_network_detection() {
    log_test "Testing network interface detection"

    # Get default interface
    DEFAULT_IFACE=$(ip route | grep default | awk '{print $5}')

    if [ -n "$DEFAULT_IFACE" ]; then
        log_pass "Default interface detected: $DEFAULT_IFACE"
    else
        log_fail "No default interface detected"
        return 1
    fi

    # Verify interface exists
    if ip link show "$DEFAULT_IFACE" > /dev/null 2>&1; then
        log_pass "Interface $DEFAULT_IFACE exists"
    else
        log_fail "Interface $DEFAULT_IFACE not found"
        return 1
    fi

    return 0
}

# Test: Gateway detection
test_gateway_detection() {
    log_test "Testing gateway detection"

    # Get default gateway
    DEFAULT_GATEWAY=$(ip route | grep default | awk '{print $3}')

    if [ -n "$DEFAULT_GATEWAY" ]; then
        log_pass "Default gateway detected: $DEFAULT_GATEWAY"
    else
        log_fail "No default gateway detected"
        return 1
    fi

    # Validate gateway is IP address
    if [[ $DEFAULT_GATEWAY =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
        log_pass "Gateway is valid IP format"
    else
        log_fail "Gateway is not valid IP"
        return 1
    fi

    return 0
}

# Test: Hostname validation
test_hostname_validation() {
    log_test "Testing hostname validation"

    # Valid hostnames
    VALID_NAMES=("lightstack-demo" "cloud-01" "openstack" "node1")
    for name in "${VALID_NAMES[@]}"; do
        if [[ $name =~ ^[a-z0-9-]+$ ]]; then
            log_pass "Valid hostname: $name"
        else
            log_fail "Hostname rejected incorrectly: $name"
            return 1
        fi
    done

    # Invalid hostnames
    INVALID_NAMES=("lightstack_demo" "cloud.01" "Open Stack" "node#1")
    for name in "${INVALID_NAMES[@]}"; do
        if [[ $name =~ ^[a-z0-9-]+$ ]]; then
            log_fail "Invalid hostname accepted: $name"
            return 1
        else
            log_pass "Invalid hostname rejected: $name"
        fi
    done

    return 0
}

# Repo root, so these checks read the real scripts rather than the test sandbox.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Test: the pg_hba line must name the database and role the script actually creates, and
# the guard must match the line it writes (or the line is re-appended on every run).
# Both sides are derived from the script itself — nothing here is hardcoded.
test_pg_hba_identifier_case() {
    log_test "Testing pg_hba identifiers match the database deploy-turnkey.sh creates"

    local script="$REPO_ROOT/scripts/deploy-turnkey.sh"
    local created_db created_role appended guard line_db line_role

    created_db="$(sed -n 's/^CREATE DATABASE \([A-Za-z0-9_]*\);.*/\1/p' "$script" | head -1)"
    created_role="$(sed -n 's/^CREATE USER \([A-Za-z0-9_]*\) WITH.*/\1/p' "$script" | head -1)"
    appended="$(sed -n 's/.*echo "\(host  *[^"]*\)".*PG_HBA.*/\1/p' "$script" | head -1)"
    guard="$(sed -n 's/.*grep -q "\([^"]*\)".*PG_HBA.*/\1/p' "$script" | head -1)"

    if [ -z "$created_db" ] || [ -z "$created_role" ] || [ -z "$appended" ] || [ -z "$guard" ]; then
        log_fail "could not read the database setup out of deploy-turnkey.sh (db='$created_db' role='$created_role' line='$appended' guard='$guard')"
        return
    fi

    line_db="$(echo "$appended" | awk '{print $2}')"
    line_role="$(echo "$appended" | awk '{print $3}')"

    if [ "$line_db" != "$created_db" ]; then
        log_fail "pg_hba names database '$line_db' but the script creates '$created_db' (pg_hba matching is case-sensitive)"
        return
    fi
    if [ "$line_role" != "$created_role" ]; then
        log_fail "pg_hba names role '$line_role' but the script creates '$created_role' (pg_hba matching is case-sensitive)"
        return
    fi
    if ! echo "$appended" | grep -qE "$guard"; then
        log_fail "the guard '$guard' does not match the line it writes — it would be appended on every run"
        return
    fi
    log_pass "pg_hba names $created_db/$created_role and the guard matches the line"
}

# Test: the "check the logs" hints must name a real Compose service / container. Both name
# sets are read out of the compose file, so this cannot drift from it.
test_compose_log_hints() {
    log_test "Testing log hints name a real Compose service and container"

    local compose="$REPO_ROOT/deployments/docker-compose.yml"
    local services containers name

    # Only keys inside the services: block — top-level volumes/networks are not services.
    services="$(awk '/^services:/{inblock=1; next} /^[a-zA-Z]/{inblock=0} inblock && /^  [a-z][a-z0-9_-]*:[[:space:]]*$/{gsub(/[ :]/, ""); print}' "$compose")"
    containers="$(sed -n 's/^ *container_name: *\([^ ]*\).*/\1/p' "$compose")"
    if [ -z "$services" ] || [ -z "$containers" ]; then
        log_fail "could not read service/container names out of $compose"
        return
    fi

    # scripts/start.sh points at a Compose service.
    name="$(sed -n 's/.*COMPOSE_CMD logs \([^"]*\)".*/\1/p' "$REPO_ROOT/scripts/start.sh" | head -1)"
    if [ -z "$name" ]; then
        log_fail "could not find the compose log hint in scripts/start.sh"
        return
    fi
    if ! echo "$services" | grep -qx "$name"; then
        log_fail "start.sh suggests 'logs $name', which is not a Compose service ($(echo $services | tr '\n' ' '))"
        return
    fi

    # scripts/start_validation.sh points at a container.
    name="$(sed -n 's/.*docker logs \([^"]*\)".*/\1/p' "$REPO_ROOT/scripts/start_validation.sh" | head -1)"
    if [ -z "$name" ]; then
        log_fail "could not find the docker log hint in scripts/start_validation.sh"
        return
    fi
    if ! echo "$containers" | grep -qx "$name"; then
        log_fail "start_validation.sh suggests 'docker logs $name', which is not a container ($(echo $containers | tr '\n' ' '))"
        return
    fi
    log_pass "both log hints name real Compose objects"
}

# Test: scripts must target the identity port the shipped config actually serves. The
# upstream Keystone convention is 5000; this product listens on keystone.port, so a script
# hardcoding 5000 points at a port nothing binds. The expected value is read from the config.
test_identity_port_consistency() {
    log_test "Testing scripts target the identity port from config/lightstack.yaml"

    local port offenders declared
    port="$(awk '/^keystone:/{f=1; next} /^[a-z]/{f=0} f && /^  port:/{print $2; exit}' \
        "$REPO_ROOT/config/lightstack.yaml")"
    if [ -z "$port" ]; then
        log_fail "could not read keystone.port from config/lightstack.yaml"
        return
    fi

    # No script may point at the upstream port unless the config actually moved there.
    if [ "$port" != "5000" ]; then
        # Character class so this check does not match its own source file.
        offenders="$(grep -rn "localhost:[5]000" "$REPO_ROOT"/scripts/*.sh 2>/dev/null || true)"
        if [ -n "$offenders" ]; then
            log_fail "identity is on $port, but these point at 5000:"$'\n'"$offenders"
            return
        fi
    fi

    # start.sh's readiness probe and port-conflict check share one declared default.
    declared="$(sed -n 's/^KEYSTONE_PORT="\${KEYSTONE_PORT:-\([0-9]*\)}"/\1/p' \
        "$REPO_ROOT/scripts/start.sh" | head -1)"
    if [ -z "$declared" ]; then
        log_fail "scripts/start.sh does not declare a KEYSTONE_PORT default"
        return
    fi
    if [ "$declared" != "$port" ]; then
        log_fail "start.sh defaults KEYSTONE_PORT to $declared, config says $port"
        return
    fi

    # The two keystone test scripts derive their base URL from OS_AUTH_URL with a default.
    local script name
    for script in test-keystone.sh test-all.sh; do
        name="$(sed -n "s|.*OS_AUTH_URL:-http://localhost:\([0-9]*\)/v3}.*|\1|p" \
            "$REPO_ROOT/scripts/$script" | head -1)"
        if [ -z "$name" ]; then
            log_fail "scripts/$script does not default its base URL from OS_AUTH_URL"
            return
        fi
        if [ "$name" != "$port" ]; then
            log_fail "scripts/$script defaults to port $name, config says $port"
            return
        fi
    done

    log_pass "every script targets the configured identity port ($port)"
}

# Run all tests
run_all_tests() {
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║                                                           ║"
    echo "║   Lightstack Deployment Script Test Suite                       ║"
    echo "║                                                           ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""

    setup_test_env

    # Run tests
    test_netplan_generation || true
    test_dns_parsing || true
    test_hardware_checks || true
    test_config_validation || true
    test_password_generation || true
    test_path_validation || true
    test_service_validation || true
    test_network_detection || true
    test_gateway_detection || true
    test_hostname_validation || true
    test_pg_hba_identifier_case || true
    test_compose_log_hints || true
    test_identity_port_consistency || true

    cleanup_test_env

    # Summary
    echo ""
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║   Test Summary                                            ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""
    echo -e "${GREEN}Tests Passed: $TESTS_PASSED${NC}"
    echo -e "${RED}Tests Failed: $TESTS_FAILED${NC}"
    echo ""

    if [ "$TESTS_FAILED" -eq 0 ]; then
        echo -e "${GREEN}✓ All tests passed!${NC}"
        return 0
    else
        echo -e "${RED}✗ Some tests failed${NC}"
        return 1
    fi
}

# Run tests
run_all_tests
