#!/bin/bash
#
# O3K Turnkey Deployment Script
# One-command deployment of O3K + Horizon + KVM on a dedicated host.
#
# Usage:
#   sudo O3K_HOST_IP=10.2.199.101 O3K_NETWORK_IFACE=ens19 O3K_GATEWAY=10.2.199.254 \
#       ./scripts/deploy-turnkey.sh
#   ./scripts/deploy-turnkey.sh --help
#
# This script deploys a complete single-node OpenStack environment:
#   - O3K (all 5 core services) with real KVM hypervisor
#   - Horizon dashboard (Flamingo 2025.2) with noVNC console
#   - PostgreSQL 18 database
#   - Network bridge (br-ext) on $O3K_NETWORK_IFACE for VM external connectivity
#
# It is a development/lab deployment, not a hardened production one: it runs the
# O3K container privileged, terminates TLS nowhere, and stores the Keystone JWT
# secret in a file on disk.
#
# WARNING: this rewrites /etc/netplan and moves $O3K_HOST_IP onto a new br-ext
# bridge, taking over $O3K_NETWORK_IFACE. Run it only on a host you intend to
# dedicate to O3K. On failure every change it made is rolled back (see rollback()).
#
# Configuration comes from the environment -- there are no host-specific defaults.
# Run with --help for the full list of variables.
#
# Requirements:
#   - Ubuntu 24.04/22.04 or Debian 12
#   - 4+ CPU cores, 16+ GB RAM, 100+ GB disk
#   - KVM virtualization support (Intel VT-x or AMD-V)
#   - Root/sudo access
#   - openssl (secret generation; no insecure fallback)
#

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Configuration. Network coordinates are required and have no defaults on purpose:
# a wrong guess here rewrites the host's networking. Validated in validate_config().
HOST_IP="${O3K_HOST_IP:-}"
NETWORK_IFACE="${O3K_NETWORK_IFACE:-}"
GATEWAY="${O3K_GATEWAY:-}"
DNS_SERVERS="${O3K_DNS_SERVERS:-8.8.8.8}"
NETMASK="${O3K_NETMASK:-24}"
# Secrets: generated in generate_secrets() unless supplied. Never hardcoded.
DB_PASSWORD="${O3K_DB_PASSWORD:-}"
JWT_SECRET=""
INSTALL_DIR="${O3K_INSTALL_DIR:-/opt/o3k}"
CONFIG_DIR="$INSTALL_DIR/config"
STORAGE_DIR="${O3K_STORAGE_DIR:-/var/lib/o3k}"
REPO_DIR="${O3K_REPO_DIR:-/root/o3k}"

# Logging functions
log_info() { echo -e "${BLUE}[INFO]${NC} ${1:-}"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} ${1:-}"; }
log_warning() { echo -e "${YELLOW}[WARNING]${NC} ${1:-}"; }
log_error() { echo -e "${RED}[ERROR]${NC} ${1:-}" >&2; }

# Abort with a message. Marks the exit deliberate so on_exit() skips the
# "unexpected failure" diagnostic but still rolls back whatever was applied.
die() {
    log_error "${1:-unspecified error}"
    CLEAN_ABORT=1
    exit "${2:-1}"
}

usage() {
    cat >&2 <<'USAGE'
Usage: sudo O3K_HOST_IP=<ip> O3K_NETWORK_IFACE=<iface> O3K_GATEWAY=<ip> \
           ./scripts/deploy-turnkey.sh

Required:
  O3K_HOST_IP        IPv4 address to move onto the br-ext bridge (e.g. 10.2.199.101)
  O3K_NETWORK_IFACE  Existing host interface to enslave into br-ext (e.g. ens19)
  O3K_GATEWAY        IPv4 default gateway reachable via br-ext (e.g. 10.2.199.254)

Optional:
  O3K_NETMASK        Prefix length for O3K_HOST_IP                (default: 24)
  O3K_DNS_SERVERS    Comma-separated resolvers                    (default: 8.8.8.8)
  O3K_DB_PASSWORD    Password for the PostgreSQL o3k role; 12+ chars from
                     [A-Za-z0-9._~-]                              (default: generated)
  O3K_INSTALL_DIR    Config install prefix                        (default: /opt/o3k)
  O3K_STORAGE_DIR    Volume/image/instance storage root           (default: /var/lib/o3k)
  O3K_REPO_DIR       O3K checkout used as the Docker build context (default: /root/o3k)

WARNING: this rewrites /etc/netplan and takes over O3K_NETWORK_IFACE. Run it only
on a host you intend to dedicate to O3K.
USAGE
}

# --- failure handling ---------------------------------------------------------
# Every step that mutates the host records what it did via record(); rollback()
# undoes those in reverse order when the script exits non-zero. Package installs
# and storage directories are deliberately not reverted (see rollback()).
CLEAN_ABORT=""
FAILED_CMD=""
FAILED_LINE=""
STATE_CHANGES=0
STATE_NETPLAN=0
STATE_NETPLAN_BACKUP=""
STATE_SYSCTL=0
STATE_DB_ROLE_CREATED=0
STATE_DB_CREATED=0
STATE_DB_PW_CHANGED=0
STATE_PGHBA_BACKUP=""
STATE_CONFIG=0
STATE_COMPOSE=0
PG_HBA=""

# record <state-var> [value] -- remember a mutation so rollback() can undo it.
record() {
    local -n slot="$1"
    slot="${2:-1}"
    STATE_CHANGES=$((STATE_CHANGES + 1))
}

on_err() {
    FAILED_CMD="$BASH_COMMAND"
    FAILED_LINE="${BASH_LINENO[0]}"
}

rollback() {
    log_warning "Rolling back $STATE_CHANGES change(s) applied by this run..."

    if [ "$STATE_COMPOSE" -eq 1 ]; then
        log_info "Stopping containers..."
        docker compose -f "$REPO_DIR/deployments/docker-compose-turnkey.yml" down \
            --remove-orphans > /dev/null 2>&1 || log_warning "docker compose down failed"
    fi

    if [ "$STATE_CONFIG" -eq 1 ]; then
        log_info "Removing generated configuration ($CONFIG_DIR)..."
        rm -rf "$CONFIG_DIR"
    fi

    if [ -n "$STATE_PGHBA_BACKUP" ]; then
        if [ -f "$STATE_PGHBA_BACKUP" ]; then
            log_info "Restoring $PG_HBA..."
            mv -f "$STATE_PGHBA_BACKUP" "$PG_HBA" && systemctl restart postgresql > /dev/null 2>&1 \
                || log_warning "could not restore $PG_HBA"
        else
            log_warning "backup $STATE_PGHBA_BACKUP is gone; $PG_HBA still has the o3k line"
        fi
    fi

    if [ "$STATE_DB_CREATED" -eq 1 ]; then
        log_info "Dropping database o3k..."
        sudo -u postgres psql -q -c 'DROP DATABASE IF EXISTS o3k;' > /dev/null 2>&1 \
            || log_warning "could not drop database o3k"
    fi

    if [ "$STATE_DB_ROLE_CREATED" -eq 1 ]; then
        log_info "Dropping role o3k..."
        sudo -u postgres psql -q -c 'DROP ROLE IF EXISTS o3k;' > /dev/null 2>&1 \
            || log_warning "could not drop role o3k"
    fi

    if [ "$STATE_SYSCTL" -eq 1 ]; then
        log_info "Removing bridge netfilter configuration..."
        rm -f /etc/sysctl.d/99-o3k-bridge.conf /etc/modules-load.d/o3k.conf
    fi

    if [ "$STATE_NETPLAN" -eq 1 ]; then
        log_info "Reverting netplan bridge configuration..."
        rm -f /etc/netplan/01-o3k-bridge.yaml
        if [ -n "$STATE_NETPLAN_BACKUP" ]; then
            if [ -f "$STATE_NETPLAN_BACKUP" ]; then
                log_info "Restoring the previous /etc/netplan/01-o3k-bridge.yaml..."
                mv -f "$STATE_NETPLAN_BACKUP" /etc/netplan/01-o3k-bridge.yaml
            else
                log_warning "backup $STATE_NETPLAN_BACKUP is gone; the previous bridge config is lost"
            fi
        fi
        netplan apply > /dev/null 2>&1 \
            || log_warning "netplan apply failed during rollback -- check networking by hand"
    fi

    if [ "$STATE_DB_PW_CHANGED" -eq 1 ]; then
        log_warning "The password of the pre-existing PostgreSQL role o3k was changed and NOT reverted."
    fi
    log_warning "Installed packages (docker, libvirt, postgresql) were left in place."
    log_warning "Storage directories under $STORAGE_DIR were left in place."
    log_warning "Rollback complete."
}

on_exit() {
    local rc="$1"
    trap - ERR EXIT
    set +e
    [ "$rc" -eq 0 ] && return 0
    if [ -z "$CLEAN_ABORT" ]; then
        log_error "Aborted at line ${FAILED_LINE:-?} (exit $rc): ${FAILED_CMD:-unknown command}"
    fi
    [ "$STATE_CHANGES" -eq 0 ] && return 0
    rollback
}

trap 'on_err' ERR
trap 'on_exit $?' EXIT

# --- validation ---------------------------------------------------------------
is_ipv4() {
    local ip="${1:-}" octet
    [[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    local IFS='.'
    for octet in $ip; do
        [ "$((10#$octet))" -le 255 ] || return 1
    done
    return 0
}

# Netplan wants one YAML list item per resolver; O3K_DNS_SERVERS is comma-separated.
render_dns_yaml() {
    local entry sep="" out=""
    local IFS=','
    for entry in $DNS_SERVERS; do
        out+="${sep}          - ${entry}"
        sep=$'\n'
    done
    printf '%s' "$out"
}

validate_dns_servers() {
    local entry
    local IFS=','
    for entry in $DNS_SERVERS; do
        is_ipv4 "$entry" || die "O3K_DNS_SERVERS contains an invalid IPv4 address: $entry"
    done
}

check_prerequisites() {
    local cmd
    for cmd in openssl ip; do
        command -v "$cmd" > /dev/null 2>&1 \
            || die "$cmd is required but not installed (no insecure fallback)"
    done
}

validate_config() {
    local missing=0

    [ -n "$HOST_IP" ] || { log_error "O3K_HOST_IP is required"; missing=1; }
    [ -n "$NETWORK_IFACE" ] || { log_error "O3K_NETWORK_IFACE is required"; missing=1; }
    [ -n "$GATEWAY" ] || { log_error "O3K_GATEWAY is required"; missing=1; }
    if [ "$missing" -ne 0 ]; then
        usage
        die "refusing to run without explicit network configuration"
    fi

    is_ipv4 "$HOST_IP" || die "O3K_HOST_IP is not a valid IPv4 address: $HOST_IP"
    is_ipv4 "$GATEWAY" || die "O3K_GATEWAY is not a valid IPv4 address: $GATEWAY"

    [[ "$NETMASK" =~ ^[0-9]+$ ]] && [ "$NETMASK" -ge 1 ] && [ "$NETMASK" -le 32 ] \
        || die "O3K_NETMASK must be a prefix length between 1 and 32: $NETMASK"

    validate_dns_servers

    ip link show "$NETWORK_IFACE" > /dev/null 2>&1 \
        || die "network interface $NETWORK_IFACE not found on this host"

    # The password is interpolated into an SQL string literal and a postgres:// DSN,
    # so restrict it to characters that need no quoting or percent-encoding.
    if [ -n "$DB_PASSWORD" ] && ! [[ "$DB_PASSWORD" =~ ^[A-Za-z0-9._~-]{12,}$ ]]; then
        die "O3K_DB_PASSWORD must be 12+ characters from [A-Za-z0-9._~-]"
    fi

    log_success "Configuration validated"
}

generate_secrets() {
    JWT_SECRET="$(openssl rand -hex 32)"
    if [ -z "$DB_PASSWORD" ]; then
        DB_PASSWORD="$(openssl rand -hex 16)"
        log_info "Generated a PostgreSQL password for role o3k (written to $CONFIG_DIR/o3k.yaml)"
    fi
}

# Banner
show_banner() {
    cat << "EOF"
╔═══════════════════════════════════════════════════════════╗
║                                                           ║
║   O3K Turnkey Deployment                                 ║
║   Complete OpenStack in One Command                      ║
║                                                           ║
╚═══════════════════════════════════════════════════════════╝
EOF
    echo ""
    echo "Network Configuration:"
    echo "  Interface: $NETWORK_IFACE"
    echo "  IP: $HOST_IP/$NETMASK"
    echo "  Gateway: $GATEWAY"
    echo "  DNS: $DNS_SERVERS"
    echo ""
}

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        die "this script must be run as root (use sudo)"
    fi
}

# Detect OS
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS="${ID:-}"
        OS_VERSION="${VERSION_ID:-unknown}"
    else
        die "cannot detect OS: /etc/os-release not found"
    fi

    log_info "Detected OS: $OS $OS_VERSION"

    case "$OS" in
        ubuntu|debian) ;;
        *) die "unsupported OS: ${OS:-unknown}. Supported: Ubuntu, Debian" ;;
    esac
}

# Check hardware
check_hardware() {
    log_info "Checking hardware requirements..."

    CPU_CORES=$(nproc)
    if [ "$CPU_CORES" -lt 4 ]; then
        die "insufficient CPU cores: $CPU_CORES (minimum: 4)"
    fi

    TOTAL_RAM=$(free -g | awk '/^Mem:/{print $2}')
    if [ "$TOTAL_RAM" -lt 16 ]; then
        log_warning "RAM is ${TOTAL_RAM}GB (recommended: 16GB+)"
    fi

    if ! grep -Ec '(vmx|svm)' /proc/cpuinfo > /dev/null 2>&1; then
        die "KVM virtualization not supported by CPU"
    fi

    log_success "Hardware requirements met (${CPU_CORES} cores, ${TOTAL_RAM}GB RAM)"
}

# Install system packages
install_packages() {
    log_info "Installing system packages..."

    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq \
        apt-transport-https \
        ca-certificates \
        curl \
        gnupg \
        lsb-release \
        qemu-kvm \
        libvirt-daemon-system \
        libvirt-clients \
        bridge-utils \
        netplan.io \
        postgresql \
        postgresql-contrib \
        jq \
        python3-openstackclient \
        > /dev/null 2>&1

    log_success "System packages installed"
}

# Install Docker
install_docker() {
    if command -v docker &> /dev/null; then
        log_success "Docker already installed ($(docker --version))"
        return
    fi

    log_info "Installing Docker..."

    curl -fsSL "https://download.docker.com/linux/$OS/gpg" \
        | gpg --batch --yes --dearmor -o /usr/share/keyrings/docker-archive-keyring.gpg
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/docker-archive-keyring.gpg] https://download.docker.com/linux/$OS $(lsb_release -cs) stable" | \
        tee /etc/apt/sources.list.d/docker.list > /dev/null

    apt-get update -qq
    apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-compose-plugin > /dev/null 2>&1

    systemctl enable --now docker > /dev/null 2>&1
    usermod -aG docker "${SUDO_USER:-${USER:-root}}" 2>/dev/null || true

    log_success "Docker installed ($(docker --version))"
}

# Configure networking
configure_networking() {
    log_info "Configuring network bridge on $NETWORK_IFACE..."

    # Verify interface exists (also checked up front in validate_config)
    if ! ip link show "$NETWORK_IFACE" > /dev/null 2>&1; then
        die "network interface $NETWORK_IFACE not found"
    fi

    # Preserve any existing drop-in so rollback() can put it back
    if [ -f /etc/netplan/01-o3k-bridge.yaml ]; then
        STATE_NETPLAN_BACKUP="/etc/netplan/01-o3k-bridge.yaml.o3k-bak.$$"
        cp -p /etc/netplan/01-o3k-bridge.yaml "$STATE_NETPLAN_BACKUP"
    fi

    # Create netplan configuration
    record STATE_NETPLAN
    cat > /etc/netplan/01-o3k-bridge.yaml <<EOF
network:
  version: 2
  renderer: networkd
  ethernets:
    $NETWORK_IFACE:
      dhcp4: no
      dhcp6: no
  bridges:
    br-ext:
      interfaces: [$NETWORK_IFACE]
      addresses:
        - $HOST_IP/$NETMASK
      routes:
        - to: default
          via: $GATEWAY
      nameservers:
        addresses:
$(render_dns_yaml)
      dhcp4: no
      dhcp6: no
      parameters:
        stp: false
        forward-delay: 0
EOF

    # Apply netplan
    netplan apply
    sleep 3

    # Verify bridge
    if ! ip link show br-ext > /dev/null 2>&1; then
        die "bridge br-ext creation failed"
    fi

    log_success "Network bridge configured (br-ext on $NETWORK_IFACE)"
}

# Configure bridge netfilter
configure_bridge_netfilter() {
    log_info "Configuring bridge netfilter..."

    if modprobe br_netfilter 2>/dev/null; then
        record STATE_SYSCTL
        echo "br_netfilter" > /etc/modules-load.d/o3k.conf

        cat > /etc/sysctl.d/99-o3k-bridge.conf <<EOF
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
EOF
        sysctl -p /etc/sysctl.d/99-o3k-bridge.conf > /dev/null 2>&1
        log_success "Bridge netfilter configured"
    else
        log_warning "Could not load br_netfilter module (may not be needed)"
    fi
}

# Setup PostgreSQL
setup_database() {
    log_info "Configuring PostgreSQL database..."

    systemctl enable --now postgresql > /dev/null 2>&1

    # Create the role and database only if absent, so a failed run never drops a
    # pre-existing database in rollback().
    if [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'o3k'")" = "1" ]; then
        log_warning "PostgreSQL role o3k already exists; updating its password to match the generated config"
        sudo -u postgres psql -q -c "ALTER ROLE o3k WITH ENCRYPTED PASSWORD '$DB_PASSWORD';"
        record STATE_DB_PW_CHANGED
    else
        sudo -u postgres psql -q -c "CREATE ROLE o3k LOGIN ENCRYPTED PASSWORD '$DB_PASSWORD';"
        record STATE_DB_ROLE_CREATED
    fi

    if [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'o3k'")" = "1" ]; then
        log_warning "Database o3k already exists; leaving its contents untouched"
    else
        sudo -u postgres psql -q -c 'CREATE DATABASE o3k OWNER o3k;'
        record STATE_DB_CREATED
    fi
    sudo -u postgres psql -q -c 'GRANT ALL PRIVILEGES ON DATABASE o3k TO o3k;'

    # Allow local connections
    PG_VERSION=$(psql --version | awk '{print $3}' | cut -d. -f1)
    PG_HBA="/etc/postgresql/$PG_VERSION/main/pg_hba.conf"

    if [ -f "$PG_HBA" ]; then
        if ! grep -q "host.*o3k.*o3k.*127.0.0.1" "$PG_HBA"; then
            local backup="$PG_HBA.o3k-bak.$$"
            cp -p "$PG_HBA" "$backup"
            record STATE_PGHBA_BACKUP "$backup"
            echo "host    o3k    o3k    127.0.0.1/32    md5" >> "$PG_HBA"
            systemctl restart postgresql
        fi
    fi

    log_success "PostgreSQL configured"
}

# Create storage directories
create_storage() {
    log_info "Creating storage directories..."

    mkdir -p "$STORAGE_DIR"/{volumes,images,instances}
    chmod 755 "$STORAGE_DIR"/{volumes,images,instances}

    log_success "Storage directories created at $STORAGE_DIR"
}

# Generate O3K configuration
generate_config() {
    log_info "Generating O3K configuration..."

    [ -d "$CONFIG_DIR" ] || record STATE_CONFIG
    mkdir -p "$CONFIG_DIR"

    # O3K main configuration
    cat > "$CONFIG_DIR/o3k.yaml" <<EOF
database:
  url: "postgres://o3k:$DB_PASSWORD@localhost:5432/o3k?sslmode=disable"
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
  instance_storage_path: "$STORAGE_DIR/instances"
  console_proxy_base_url: "http://$HOST_IP:6080/vnc_auto.html"

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
  local_storage_path: "$STORAGE_DIR/volumes"

glance:
  host: "0.0.0.0"
  port: 9292
  storage_mode: local
  local_storage_path: "$STORAGE_DIR/images"

metadata:
  host: "0.0.0.0"
  port: 8775

compute:
  node_id: "compute-node-1"
  node_name: "$(hostname)"
  tunnel_ip: "$HOST_IP"
EOF

    # Copy Horizon configuration from repo and modify for host networking
    if [ ! -d "$REPO_DIR/deployments/horizon-config" ]; then
        die "Horizon config not found at $REPO_DIR/deployments/horizon-config (set O3K_REPO_DIR)"
    fi

    # Create modified local_settings for host networking (memcached on HOST_IP)
    log_info "Creating Horizon configuration for host networking..."
    mkdir -p "$CONFIG_DIR/horizon-config"

    # Copy local_settings and change memcached location for host networking
    # Use HOST_IP instead of localhost since containers have separate network namespaces
    sed "s/memcached:11211/$HOST_IP:11211/g" \
        "$REPO_DIR/deployments/horizon-config/local_settings" > "$CONFIG_DIR/horizon-config/local_settings"

    # Also fix OPENSTACK_HOST to use HOST_IP for host networking
    sed -i "s/OPENSTACK_HOST = \"o3k\"/OPENSTACK_HOST = \"$HOST_IP\"/g" "$CONFIG_DIR/horizon-config/local_settings"

    # Fix noVNC URL to use HOST_IP
    sed -i "s/novnc:6080/$HOST_IP:6080/g" "$CONFIG_DIR/horizon-config/local_settings"

    log_success "Configuration generated at $CONFIG_DIR"
}

# Create Docker Compose file
create_docker_compose() {
    log_info "Creating Docker Compose configuration..."

    cd "$REPO_DIR"

    cat > "$REPO_DIR/deployments/docker-compose-turnkey.yml" <<EOF
services:
  # Memcached for Horizon sessions
  memcached:
    image: memcached:1.6-alpine
    container_name: o3k-memcached
    hostname: memcached
    network_mode: host
    command: memcached -m 64 -l 0.0.0.0
    restart: unless-stopped

  # O3K OpenStack Services with KVM
  o3k:
    build:
      context: ..
      dockerfile: build/package/Dockerfile
    image: o3k:latest
    container_name: o3k
    privileged: true
    network_mode: host
    volumes:
      - /var/run/libvirt/libvirt-sock:/var/run/libvirt/libvirt-sock
      - $STORAGE_DIR/volumes:$STORAGE_DIR/volumes
      - $STORAGE_DIR/images:$STORAGE_DIR/images
      - $STORAGE_DIR/instances:$STORAGE_DIR/instances
      - $CONFIG_DIR:/opt/o3k/config:ro
    command: [/app/o3k, -config, /opt/o3k/config/o3k.yaml]
    restart: unless-stopped
    healthcheck:
      test: [CMD-SHELL, "curl -f http://localhost:35357/v3 || exit 1"]
      interval: 10s
      timeout: 5s
      retries: 3

  # noVNC console client (bonigarcia/novnc, pinned). Needs a VNC target; O3K exposes none, so
  # VNC_SERVER is a dead placeholder and the console fails on connect (research.md U1).
  novnc:
    image: bonigarcia/novnc:1.3.0
    container_name: o3k-novnc
    hostname: novnc
    network_mode: host
    environment:
      - VNC_SERVER=127.0.0.1:5900
    restart: unless-stopped
    healthcheck:
      test: [CMD, nc, -z, localhost, "6080"]
      interval: 30s
      timeout: 5s
      retries: 3

  # OpenStack Horizon Dashboard (using working config from repo)
  horizon:
    image: quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble
    container_name: o3k-horizon
    hostname: horizon
    network_mode: host
    depends_on:
      o3k:
        condition: service_healthy
      novnc:
        condition: service_started
    environment:
      - KOLLA_INSTALL_TYPE=source
      - KOLLA_CONFIG_STRATEGY=COPY_ALWAYS
    volumes:
      - ./horizon-config/config.json:/var/lib/kolla/config_files/config.json:ro
      - $CONFIG_DIR/horizon-config/local_settings:/var/lib/kolla/config_files/local_settings:ro
      - ./horizon-config/apache/ports.conf:/var/lib/kolla/config_files/ports.conf:ro
      - ./horizon-config/apache/horizon-nolist.conf:/var/lib/kolla/config_files/horizon-nolist.conf:ro
      - horizon-static:/var/lib/kolla/venv/lib/python3.12/site-packages/static:rw
      - horizon-logs:/var/log/kolla/horizon:rw
    restart: unless-stopped
    healthcheck:
      test: [CMD, curl, -f, "http://localhost:80/dashboard/auth/login/"]
      interval: 30s
      timeout: 10s
      retries: 5
      start_period: 60s

volumes:
  horizon-static:
  horizon-logs:
EOF

    log_success "Docker Compose configuration created"
}

# Build and start services
deploy_services() {
    log_info "Building O3K image (this may take several minutes)..."

    cd "$REPO_DIR/deployments"

    docker compose -f docker-compose-turnkey.yml build --quiet
    log_success "O3K image built"

    log_info "Starting services..."
    record STATE_COMPOSE
    docker compose -f docker-compose-turnkey.yml up -d

    log_info "Waiting for services to be ready..."
    sleep 10

    # Wait for O3K to be healthy
    for i in {1..30}; do
        if curl -sf http://localhost:35357/v3 > /dev/null 2>&1; then
            log_success "O3K services are ready"
            break
        fi
        if [ "$i" -eq 30 ]; then
            docker compose -f docker-compose-turnkey.yml logs o3k
            die "O3K failed to start within 5 minutes"
        fi
        sleep 10
    done

    # Wait for Horizon to be ready
    log_info "Waiting for Horizon dashboard..."
    for i in {1..30}; do
        if curl -sf http://localhost:8080/dashboard/auth/login/ > /dev/null 2>&1; then
            log_success "Horizon dashboard is ready"
            break
        fi
        if [ "$i" -eq 30 ]; then
            log_warning "Horizon may still be starting (takes 2-3 minutes)"
        fi
        sleep 10
    done
}

# Create CLI environment file
create_cli_env() {
    log_info "Creating OpenStack CLI environment file..."

    cat > /root/.o3k-env <<EOF
# O3K OpenStack Environment
export OS_AUTH_URL=http://$HOST_IP:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF

    # Add to bashrc if not already there
    if ! grep -q "source /root/.o3k-env" /root/.bashrc; then
        echo "source /root/.o3k-env" >> /root/.bashrc
    fi

    log_success "CLI environment configured"
}

# Show completion message
show_completion() {
    log_success "O3K deployment complete!"
    echo ""
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║                    Access Information                      ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""
    echo "🌐 Horizon Dashboard:"
    echo "   URL: http://$HOST_IP/dashboard"
    echo "   Username: admin"
    echo "   Password: secret"
    echo "   Domain: Default"
    echo ""
    echo "🖥️  noVNC Console:"
    echo "   URL: http://$HOST_IP:6080"
    echo ""
    echo "🔧 OpenStack CLI:"
    echo "   source /root/.o3k-env"
    echo "   openstack token issue"
    echo "   openstack server list"
    echo ""
    echo "📊 Service Status:"
    echo "   docker compose -f /root/o3k/deployments/docker-compose-turnkey.yml ps"
    echo ""
    echo "📋 Logs:"
    echo "   docker compose -f /root/o3k/deployments/docker-compose-turnkey.yml logs -f"
    echo ""
    echo "Network Configuration:"
    echo "   Bridge: br-ext"
    echo "   Interface: $NETWORK_IFACE"
    echo "   Host IP: $HOST_IP/$NETMASK"
    echo "   Gateway: $GATEWAY"
    echo ""
    echo "⚡ Quick Test:"
    echo "   source /root/.o3k-env"
    echo "   openstack network create test-net"
    echo "   openstack server create --flavor m1.small --image cirros --network test-net test-vm"
    echo "   openstack server list"
    echo ""
}

# Main deployment flow
main() {
    case "${1:-}" in
        -h|--help) usage; CLEAN_ABORT=1; exit 0 ;;
    esac

    check_root
    check_prerequisites
    validate_config
    generate_secrets
    show_banner
    detect_os
    check_hardware
    install_packages
    install_docker
    configure_networking
    configure_bridge_netfilter
    setup_database
    create_storage
    generate_config
    create_docker_compose
    deploy_services
    create_cli_env
    show_completion
}

# Run main function
main "$@"
