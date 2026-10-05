#!/bin/bash
#
# Lightstack Turnkey Deployment Script
# One-command deployment of Lightstack + Horizon + KVM on a dedicated host.
#
# Usage:
#   sudo LIGHTSTACK_HOST_IP=10.2.199.101 LIGHTSTACK_NETWORK_IFACE=ens19 LIGHTSTACK_GATEWAY=10.2.199.254 \
#       ./scripts/deploy-turnkey.sh
#   ./scripts/deploy-turnkey.sh --help
#
# This script deploys a complete single-node OpenStack environment:
#   - Lightstack (all 5 core services) with real KVM hypervisor
#   - Horizon dashboard (Flamingo 2025.2) with noVNC console
#   - PostgreSQL 18 database
#   - Network bridge (br-ext) on $LIGHTSTACK_NETWORK_IFACE for VM external connectivity
#
# It is a development/lab deployment, not a hardened production one: it runs the
# Lightstack container privileged, terminates TLS nowhere, and stores the Keystone JWT
# secret in a file on disk.
#
# WARNING: this rewrites /etc/netplan and moves $LIGHTSTACK_HOST_IP onto a new br-ext
# bridge, taking over $LIGHTSTACK_NETWORK_IFACE. Run it only on a host you intend to
# dedicate to Lightstack. On failure it rolls back, in reverse order, what it changed in netplan,
# sysctl, PostgreSQL and the generated config, and stops the containers it started. Installed
# packages, $LIGHTSTACK_STORAGE_DIR and a pre-existing lightstack role's changed password are deliberately
# left in place — rollback() logs exactly what it skipped.
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

set -Eeuo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Configuration. Network coordinates are required and have no defaults on purpose:
# a wrong guess here rewrites the host's networking. Validated in validate_config().
HOST_IP="${LIGHTSTACK_HOST_IP:-}"
NETWORK_IFACE="${LIGHTSTACK_NETWORK_IFACE:-}"
GATEWAY="${LIGHTSTACK_GATEWAY:-}"
DNS_SERVERS="${LIGHTSTACK_DNS_SERVERS:-8.8.8.8}"
NETMASK="${LIGHTSTACK_NETMASK:-24}"
# Secrets: never hardcoded. The DB password and JWT secret are written to
# $CONFIG_DIR/lightstack.yaml by generate_config(); the Horizon key is injected into the
# generated Compose file by create_docker_compose().
# The DB password can be supplied via LIGHTSTACK_DB_PASSWORD; the JWT secret and the Horizon
# SECRET_KEY cannot, and generate_secrets() mints fresh ones on every run -- so a redeploy
# invalidates every outstanding token and every Horizon session. Acceptable for a lab host:
# tokens are stateless, with token_ttl 24h.
#
# HORIZON_SECRET_KEY must be injected rather than left to local_settings' file fallback:
# kolla's extend_start.sh imports the settings as root (collectstatic/compress), creating
# /tmp/.horizon_secret_key as root:0600, while apache's WSGIDaemonProcess runs as `horizon` --
# so the workers get PermissionError on read and Horizon fails to start.
DB_PASSWORD="${LIGHTSTACK_DB_PASSWORD:-}"
JWT_SECRET=""
HORIZON_SECRET_KEY=""
INSTALL_DIR="${LIGHTSTACK_INSTALL_DIR:-/opt/lightstack}"
CONFIG_DIR="$INSTALL_DIR/config"
STORAGE_DIR="${LIGHTSTACK_STORAGE_DIR:-/var/lib/lightstack}"
REPO_DIR="${LIGHTSTACK_REPO_DIR:-/root/lightstack}"

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
Usage: sudo LIGHTSTACK_HOST_IP=<ip> LIGHTSTACK_NETWORK_IFACE=<iface> LIGHTSTACK_GATEWAY=<ip> \
           ./scripts/deploy-turnkey.sh

Required:
  LIGHTSTACK_HOST_IP        IPv4 address to move onto the br-ext bridge (e.g. 10.2.199.101)
  LIGHTSTACK_NETWORK_IFACE  Existing host interface to enslave into br-ext (e.g. ens19)
  LIGHTSTACK_GATEWAY        IPv4 default gateway reachable via br-ext (e.g. 10.2.199.254)

Optional:
  LIGHTSTACK_NETMASK        Prefix length for LIGHTSTACK_HOST_IP                (default: 24)
  LIGHTSTACK_DNS_SERVERS    Comma-separated resolvers                    (default: 8.8.8.8)
  LIGHTSTACK_DB_PASSWORD    Password for the PostgreSQL lightstack role; 12+ chars from
                     [A-Za-z0-9._~-]                              (default: generated)
  LIGHTSTACK_INSTALL_DIR    Config install prefix                        (default: /opt/lightstack)
  LIGHTSTACK_STORAGE_DIR    Node-local state root (state_path)           (default: /var/lib/lightstack)
  LIGHTSTACK_REPO_DIR       Lightstack checkout used as the Docker build context (default: /root/lightstack)

WARNING: this rewrites /etc/netplan and takes over LIGHTSTACK_NETWORK_IFACE. Run it only
on a host you intend to dedicate to Lightstack.
USAGE
}

# --- failure handling ---------------------------------------------------------
# Every step that mutates the host records what it did via record(); rollback()
# undoes those in reverse order when the script exits non-zero. Package installs, storage
# directories and a pre-existing role's changed password are deliberately not reverted, and
# a restore is skipped if its backup file has gone — rollback() warns in each of those cases.
CLEAN_ABORT=""
FAILED_CMD=""
FAILED_LINE=""
STATE_CHANGES=0
STATE_NETPLAN=0
STATE_NETPLAN_BACKUP=""
STATE_SYSCTL=0
STATE_SYSCTL_BACKUP=""
STATE_MODULES_BACKUP=""
STATE_DB_ROLE_CREATED=0
STATE_DB_CREATED=0
STATE_DB_PW_CHANGED=0
STATE_PGHBA_BACKUP=""
STATE_CONFIG=0
STATE_CONFIG_BACKUP=""
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
    elif [ -n "$STATE_CONFIG_BACKUP" ]; then
        if [ -d "$STATE_CONFIG_BACKUP" ]; then
            log_info "Restoring the previous configuration ($CONFIG_DIR)..."
            rm -rf "$CONFIG_DIR"
            mv -f "$STATE_CONFIG_BACKUP" "$CONFIG_DIR" \
                || log_warning "could not restore $CONFIG_DIR"
        else
            log_warning "backup $STATE_CONFIG_BACKUP is gone; $CONFIG_DIR holds this run's config"
        fi
    fi

    if [ -n "$STATE_PGHBA_BACKUP" ]; then
        if [ -f "$STATE_PGHBA_BACKUP" ]; then
            log_info "Restoring $PG_HBA..."
            mv -f "$STATE_PGHBA_BACKUP" "$PG_HBA" && systemctl restart postgresql > /dev/null 2>&1 \
                || log_warning "could not restore $PG_HBA"
        else
            log_warning "backup $STATE_PGHBA_BACKUP is gone; $PG_HBA still has the lightstack line"
        fi
    fi

    if [ "$STATE_DB_CREATED" -eq 1 ]; then
        log_info "Dropping database lightstack..."
        sudo -u postgres psql -q -c 'DROP DATABASE IF EXISTS lightstack;' > /dev/null 2>&1 \
            || log_warning "could not drop database lightstack"
    fi

    if [ "$STATE_DB_ROLE_CREATED" -eq 1 ]; then
        log_info "Dropping role lightstack..."
        sudo -u postgres psql -q -c 'DROP ROLE IF EXISTS lightstack;' > /dev/null 2>&1 \
            || log_warning "could not drop role lightstack"
    fi

    if [ "$STATE_SYSCTL" -eq 1 ]; then
        log_info "Reverting bridge netfilter configuration..."
        if [ -n "$STATE_SYSCTL_BACKUP" ]; then
            if [ -f "$STATE_SYSCTL_BACKUP" ]; then
                mv -f "$STATE_SYSCTL_BACKUP" /etc/sysctl.d/99-lightstack-bridge.conf
                sysctl -p /etc/sysctl.d/99-lightstack-bridge.conf > /dev/null 2>&1 \
                    || log_warning "could not re-apply the restored /etc/sysctl.d/99-lightstack-bridge.conf"
            else
                log_warning "backup $STATE_SYSCTL_BACKUP is gone; the previous /etc/sysctl.d/99-lightstack-bridge.conf is lost"
            fi
        else
            rm -f /etc/sysctl.d/99-lightstack-bridge.conf
        fi
        if [ -n "$STATE_MODULES_BACKUP" ]; then
            if [ -f "$STATE_MODULES_BACKUP" ]; then
                mv -f "$STATE_MODULES_BACKUP" /etc/modules-load.d/lightstack.conf
            else
                log_warning "backup $STATE_MODULES_BACKUP is gone; the previous /etc/modules-load.d/lightstack.conf is lost"
            fi
        else
            rm -f /etc/modules-load.d/lightstack.conf
        fi
        log_warning "Sysctl keys this run set that the restored file does not list stay live until reboot."
    fi

    if [ "$STATE_NETPLAN" -eq 1 ]; then
        log_info "Reverting netplan bridge configuration..."
        rm -f /etc/netplan/01-lightstack-bridge.yaml
        if [ -n "$STATE_NETPLAN_BACKUP" ]; then
            if [ -f "$STATE_NETPLAN_BACKUP" ]; then
                log_info "Restoring the previous /etc/netplan/01-lightstack-bridge.yaml..."
                mv -f "$STATE_NETPLAN_BACKUP" /etc/netplan/01-lightstack-bridge.yaml
            else
                log_warning "backup $STATE_NETPLAN_BACKUP is gone; the previous bridge config is lost"
            fi
        fi
        netplan apply > /dev/null 2>&1 \
            || log_warning "netplan apply failed during rollback -- check networking by hand"
    fi

    if [ "$STATE_DB_PW_CHANGED" -eq 1 ]; then
        log_warning "The password of the pre-existing PostgreSQL role lightstack was changed and NOT reverted."
    fi
    log_warning "Installed packages (docker, libvirt, postgresql) were left in place."
    log_warning "Storage directories under $STORAGE_DIR were left in place."
    log_warning "Rollback complete."
}

on_exit() {
    local rc="$1"
    trap - ERR EXIT
    set +e
    if [ "$rc" -eq 0 ]; then
        # Success: drop the pre-existing-config backup rather than leaving the old JWT secret
        # and DB password sitting in $CONFIG_DIR.lightstack-bak.*
        [ -n "$STATE_CONFIG_BACKUP" ] && rm -rf "$STATE_CONFIG_BACKUP"
        return 0
    fi
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

# LIGHTSTACK_DNS_SERVERS is comma-separated, and "8.8.8.8, 1.1.1.1" is the natural thing to type, so
# entries are trimmed and empty ones dropped. Both the validator and the netplan renderer read
# resolvers through here, so neither can disagree with the other about what one entry is.
dns_entries() {
    local entry
    local IFS=','
    for entry in $DNS_SERVERS; do
        entry="${entry#"${entry%%[![:space:]]*}"}"
        entry="${entry%"${entry##*[![:space:]]}"}"
        [ -n "$entry" ] && printf '%s\n' "$entry"
    done
    return 0
}

# Netplan wants one YAML list item per resolver.
render_dns_yaml() {
    local entry sep="" out=""
    while IFS= read -r entry; do
        out+="${sep}          - ${entry}"
        sep=$'\n'
    done < <(dns_entries)
    printf '%s' "$out"
}

validate_dns_servers() {
    local entry found=0
    while IFS= read -r entry; do
        found=1
        is_ipv4 "$entry" || die "LIGHTSTACK_DNS_SERVERS contains an invalid IPv4 address: '$entry'"
    done < <(dns_entries)
    [ "$found" -eq 1 ] || die "LIGHTSTACK_DNS_SERVERS contains no resolvers: '$DNS_SERVERS'"
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

    [ -n "$HOST_IP" ] || { log_error "LIGHTSTACK_HOST_IP is required"; missing=1; }
    [ -n "$NETWORK_IFACE" ] || { log_error "LIGHTSTACK_NETWORK_IFACE is required"; missing=1; }
    [ -n "$GATEWAY" ] || { log_error "LIGHTSTACK_GATEWAY is required"; missing=1; }
    if [ "$missing" -ne 0 ]; then
        usage
        die "refusing to run without explicit network configuration"
    fi

    is_ipv4 "$HOST_IP" || die "LIGHTSTACK_HOST_IP is not a valid IPv4 address: $HOST_IP"
    is_ipv4 "$GATEWAY" || die "LIGHTSTACK_GATEWAY is not a valid IPv4 address: $GATEWAY"

    [[ "$NETMASK" =~ ^[0-9]+$ ]] && [ "$NETMASK" -ge 1 ] && [ "$NETMASK" -le 32 ] \
        || die "LIGHTSTACK_NETMASK must be a prefix length between 1 and 32: $NETMASK"

    validate_dns_servers

    ip link show "$NETWORK_IFACE" > /dev/null 2>&1 \
        || die "network interface $NETWORK_IFACE not found on this host"

    # The password is interpolated into an SQL string literal and a postgres:// DSN,
    # so restrict it to characters that need no quoting or percent-encoding.
    if [ -n "$DB_PASSWORD" ] && ! [[ "$DB_PASSWORD" =~ ^[A-Za-z0-9._~-]{12,}$ ]]; then
        die "LIGHTSTACK_DB_PASSWORD must be 12+ characters from [A-Za-z0-9._~-]"
    fi

    log_success "Configuration validated"
}

generate_secrets() {
    JWT_SECRET="$(openssl rand -hex 32)"
    HORIZON_SECRET_KEY="$(openssl rand -hex 32)"
    if [ -z "$DB_PASSWORD" ]; then
        DB_PASSWORD="$(openssl rand -hex 16)"
        log_info "Generated a PostgreSQL password for role lightstack (written to $CONFIG_DIR/lightstack.yaml)"
    fi
}

# Banner
show_banner() {
    cat << "EOF"
╔═══════════════════════════════════════════════════════════╗
║                                                           ║
║   Lightstack Turnkey Deployment                           ║
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
    if [ -f /etc/netplan/01-lightstack-bridge.yaml ]; then
        STATE_NETPLAN_BACKUP="/etc/netplan/01-lightstack-bridge.yaml.lightstack-bak.$$"
        cp -p /etc/netplan/01-lightstack-bridge.yaml "$STATE_NETPLAN_BACKUP"
    fi

    # Create netplan configuration
    record STATE_NETPLAN
    cat > /etc/netplan/01-lightstack-bridge.yaml <<EOF
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
        # Preserve any existing drop-ins so rollback() can put them back. Without this a rerun
        # overwrites them and rollback then deletes the originals -- the host would come up
        # without ip_forward/bridge-nf at the NEXT reboot, long after the failed run.
        if [ -f /etc/modules-load.d/lightstack.conf ]; then
            STATE_MODULES_BACKUP="/etc/modules-load.d/lightstack.conf.lightstack-bak.$$"
            cp -p /etc/modules-load.d/lightstack.conf "$STATE_MODULES_BACKUP"
        fi
        if [ -f /etc/sysctl.d/99-lightstack-bridge.conf ]; then
            STATE_SYSCTL_BACKUP="/etc/sysctl.d/99-lightstack-bridge.conf.lightstack-bak.$$"
            cp -p /etc/sysctl.d/99-lightstack-bridge.conf "$STATE_SYSCTL_BACKUP"
        fi

        record STATE_SYSCTL
        echo "br_netfilter" > /etc/modules-load.d/lightstack.conf

        cat > /etc/sysctl.d/99-lightstack-bridge.conf <<EOF
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
EOF
        sysctl -p /etc/sysctl.d/99-lightstack-bridge.conf > /dev/null 2>&1
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
    if [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'lightstack'")" = "1" ]; then
        log_warning "PostgreSQL role lightstack already exists; updating its password to match the generated config"
        sudo -u postgres psql -q -c "ALTER ROLE lightstack WITH ENCRYPTED PASSWORD '$DB_PASSWORD';"
        record STATE_DB_PW_CHANGED
    else
        sudo -u postgres psql -q -c "CREATE ROLE lightstack LOGIN ENCRYPTED PASSWORD '$DB_PASSWORD';"
        record STATE_DB_ROLE_CREATED
    fi

    if [ "$(sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname = 'lightstack'")" = "1" ]; then
        log_warning "Database lightstack already exists; leaving its contents untouched"
    else
        sudo -u postgres psql -q -c 'CREATE DATABASE lightstack OWNER lightstack;'
        record STATE_DB_CREATED
    fi
    sudo -u postgres psql -q -c 'GRANT ALL PRIVILEGES ON DATABASE lightstack TO lightstack;'

    # Allow local connections
    PG_VERSION=$(psql --version | awk '{print $3}' | cut -d. -f1)
    PG_HBA="/etc/postgresql/$PG_VERSION/main/pg_hba.conf"

    if [ -f "$PG_HBA" ]; then
        if ! grep -q "host.*lightstack.*lightstack.*127.0.0.1" "$PG_HBA"; then
            local backup="$PG_HBA.lightstack-bak.$$"
            cp -p "$PG_HBA" "$backup"
            record STATE_PGHBA_BACKUP "$backup"
            echo "host    lightstack    lightstack    127.0.0.1/32    md5" >> "$PG_HBA"
            systemctl restart postgresql
        fi
    fi

    log_success "PostgreSQL configured"
}

# Create storage directories
create_storage() {
    log_info "Creating storage directories..."

    # The subdirs the chart's init-host-storage initContainer creates: not every service
    # creates its own, and one that finds its subdir missing fails.
    mkdir -p "$STORAGE_DIR"/{images,volumes,instances,cloud-init,dhcp,console-tokens,console-sockets}
    chmod 755 "$STORAGE_DIR"/{images,volumes,instances,cloud-init,dhcp,console-tokens}

    log_success "Storage directories created at $STORAGE_DIR"
}

# Generate Lightstack configuration
generate_config() {
    log_info "Generating Lightstack configuration..."

    # The files below are written unconditionally, so a pre-existing config is preserved the
    # same way pg_hba and netplan are — without this, a later failure would lose the previous
    # JWT secret and DB password for good. A directory we created ourselves is simply removed.
    if [ -d "$CONFIG_DIR" ]; then
        STATE_CONFIG_BACKUP="$CONFIG_DIR.lightstack-bak.$$"
        rm -rf "$STATE_CONFIG_BACKUP"
        cp -a "$CONFIG_DIR" "$STATE_CONFIG_BACKUP"
        record STATE_CONFIG_BACKUP "$STATE_CONFIG_BACKUP"
    else
        record STATE_CONFIG
    fi
    mkdir -p "$CONFIG_DIR"

    # Lightstack main configuration
    cat > "$CONFIG_DIR/lightstack.yaml" <<EOF
# Node-local state root. The compose file mounts it at this same path, because libvirtd on
# the host opens the paths Lightstack writes into the domain XML.
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
  node_name: "$(hostname)"
  tunnel_ip: "$HOST_IP"
EOF

    # Copy Horizon configuration from repo and modify for host networking
    if [ ! -d "$REPO_DIR/deployments/horizon-config" ]; then
        die "Horizon config not found at $REPO_DIR/deployments/horizon-config (set LIGHTSTACK_REPO_DIR)"
    fi

    # Create modified local_settings for host networking (memcached on HOST_IP)
    log_info "Creating Horizon configuration for host networking..."
    mkdir -p "$CONFIG_DIR/horizon-config"

    # Copy local_settings and change memcached location for host networking
    # Use HOST_IP instead of localhost since containers have separate network namespaces
    sed "s/memcached:11211/$HOST_IP:11211/g" \
        "$REPO_DIR/deployments/horizon-config/local_settings" > "$CONFIG_DIR/horizon-config/local_settings"

    # Also fix OPENSTACK_HOST to use HOST_IP for host networking
    sed -i "s/OPENSTACK_HOST = \"lightstack\"/OPENSTACK_HOST = \"$HOST_IP\"/g" "$CONFIG_DIR/horizon-config/local_settings"

    # No noVNC rewrite here, on purpose. Horizon has no setting for the console proxy URL --
    # upstream documents CONSOLE_TYPE only, and the URL arrives in Nova's os-getVNCConsole /
    # remote-consoles response, which Lightstack builds from nova.console_proxy_base_url in
    # lightstack.yaml (written above), so nothing edited into local_settings can change it.
    # A `sed s/novnc:6080/.../` used to sit here; that token never existed in
    # deployments/horizon-config/local_settings, so it never matched anything.

    # AVAILABLE_REGIONS carries its own literal Keystone URL (the compose service name), which the
    # OPENSTACK_HOST rewrite above does not reach. With it left alone Horizon's login page offers a
    # region whose URL does not resolve on a host-networked deployment. Derive it from
    # OPENSTACK_KEYSTONE_URL instead, as deployments/horizon-chart does via `make horizon-chart-sync`,
    # so it follows OPENSTACK_HOST automatically and cannot drift again.
    sed -i "s|('http://lightstack:35357/v3', 'RegionOne'),|(OPENSTACK_KEYSTONE_URL, 'RegionOne'),|g" \
        "$CONFIG_DIR/horizon-config/local_settings"

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
    container_name: lightstack-memcached
    hostname: memcached
    network_mode: host
    command: memcached -m 64 -l 0.0.0.0
    restart: unless-stopped

  # Lightstack OpenStack Services with KVM
  lightstack:
    build:
      context: ..
      dockerfile: build/package/Dockerfile
    image: lightstack:latest
    container_name: lightstack
    privileged: true
    network_mode: host
    volumes:
      # The socket's directory, not the socket: a bind of the file keeps its inode, so a
      # libvirtd that re-creates its socket would stay unreachable until a restart.
      - /var/run/libvirt:/var/run/libvirt
      # The whole state root, at the same path: it is the binary's state_path, and libvirtd
      # on the host opens the paths Lightstack writes into the domain XML (disks, cloud-init
      # ISOs, console logs). The novnc proxy below reads console-tokens from it too.
      - $STORAGE_DIR:$STORAGE_DIR
      - $CONFIG_DIR:/opt/lightstack/config:ro
    command: [/app/lightstack, -config, /opt/lightstack/config/lightstack.yaml]
    restart: unless-stopped
    healthcheck:
      test: [CMD-SHELL, "curl -f http://localhost:35357/v3 || exit 1"]
      interval: 10s
      timeout: 5s
      retries: 3

  # noVNC console proxy (SPEC-021) -- the recipe of charts/lightstack/templates/console-proxy.yaml.
  # websockify serves the noVNC client and resolves each ?token= against the entries Lightstack
  # writes to $STORAGE_DIR/console-tokens; each entry names the guest's VNC unix socket.
  # Host networking so it answers on the host's own address at port 6080.
  #   entrypoint, not command: the image's default runs novnc_proxy against ONE fixed target,
  #     and compose's command replaces only its arguments
  #   TokenFile, not ReadOnlyTokenFile: the read-only plugin caches, and would keep serving
  #     revoked tokens
  #   no --host-token: it would let the client pick the target instead of the token
  #   pinned to 1.3.0's digest, as the chart is: the tag is mutable (digest only, because
  #     Podman rejects tag@digest)
  novnc:
    image: bonigarcia/novnc@sha256:a5be468dc8967a55ffe870e809fa36dbbfc19b67d8156c1e7cb2d26aaa4f32c2
    container_name: lightstack-novnc
    hostname: novnc
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
    restart: unless-stopped
    healthcheck:
      # The client page, not a TCP connect: the kernel accepts from the listen backlog even
      # when websockify is wedged.
      test: [CMD, python3, -c, "import urllib.request; urllib.request.urlopen('http://127.0.0.1:6080/vnc_lite.html', timeout=4)"]
      interval: 30s
      timeout: 5s
      retries: 3

  # OpenStack Horizon Dashboard (using working config from repo)
  horizon:
    image: quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble
    container_name: lightstack-horizon
    hostname: horizon
    network_mode: host
    depends_on:
      lightstack:
        condition: service_healthy
      novnc:
        condition: service_started
    environment:
      - KOLLA_INSTALL_TYPE=source
      - KOLLA_CONFIG_STRATEGY=COPY_ALWAYS
      # Shared by all 3 apache workers (horizon-nolist.conf: processes=3). Without it
      # local_settings falls back to /tmp/.horizon_secret_key, which kolla creates as root
      # and the horizon-user workers cannot read. See the secrets block at the top.
      - SECRET_KEY=$HORIZON_SECRET_KEY
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
    log_info "Building Lightstack image (this may take several minutes)..."

    cd "$REPO_DIR/deployments"

    docker compose -f docker-compose-turnkey.yml build --quiet
    log_success "Lightstack image built"

    log_info "Starting services..."
    record STATE_COMPOSE
    docker compose -f docker-compose-turnkey.yml up -d

    log_info "Waiting for services to be ready..."
    sleep 10

    # Wait for Lightstack to be healthy
    for i in {1..30}; do
        if curl -sf http://localhost:35357/v3 > /dev/null 2>&1; then
            log_success "Lightstack services are ready"
            break
        fi
        if [ "$i" -eq 30 ]; then
            docker compose -f docker-compose-turnkey.yml logs lightstack
            die "Lightstack failed to start within 5 minutes"
        fi
        sleep 10
    done

    # Wait for Horizon to be ready
    log_info "Waiting for Horizon dashboard..."
    for i in {1..30}; do
        if curl -sf http://localhost:80/dashboard/auth/login/ > /dev/null 2>&1; then
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

    cat > /root/.lightstack-env <<EOF
# Lightstack OpenStack Environment
export OS_AUTH_URL=http://$HOST_IP:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF

    # Add to bashrc if not already there
    if ! grep -q "source /root/.lightstack-env" /root/.bashrc; then
        echo "source /root/.lightstack-env" >> /root/.bashrc
    fi

    log_success "CLI environment configured"
}

# Show completion message
show_completion() {
    log_success "Lightstack deployment complete!"
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
    echo "🖥️  Instance Console (noVNC):"
    echo "   Horizon: Project → Compute → Instances → <server> → Console"
    echo "   CLI:     openstack console url show <server>"
    echo "   Each link is per instance and expires after 10 minutes;"
    echo "   http://$HOST_IP:6080 on its own opens no console."
    echo ""
    echo "🔧 OpenStack CLI:"
    echo "   source /root/.lightstack-env"
    echo "   openstack token issue"
    echo "   openstack server list"
    echo ""
    echo "📊 Service Status:"
    echo "   docker compose -f $REPO_DIR/deployments/docker-compose-turnkey.yml ps"
    echo ""
    echo "📋 Logs:"
    echo "   docker compose -f $REPO_DIR/deployments/docker-compose-turnkey.yml logs -f"
    echo ""
    echo "Network Configuration:"
    echo "   Bridge: br-ext"
    echo "   Interface: $NETWORK_IFACE"
    echo "   Host IP: $HOST_IP/$NETMASK"
    echo "   Gateway: $GATEWAY"
    echo ""
    echo "⚡ Quick Test:"
    echo "   source /root/.lightstack-env"
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
