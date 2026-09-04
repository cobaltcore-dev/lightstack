#!/bin/bash
# Quick startup script for Lightstack Docker Compose deployment

set -e

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

echo "=========================================="
echo "  Lightstack Docker Compose Startup"
echo "=========================================="
echo ""

# Check Docker
echo -e "${YELLOW}[1/5]${NC} Checking Docker..."
if ! command -v docker &> /dev/null; then
    echo -e "${RED}✗${NC} Docker is not installed"
    echo "Please install Docker from https://www.docker.com/get-started"
    exit 1
fi

if ! docker info &> /dev/null; then
    echo -e "${RED}✗${NC} Docker is not running"
    echo "Please start Docker Desktop"
    exit 1
fi

echo -e "${GREEN}✓${NC} Docker is running"

# Check Docker Compose
echo -e "${YELLOW}[2/5]${NC} Checking Docker Compose..."
if docker compose version &> /dev/null; then
    echo -e "${GREEN}✓${NC} Docker Compose is installed"
elif command -v docker-compose &> /dev/null; then
    echo -e "${GREEN}✓${NC} Docker Compose is installed (legacy)"
    COMPOSE_CMD="docker-compose"
else
    echo -e "${RED}✗${NC} Docker Compose is not installed"
    exit 1
fi

# Use appropriate compose command
COMPOSE_CMD="${COMPOSE_CMD:-docker compose}"

# Identity listens on keystone.port (35357), not the upstream Keystone convention of
# 5000. One declaration for the conflict check, the readiness probe and the URLs printed
# at the end, so they cannot drift apart again.
KEYSTONE_PORT="${KEYSTONE_PORT:-35357}"
IDENTITY_URL="http://localhost:${KEYSTONE_PORT}/v3"

# Check for existing services
echo -e "${YELLOW}[3/5]${NC} Checking for conflicts..."
if lsof -i:"$KEYSTONE_PORT" &> /dev/null; then
    echo -e "${RED}✗${NC} Port $KEYSTONE_PORT is already in use"
    echo "Please stop any service using port $KEYSTONE_PORT"
    lsof -i:"$KEYSTONE_PORT"
    exit 1
fi

echo -e "${GREEN}✓${NC} Ports are available"

# Start services
echo -e "${YELLOW}[4/5]${NC} Starting services..."
echo "This may take 2-3 minutes on first run (downloading images, building Lightstack)..."

cd "$(dirname "$0")"
$COMPOSE_CMD up -d --build

# Wait for services to be healthy
echo ""
echo -e "${YELLOW}[5/5]${NC} Waiting for services to be ready..."
echo "This may take 30-60 seconds..."

MAX_ATTEMPTS=60
ATTEMPT=0

while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
    # Check Lightstack health
    if curl -sf "$IDENTITY_URL" > /dev/null 2>&1; then
        echo -e "\n${GREEN}✓${NC} Lightstack is ready!"
        break
    fi
    ATTEMPT=$((ATTEMPT + 1))
    echo -n "."
    sleep 2
done

if [ $ATTEMPT -eq $MAX_ATTEMPTS ]; then
    echo ""
    echo -e "${RED}✗${NC} Lightstack did not start in time"
    echo ""
    echo "Check logs with:"
    echo "  $COMPOSE_CMD logs lightstack"
    exit 1
fi

# Check Horizon (may take longer)
echo ""
echo "Waiting for Horizon Dashboard..."
ATTEMPT=0
while [ $ATTEMPT -lt $MAX_ATTEMPTS ]; do
    if curl -sf http://localhost/dashboard/ > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC} Horizon is ready!"
        break
    fi
    ATTEMPT=$((ATTEMPT + 1))
    echo -n "."
    sleep 2
done

if [ $ATTEMPT -eq $MAX_ATTEMPTS ]; then
    echo ""
    echo -e "${YELLOW}⚠${NC}  Horizon did not start (may not work on ARM Mac)"
    echo ""
    echo "You can still use the OpenStack CLI:"
    echo "  pipx install python-openstackclient"
    echo "  export OS_AUTH_URL=$IDENTITY_URL"
    echo "  export OS_USERNAME=admin"
    echo "  export OS_PASSWORD=secret"
    echo "  export OS_PROJECT_NAME=default"
    echo "  export OS_PROJECT_DOMAIN_NAME=Default"
    echo "  export OS_USER_DOMAIN_NAME=Default"
    echo "  export OS_IDENTITY_API_VERSION=3"
    echo ""
fi

echo ""
echo "=========================================="
echo "  Lightstack is Ready!"
echo "=========================================="
echo ""
echo -e "${GREEN}Services Status:${NC}"
$COMPOSE_CMD ps
echo ""
echo -e "${GREEN}Access Points:${NC}"
echo "  Web UI:       http://localhost/dashboard"
echo "  API (Auth):   $IDENTITY_URL"
echo "  API (Compute) http://localhost:8774/v2.1"
echo "  API (Network) http://localhost:9696/v2.0"
echo ""
echo -e "${GREEN}Login Credentials:${NC}"
echo "  Domain:   Default"
echo "  Username: admin"
echo "  Password: secret"
echo ""
echo -e "${GREEN}Quick Commands:${NC}"
echo "  View logs:     $COMPOSE_CMD logs -f"
echo "  Stop:          $COMPOSE_CMD down"
echo "  Restart:       $COMPOSE_CMD restart"
echo "  Rebuild:       $COMPOSE_CMD up -d --build"
echo ""
echo -e "${GREEN}OpenStack CLI Setup:${NC}"
echo "  source /tmp/lightstack-env.sh"
echo "  openstack server list"
echo ""

# Try to open browser (works on macOS and Linux)
if [ -n "$HORIZON_READY" ]; then
    echo "Opening browser..."
    if command -v open &> /dev/null; then
        open "http://localhost/dashboard"
    elif command -v xdg-open &> /dev/null; then
        xdg-open "http://localhost/dashboard"
    fi
fi

echo "Happy cloud computing! 🚀"
