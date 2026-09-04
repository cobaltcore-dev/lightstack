#!/bin/bash
# Lightstack Validation Startup Script
# Run this script once Docker/OrbStack is running

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${BLUE}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║           Lightstack Real-World Validation - Startup                 ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""

# Check Docker is running
echo -e "${YELLOW}[1/5]${NC} Checking Docker daemon..."
if ! docker ps &> /dev/null; then
    echo -e "${RED}✗ Docker is not running${NC}"
    echo ""
    echo "Please start Docker/OrbStack and run this script again"
    exit 1
fi
echo -e "${GREEN}✓ Docker is running${NC}"
echo ""

# Start Lightstack + Horizon stack
echo -e "${YELLOW}[2/5]${NC} Starting Lightstack + Horizon stack..."
export LIGHTSTACK_JWT_SECRET="${LIGHTSTACK_JWT_SECRET:-validation-only-secret}"
docker compose -f deployments/docker-compose-horizon.yml down &> /dev/null || true
docker compose -f deployments/docker-compose-horizon.yml up -d --build
echo -e "${GREEN}✓ Lightstack services started${NC}"
echo ""

# Wait for Lightstack to be ready
echo -e "${YELLOW}[3/5]${NC} Waiting for Lightstack to be healthy..."
for i in {1..30}; do
    if curl -s -f http://localhost:35357/v3 &> /dev/null; then
        echo -e "${GREEN}✓ Lightstack is healthy${NC}"
        break
    fi
    if [ $i -eq 30 ]; then
        echo -e "${RED}✗ Lightstack failed to start${NC}"
        echo "Check logs with: docker logs lightstack"
        exit 1
    fi
    sleep 2
    echo -n "."
done
echo ""

# Run validation suite
echo -e "${YELLOW}[4/5]${NC} Running CLI validation suite..."
echo ""
./test/validation_suite.sh | tee validation_results_$(date +%Y%m%d_%H%M%S).txt
VALIDATION_EXIT=$?
echo ""

# Wait for Horizon (started with the stack in step 2)
echo -e "${YELLOW}[5/5]${NC} Waiting for Horizon to be healthy..."
for i in {1..30}; do
    if curl -s -f http://localhost/dashboard/auth/login/ &> /dev/null; then
        echo -e "${GREEN}✓ Horizon is healthy${NC}"
        break
    fi
    if [ $i -eq 30 ]; then
        echo -e "${YELLOW}⚠ Horizon may not be ready yet${NC}"
        echo "Check logs with: docker logs lightstack-horizon"
    fi
    sleep 2
    echo -n "."
done
echo ""

# Summary
echo -e "${BLUE}╔══════════════════════════════════════════════════════════════╗${NC}"
echo -e "${BLUE}║                    VALIDATION READY                           ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "✓ Lightstack Services:     ${GREEN}http://localhost:35357/v3${NC} (Keystone)"
echo -e "                    http://localhost:8774/v2.1 (Nova)"
echo -e "                    http://localhost:9696/v2.0 (Neutron)"
echo -e "                    http://localhost:8776/v3 (Cinder)"
echo -e "                    http://localhost:9292/v2 (Glance)"
echo ""
echo -e "✓ Horizon UI:       ${GREEN}http://localhost${NC}"
echo -e "  Login:            admin / secret"
echo ""
echo -e "✓ Validation Log:   validation_results_*.txt"
echo ""

if [ $VALIDATION_EXIT -eq 0 ]; then
    echo -e "${GREEN}✓ CLI Validation: ALL TESTS PASSED${NC}"
    echo ""
    echo -e "${BLUE}Next Steps:${NC}"
    echo "1. Open Horizon in browser: http://localhost"
    echo "2. Login with: admin / secret"
    echo "3. Test these workflows:"
    echo "   - Launch an instance"
    echo "   - Create a network"
    echo "   - Create a volume"
    echo "   - Upload an image"
    echo "4. Document any errors (check browser console F12)"
else
    echo -e "${YELLOW}⚠ CLI Validation: SOME TESTS FAILED${NC}"
    echo ""
    echo -e "${BLUE}Next Steps:${NC}"
    echo "1. Review validation_results_*.txt"
    echo "2. Identify missing APIs"
    echo "3. Prioritize by impact"
    echo "4. Open Horizon (http://localhost) to discover UI-specific gaps"
fi
echo ""
echo -e "${BLUE}To stop services:${NC}"
echo "  docker compose -f deployments/docker-compose-horizon.yml down"
echo ""
