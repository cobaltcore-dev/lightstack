#!/bin/bash
#
# Lightstack Single-Node Deployment Script
# Interactively configures and deploys Lightstack with KVM hypervisor on a single host
#
# Usage: sudo ./scripts/deploy-single-node.sh
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Banner
show_banner() {
    cat << "EOF"
╔═══════════════════════════════════════════════════════════╗
║                                                           ║
║   lightstack Single-Node Deployment Script                      ║
║   Deploy lightstack + KVM Hypervisor on Single Host            ║
║                                                           ║
╚═══════════════════════════════════════════════════════════╝
EOF
    echo ""
}

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi
}

# Detect OS
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS=$ID
        OS_VERSION=$VERSION_ID
    else
        log_error "Cannot detect OS. /etc/os-release not found."
        exit 1
    fi

    log_info "Detected OS: $OS $OS_VERSION"

    # Check if supported
    case "$OS" in
        ubuntu)
            if [ "$OS_VERSION" != "24.04" ] && [ "$OS_VERSION" != "22.04" ]; then
                log_warning "OS version $OS_VERSION not officially tested. Recommended: Ubuntu 24.04 LTS"
            fi
            ;;
        debian)
            if [ "$OS_VERSION" != "12" ]; then
                log_warning "OS version $OS_VERSION not officially tested. Recommended: Debian 12"
            fi
            ;;
        *)
            log_error "Unsupported OS: $OS. Supported: Ubuntu 24.04/22.04, Debian 12"
            exit 1
            ;;
    esac
}

# Check hardware requirements
check_hardware() {
    log_info "Checking hardware requirements..."

    # Check CPU cores
    CPU_CORES=$(nproc)
    if [ "$CPU_CORES" -lt 4 ]; then
        log_error "Insufficient CPU cores: $CPU_CORES (minimum: 4)"
        exit 1
    fi
    log_success "CPU cores: $CPU_CORES ✓"

    # Check RAM (in GB)
    TOTAL_RAM=$(free -g | awk '/^Mem:/{print $2}')
    if [ "$TOTAL_RAM" -lt 16 ]; then
        log_error "Insufficient RAM: ${TOTAL_RAM}GB (minimum: 16GB)"
        exit 1
    fi
    log_success "RAM: ${TOTAL_RAM}GB ✓"

    # Check disk space (in GB)
    DISK_SPACE=$(df -BG / | awk 'NR==2{print $4}' | sed 's/G//')
    if [ "$DISK_SPACE" -lt 100 ]; then
        log_error "Insufficient disk space: ${DISK_SPACE}GB (minimum: 100GB)"
        exit 1
    fi
    log_success "Disk space: ${DISK_SPACE}GB ✓"

    # Check virtualization support
    if ! egrep -c '(vmx|svm)' /proc/cpuinfo > /dev/null; then
        log_error "CPU does not support virtualization (VT-x/AMD-V)"
        exit 1
    fi
    log_success "Virtualization support: enabled ✓"
}

# Interactive configuration
get_configuration() {
    log_info "Starting interactive configuration..."
    echo ""

    # Hostname
    read -p "Enter hostname for this node [lightstack-demo]: " HOSTNAME
    HOSTNAME=${HOSTNAME:-lightstack-demo}

    # Show available network interfaces
    echo ""
    log_info "Available network interfaces:"
    ip -brief link show | grep -v "lo" | awk '{printf "  - %s (%s)\n", $1, $3}'
    echo ""

    # Host IP address
    DEFAULT_IP=$(ip route get 8.8.8.8 2>/dev/null | awk '{print $7; exit}')
    read -p "Enter host IP address [$DEFAULT_IP]: " HOST_IP
    HOST_IP=${HOST_IP:-$DEFAULT_IP}

    # Network interface for bridge
    DEFAULT_IFACE=$(ip route | grep default | awk '{print $5}')
    echo ""
    log_info "The selected interface will be added to bridge 'br-ext'"
    log_info "The bridge will get IP $HOST_IP for host management access"
    log_info "VMs will be connected to this bridge for external network access"
    echo ""
    read -p "Enter network interface to bridge (for VM traffic) [$DEFAULT_IFACE]: " NETWORK_IFACE
    NETWORK_IFACE=${NETWORK_IFACE:-$DEFAULT_IFACE}

    # Gateway
    DEFAULT_GATEWAY=$(ip route | grep default | awk '{print $3}')
    read -p "Enter network gateway [$DEFAULT_GATEWAY]: " GATEWAY
    GATEWAY=${GATEWAY:-$DEFAULT_GATEWAY}

    # DNS servers
    read -p "Enter DNS servers [8.8.8.8,8.8.4.4]: " DNS_SERVERS
    DNS_SERVERS=${DNS_SERVERS:-"8.8.8.8,8.8.4.4"}

    # Database password
    read -sp "Enter PostgreSQL password for Lightstack database: " DB_PASSWORD
    echo ""
    if [ -z "$DB_PASSWORD" ]; then
        DB_PASSWORD=$(openssl rand -base64 32)
        log_warning "No password provided. Generated random password: $DB_PASSWORD"
    fi

    # JWT secret
    JWT_SECRET=$(openssl rand -base64 32)
    log_info "Generated JWT secret for Keystone"

    # Node-local state root: images, volumes, instances, cloud-init, dhcp and console
    # tokens all live under it (the binary's state_path, which refuses anything else).
    read -p "Enter node-local state root [/var/lib/lightstack]: " STORAGE_DIR
    STORAGE_DIR=${STORAGE_DIR:-/var/lib/lightstack}
    case "$STORAGE_DIR" in
        /?*) ;;
        *) log_error "State root must be an absolute path other than /: $STORAGE_DIR"; exit 1 ;;
    esac

    # Horizon deployment
    read -p "Deploy Horizon dashboard? [Y/n]: " DEPLOY_HORIZON
    DEPLOY_HORIZON=${DEPLOY_HORIZON:-Y}

    # Confirmation
    echo ""
    log_info "Configuration Summary:"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  Hostname:           $HOSTNAME"
    echo "  Host IP:            $HOST_IP (assigned to bridge br-ext)"
    echo "  Bridge Interface:   $NETWORK_IFACE (member of br-ext for VM traffic)"
    echo "  Gateway:            $GATEWAY"
    echo "  DNS Servers:        $DNS_SERVERS"
    echo "  State Root:         $STORAGE_DIR"
    echo "  Deploy Horizon:     $DEPLOY_HORIZON"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""

    read -p "Proceed with installation? [Y/n]: " CONFIRM
    CONFIRM=${CONFIRM:-Y}

    if [[ ! "$CONFIRM" =~ ^[Yy]$ ]]; then
        log_error "Installation cancelled by user"
        exit 1
    fi
}

# Update system
update_system() {
    log_info "Updating system packages..."
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get upgrade -y -qq
    log_success "System updated"
}

# Set hostname
configure_hostname() {
    log_info "Configuring hostname: $HOSTNAME"
    hostnamectl set-hostname "$HOSTNAME"

    # Update /etc/hosts
    if ! grep -q "127.0.0.1.*$HOSTNAME" /etc/hosts; then
        echo "127.0.0.1 $HOSTNAME" >> /etc/hosts
    fi

    if ! grep -q "$HOST_IP.*$HOSTNAME" /etc/hosts; then
        echo "$HOST_IP $HOSTNAME" >> /etc/hosts
    fi

    log_success "Hostname configured"
}

# Install KVM and libvirt
install_kvm() {
    log_info "Installing KVM and libvirt..."

    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
        qemu-kvm \
        libvirt-daemon-system \
        libvirt-clients \
        bridge-utils \
        virt-manager \
        cpu-checker \
        > /dev/null

    # Verify KVM
    if kvm-ok | grep -q "KVM acceleration can be used"; then
        log_success "KVM installed and acceleration available"
    else
        log_error "KVM acceleration not available. Check BIOS virtualization settings."
        exit 1
    fi

    # Start libvirt
    systemctl enable --now libvirtd
    log_success "libvirt enabled and started"
}

# Configure networking
configure_networking() {
    log_info "Configuring network bridge..."

    # Backup existing netplan config
    mkdir -p /etc/netplan/backup
    cp /etc/netplan/*.yaml /etc/netplan/backup/ 2>/dev/null || true

    # Parse DNS servers into array
    IFS=',' read -ra DNS_LIST <<< "$DNS_SERVERS"

    # Create bridge configuration with proper YAML formatting
    cat > /etc/netplan/01-lightstack-bridge.yaml <<'NETPLAN_EOF'
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

    # Replace placeholders with actual values (using temp file for portability)
    sed "s/NETWORK_IFACE_PLACEHOLDER/$NETWORK_IFACE/g" /etc/netplan/01-lightstack-bridge.yaml > /etc/netplan/01-lightstack-bridge.yaml.tmp
    mv /etc/netplan/01-lightstack-bridge.yaml.tmp /etc/netplan/01-lightstack-bridge.yaml

    sed "s|HOST_IP_PLACEHOLDER|$HOST_IP|g" /etc/netplan/01-lightstack-bridge.yaml > /etc/netplan/01-lightstack-bridge.yaml.tmp
    mv /etc/netplan/01-lightstack-bridge.yaml.tmp /etc/netplan/01-lightstack-bridge.yaml

    sed "s/GATEWAY_PLACEHOLDER/$GATEWAY/g" /etc/netplan/01-lightstack-bridge.yaml > /etc/netplan/01-lightstack-bridge.yaml.tmp
    mv /etc/netplan/01-lightstack-bridge.yaml.tmp /etc/netplan/01-lightstack-bridge.yaml

    # Insert DNS servers with proper indentation
    # Build the DNS lines in a temporary file to avoid sed escaping issues
    DNS_TEMP=$(mktemp)
    for dns in "${DNS_LIST[@]}"; do
        echo "          - ${dns}" >> "$DNS_TEMP"
    done

    # Replace DNS_SERVERS_PLACEHOLDER with actual DNS entries
    # Use awk to avoid sed escaping issues with hyphens
    awk -v dns_file="$DNS_TEMP" '
        /DNS_SERVERS_PLACEHOLDER/ {
            while ((getline line < dns_file) > 0) {
                print line
            }
            close(dns_file)
            next
        }
        {print}
    ' /etc/netplan/01-lightstack-bridge.yaml > /etc/netplan/01-lightstack-bridge.yaml.tmp
    mv /etc/netplan/01-lightstack-bridge.yaml.tmp /etc/netplan/01-lightstack-bridge.yaml
    rm -f "$DNS_TEMP"

    # Set proper permissions
    chmod 600 /etc/netplan/01-lightstack-bridge.yaml

    # Validate netplan configuration
    log_info "Validating netplan configuration..."
    if ! netplan generate 2>/tmp/netplan-error.log; then
        log_error "Invalid netplan configuration:"
        cat /tmp/netplan-error.log
        log_error "Restoring previous configuration..."
        rm /etc/netplan/01-lightstack-bridge.yaml
        cp /etc/netplan/backup/*.yaml /etc/netplan/ 2>/dev/null || true
        exit 1
    fi

    # Show generated config for verification
    log_info "Generated netplan configuration:"
    cat /etc/netplan/01-lightstack-bridge.yaml

    # Apply netplan
    log_warning "Applying network configuration. SSH connection may be interrupted briefly..."
    if ! netplan apply 2>/tmp/netplan-apply.log; then
        log_error "Failed to apply netplan configuration:"
        cat /tmp/netplan-apply.log
        log_error "Restoring previous configuration..."
        rm /etc/netplan/01-lightstack-bridge.yaml
        cp /etc/netplan/backup/*.yaml /etc/netplan/
        netplan apply
        exit 1
    fi

    # Verify bridge
    sleep 5
    if ip addr show br-ext > /dev/null 2>&1; then
        log_success "Bridge br-ext created successfully"
    else
        log_error "Failed to create bridge br-ext"
        log_error "Restoring previous configuration..."
        rm /etc/netplan/01-lightstack-bridge.yaml
        cp /etc/netplan/backup/*.yaml /etc/netplan/
        netplan apply
        exit 1
    fi

    # Enable IP forwarding and bridge netfilter
    log_info "Enabling IP forwarding and bridge netfilter..."

    # Load bridge netfilter module and persist at boot
    if modprobe br_netfilter 2>/dev/null; then
        echo "br_netfilter" > /etc/modules-load.d/lightstack.conf
        log_success "Bridge netfilter module loaded"
    else
        log_warning "Could not load br_netfilter module (may not be needed)"
    fi

    # Add sysctl configuration
    cat >> /etc/sysctl.conf <<EOF

# Lightstack networking
net.ipv4.ip_forward=1
net.ipv4.conf.all.forwarding=1
net.ipv6.conf.all.forwarding=1
EOF

    # Add bridge netfilter settings only if module loaded
    if [ -f /proc/sys/net/bridge/bridge-nf-call-iptables ]; then
        cat >> /etc/sysctl.conf <<EOF
net.bridge.bridge-nf-call-iptables=1
net.bridge.bridge-nf-call-ip6tables=1
EOF
    fi

    # Apply sysctl settings (ignore errors for bridge settings if module not loaded)
    sysctl -p 2>&1 | grep -v "cannot stat /proc/sys/net/bridge" || true
    log_success "IP forwarding enabled"
}

# Install PostgreSQL
install_postgresql() {
    log_info "Installing PostgreSQL 18..."

    # Add PostgreSQL repository
    apt-get install -y -qq curl ca-certificates > /dev/null
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/postgresql-keyring.gpg
    echo "deb [signed-by=/usr/share/keyrings/postgresql-keyring.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" > /etc/apt/sources.list.d/pgdg.list

    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql-18 postgresql-client-18 > /dev/null

    # Start PostgreSQL
    systemctl enable --now postgresql

    # Create database and user
    sudo -u postgres psql <<EOF
CREATE DATABASE lightstack;
CREATE USER lightstack WITH ENCRYPTED PASSWORD '$DB_PASSWORD';
GRANT ALL PRIVILEGES ON DATABASE lightstack TO lightstack;
ALTER DATABASE lightstack OWNER TO lightstack;
EOF

    log_success "PostgreSQL installed and configured"
}

# Install Docker and Docker Compose
install_docker() {
    log_info "Installing Docker and Docker Compose..."

    # Install Docker
    curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
    sh /tmp/get-docker.sh > /dev/null
    rm /tmp/get-docker.sh

    # Install Docker Compose plugin
    apt-get install -y -qq docker-compose-plugin > /dev/null

    systemctl enable --now docker

    log_success "Docker and Docker Compose installed"
}

# Clone Lightstack repository
clone_lightstack() {
    log_info "Cloning Lightstack repository..."

    apt-get install -y -qq git > /dev/null

    if [ -d /opt/lightstack ]; then
        log_warning "/opt/lightstack already exists. Pulling latest changes..."
        cd /opt/lightstack
        git pull origin main
    else
        git clone https://github.com/cobaltcore-dev/lightstack.git /opt/lightstack
    fi

    log_success "Lightstack repository ready"
}

# Build Lightstack
build_lightstack() {
    log_info "Building Lightstack binary..."

    # Use the same patched toolchain as CI and the release image.
    local go_version="1.27.3"
    local go_sha256="65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"
    local installed_go=""
    if command -v go &> /dev/null; then
        installed_go=$(go env GOVERSION 2>/dev/null || true)
    fi
    if [ "$installed_go" != "go${go_version}" ]; then
        log_info "Installing Go ${go_version}..."
        log_info "Downloading Go (this may take a few minutes)..."
        wget --progress=bar:force "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz" -O /tmp/go.tar.gz 2>&1 | \
            grep --line-buffered "%" | sed -u 's/.*\([0-9]\+%\).*$/\1/' | \
            while IFS= read -r line; do echo -ne "\r  Progress: $line"; done
        echo ""
        echo "${go_sha256}  /tmp/go.tar.gz" | sha256sum --check --status || {
            log_error "Go archive checksum verification failed"
            exit 1
        }

        log_info "Extracting Go..."
        rm -rf /usr/local/go
        tar -C /usr/local -xzf /tmp/go.tar.gz
        rm /tmp/go.tar.gz
        export PATH=/usr/local/go/bin:$PATH
        echo 'export PATH=/usr/local/go/bin:$PATH' >> /etc/profile
        log_success "Go ${go_version} installed"
    fi

    log_info "Building Lightstack binary (this may take 2-3 minutes)..."
    cd /opt/lightstack

    # Show build progress
    if make build 2>&1 | grep -E "(Building|Compiling|go build)" | head -10; then
        log_success "Lightstack binary built: /opt/lightstack/bin/lightstack"
    else
        # If grep filters everything, just run silently
        make build > /tmp/lightstack-build.log 2>&1
        if [ -f /opt/lightstack/bin/lightstack ]; then
            log_success "Lightstack binary built: /opt/lightstack/bin/lightstack"
        else
            log_error "Build failed. Check /tmp/lightstack-build.log"
            cat /tmp/lightstack-build.log
            exit 1
        fi
    fi
}

# Create Lightstack configuration
create_config() {
    log_info "Creating Lightstack configuration..."

    mkdir -p /opt/lightstack/config

    cat > /opt/lightstack/config/lightstack.yaml <<EOF
# Lightstack Single-Node Configuration
# Generated by deploy-single-node.sh on $(date)

# Node-local state root. libvirtd opens the paths Lightstack writes into the domain XML,
# so this is also where the host keeps the guests' disks and cloud-init ISOs.
state_path: "$STORAGE_DIR"

database:
  url: "postgres://lightstack:$DB_PASSWORD@localhost:5432/lightstack?sslmode=disable"
  max_connections: 20

keystone:
  host: "0.0.0.0"
  port: 35357
  jwt_secret: "$JWT_SECRET"
  token_ttl: 24h

nova:
  host: "0.0.0.0"
  port: 8774
  libvirt_mode: real
  libvirt_uri: "qemu:///system"
  console_proxy_base_url: "http://$HOST_IP:6080"   # origin only, no path

neutron:
  host: "0.0.0.0"
  port: 9696
  networking_mode: iptables
  external_bridge: "br-ext"
  vxlan_enabled: false

cinder:
  host: "0.0.0.0"
  port: 8776
  storage_mode: local

glance:
  host: "0.0.0.0"
  port: 9292
  storage_mode: local

metadata:
  host: "0.0.0.0"
  port: 8775

compute:
  node_id: "compute-node-1"
  node_name: "$HOSTNAME"
  tunnel_ip: "$HOST_IP"
EOF

    chmod 600 /opt/lightstack/config/lightstack.yaml
    log_success "Configuration created: /opt/lightstack/config/lightstack.yaml"
}

# Create storage directories
create_storage_dirs() {
    log_info "Creating storage directories..."

    # The subdirs the chart's init-host-storage initContainer creates: not every service
    # creates its own, and one that finds its subdir missing fails.
    mkdir -p "$STORAGE_DIR"/{images,volumes,instances,cloud-init,dhcp,console-tokens,console-sockets}
    chmod 755 "$STORAGE_DIR"/{images,volumes,instances,cloud-init,dhcp,console-tokens}

    log_success "Storage directories created"
}

# Run database migrations
run_migrations() {
    log_info "Running database migrations..."

    cd /opt/lightstack
    ./bin/lightstack migrate --config config/lightstack.yaml

    log_success "Database migrations completed"
}

# Create systemd service
create_systemd_service() {
    log_info "Creating systemd service..."

    cat > /etc/systemd/system/lightstack.service <<EOF
[Unit]
Description=lightstack OpenStack Services
After=network.target postgresql.service libvirtd.service

[Service]
Type=simple
User=root
WorkingDirectory=/opt/lightstack
ExecStart=/opt/lightstack/bin/lightstack --config /opt/lightstack/config/lightstack.yaml
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable lightstack.service
    systemctl start lightstack.service

    # Wait for services to start
    sleep 5

    if systemctl is-active --quiet lightstack.service; then
        log_success "Lightstack service started successfully"
    else
        log_error "Lightstack service failed to start. Check logs: journalctl -u lightstack.service -n 50"
        exit 1
    fi
}

# Deploy Horizon dashboard
deploy_horizon() {
    if [[ ! "$DEPLOY_HORIZON" =~ ^[Yy]$ ]]; then
        log_info "Skipping Horizon deployment (user choice)"
        return
    fi

    log_info "Deploying Horizon dashboard..."

    mkdir -p /opt/horizon

    # Create docker-compose.yml
    cat > /opt/horizon/docker-compose.yml <<EOF
version: '3.8'

services:
  horizon:
    image: quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble
    container_name: horizon
    restart: unless-stopped
    ports:
      - "80:80"
    environment:
      - OPENSTACK_HOST=$HOST_IP
      - OPENSTACK_KEYSTONE_URL=http://$HOST_IP:35357/v3
      - OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT=True
      - OPENSTACK_KEYSTONE_DEFAULT_DOMAIN=Default
    volumes:
      - ./local_settings.py:/etc/openstack-dashboard/local_settings.py:ro
    networks:
      - lightstack-net

  # noVNC console proxy (SPEC-021) -- the recipe of charts/lightstack/templates/console-proxy.yaml.
  # websockify serves the noVNC client and resolves each ?token= against the entries Lightstack
  # writes to $STORAGE_DIR/console-tokens; each entry names the guest's VNC unix socket.
  # Not kolla's nova-novncproxy: it validates tokens against Nova's own database, which
  # Lightstack does not have. Host networking so it answers on the host's own address.
  #   entrypoint, not command: the image's default runs novnc_proxy against ONE fixed target,
  #     and compose's command replaces only its arguments
  #   TokenFile, not ReadOnlyTokenFile: the read-only plugin caches, and would keep serving
  #     revoked tokens
  #   no --host-token: it would let the client pick the target instead of the token
  #   pinned to 1.3.0's digest, as the chart is: the tag is mutable (digest only, because
  #     Podman rejects tag@digest)
  novnc:
    image: bonigarcia/novnc@sha256:a5be468dc8967a55ffe870e809fa36dbbfc19b67d8156c1e7cb2d26aaa4f32c2
    container_name: novnc-proxy
    restart: unless-stopped
    network_mode: host
    entrypoint:
      - /bin/sh
      - -c
      - >-
        cd /root/noVNC/utils/websockify &&
        exec python3 -m websockify
        --web /root/noVNC
        --token-plugin TokenFile
        --token-source $STORAGE_DIR/console-tokens
        --idle-timeout 300
        6080
    volumes:
      # Read-only, and only what the proxy needs: the token projection, and the guests'
      # console sockets ($STORAGE_DIR/console-sockets/<uuid>.sock, which every token
      # names, so the proxy sees them at the same place; read-only is enough to
      # connect()). Never all of $STORAGE_DIR, which holds every tenant's disks, volumes,
      # images and cloud-init data, nor libvirt's per-domain directories, which hold each
      # guest's QMP monitor socket: this container keeps DAC_OVERRIDE (SPEC-021 FR-023c).
      - $STORAGE_DIR/console-tokens:$STORAGE_DIR/console-tokens:ro
      - $STORAGE_DIR/console-sockets:$STORAGE_DIR/console-sockets:ro
    healthcheck:
      # The client page, not a TCP connect: the kernel accepts from the listen backlog even
      # when websockify is wedged.
      test: [CMD, python3, -c, "import urllib.request; urllib.request.urlopen('http://127.0.0.1:6080/vnc_lite.html', timeout=4)"]
      interval: 30s
      timeout: 5s
      retries: 3

networks:
  lightstack-net:
    driver: bridge
EOF

    # Create local_settings.py
    cat > /opt/horizon/local_settings.py <<EOF
import os
from django.utils.translation import gettext_lazy as _

WEBROOT = '/'
OPENSTACK_HOST = os.environ.get('OPENSTACK_HOST', '$HOST_IP')
OPENSTACK_KEYSTONE_URL = f"http://{OPENSTACK_HOST}:35357/v3"

OPENSTACK_API_VERSIONS = {
    "identity": 3,
    "image": 2,
    "volume": 3,
    "compute": 2.1,
}

OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True
OPENSTACK_KEYSTONE_DEFAULT_DOMAIN = 'Default'
OPENSTACK_KEYSTONE_DEFAULT_ROLE = 'member'

SESSION_TIMEOUT = 14400

# 'VNC', not 'novnc': Horizon's CONSOLES keys are AUTO/VNC/SPICE/RDP/SERIAL/MKS, so 'novnc'
# raises KeyError and the Console tab reports "unavailable" without ever calling Nova. Horizon
# has no console-proxy setting: the URL comes from Nova (nova.console_proxy_base_url).
CONSOLE_TYPE = 'VNC'

ALLOWED_HOSTS = ['*']
DEBUG = False

CACHES = {
    'default': {
        'BACKEND': 'django.core.cache.backends.locmem.LocMemCache',
    },
}

TIME_ZONE = "UTC"
LOGGING = {
    'version': 1,
    'disable_existing_loggers': False,
    'handlers': {
        'console': {
            'level': 'INFO',
            'class': 'logging.StreamHandler',
        },
    },
    'loggers': {
        'horizon': {
            'handlers': ['console'],
            'level': 'INFO',
            'propagate': False,
        },
        'openstack_dashboard': {
            'handlers': ['console'],
            'level': 'INFO',
            'propagate': False,
        },
    },
}
EOF

    # Start Horizon
    cd /opt/horizon
    docker compose up -d

    log_success "Horizon dashboard deployed"
}

# Configure firewall
configure_firewall() {
    log_info "Configuring firewall..."

    apt-get install -y -qq ufw > /dev/null

    # Allow SSH
    ufw allow 22/tcp

    # Allow OpenStack services
    ufw allow 35357/tcp  # Keystone
    ufw allow 8774/tcp   # Nova
    ufw allow 8775/tcp   # Metadata
    ufw allow 8776/tcp   # Cinder
    ufw allow 9292/tcp   # Glance
    ufw allow 9696/tcp   # Neutron
    ufw allow 6080/tcp   # noVNC
    ufw allow 80/tcp     # Horizon

    # Enable firewall
    ufw --force enable

    log_success "Firewall configured"
}

# Install OpenStack CLI
install_openstack_cli() {
    log_info "Installing OpenStack CLI..."

    apt-get install -y -qq python3-pip python3-openstackclient > /dev/null

    log_success "OpenStack CLI installed"
}

# Create environment file
create_env_file() {
    log_info "Creating environment file..."

    cat > /root/.lightstack-env <<EOF
# Lightstack Environment Variables
export OS_AUTH_URL=http://$HOST_IP:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF

    # Add to .bashrc
    if ! grep -q ".lightstack-env" /root/.bashrc; then
        echo "source /root/.lightstack-env" >> /root/.bashrc
    fi

    log_success "Environment file created: /root/.lightstack-env"
}

# Verify installation
verify_installation() {
    log_info "Verifying installation..."

    # Source environment
    source /root/.lightstack-env

    # Wait for services to be ready
    log_info "Waiting for services to be ready (30 seconds)..."
    sleep 30

    # Test token issue
    if openstack token issue > /dev/null 2>&1; then
        log_success "✓ Keystone authentication working"
    else
        log_error "✗ Keystone authentication failed"
        return 1
    fi

    # Test service list
    if openstack service list > /dev/null 2>&1; then
        log_success "✓ Service catalog accessible"
    else
        log_error "✗ Service catalog not accessible"
        return 1
    fi

    # Test endpoint list
    if openstack endpoint list > /dev/null 2>&1; then
        log_success "✓ API endpoints registered"
    else
        log_error "✗ API endpoints not registered"
        return 1
    fi

    log_success "Installation verified successfully!"
    return 0
}

# Show completion message
show_completion() {
    echo ""
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║                                                           ║"
    echo "║   Lightstack Single-Node Deployment Complete! 🎉                ║"
    echo "║                                                           ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""
    log_info "Deployment Summary:"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  Host IP:              $HOST_IP"
    echo "  Hostname:             $HOSTNAME"
    echo ""
    echo "  OpenStack Services:"
    echo "    - Keystone (Identity):     http://$HOST_IP:35357/v3"
    echo "    - Nova (Compute):          http://$HOST_IP:8774/v2.1"
    echo "    - Neutron (Network):       http://$HOST_IP:9696/v2.0"
    echo "    - Cinder (Block Storage):  http://$HOST_IP:8776/v3"
    echo "    - Glance (Image):          http://$HOST_IP:9292/v2"
    echo ""

    if [[ "$DEPLOY_HORIZON" =~ ^[Yy]$ ]]; then
        echo "  Horizon Dashboard:    http://$HOST_IP"
        echo "    - Domain:   Default"
        echo "    - Username: admin"
        echo "    - Password: secret"
        echo ""
    fi

    echo "  Configuration:        /opt/lightstack/config/lightstack.yaml"
    echo "  Environment:          /root/.lightstack-env"
    echo "  Logs:                 journalctl -u lightstack.service -f"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    log_info "Next Steps:"
    echo "  1. Source environment: source /root/.lightstack-env"
    echo "  2. Test authentication: openstack token issue"
    echo "  3. Create your first VM: openstack server create --help"
    if [[ "$DEPLOY_HORIZON" =~ ^[Yy]$ ]]; then
        echo "  4. Access Horizon: http://$HOST_IP"
    fi
    echo ""
    log_info "Documentation: https://github.com/cobaltcore-dev/lightstack/blob/main/docs/SINGLE_NODE_DEPLOYMENT.md"
    echo ""
}

# Main installation flow
main() {
    show_banner

    check_root
    detect_os
    check_hardware
    get_configuration

    echo ""
    log_info "Starting installation..."
    echo ""

    update_system
    configure_hostname
    install_kvm
    configure_networking
    install_postgresql
    install_docker
    clone_lightstack
    build_lightstack
    create_config
    create_storage_dirs
    run_migrations
    create_systemd_service
    deploy_horizon
    configure_firewall
    install_openstack_cli
    create_env_file

    echo ""
    if verify_installation; then
        show_completion
    else
        log_error "Installation completed but verification failed. Check logs: journalctl -u lightstack.service -n 50"
        exit 1
    fi
}

# Run main function
main "$@"
