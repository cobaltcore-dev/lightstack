# Lightstack Docker Compose Deployment with KVM Support

This deployment configuration runs Lightstack in a Docker container with access to the host's KVM/libvirt for real virtualization.

## Prerequisites

1. **Linux host** with Ubuntu 24.04/22.04 or Debian 12
2. **KVM and libvirt** installed and running:
   ```bash
   sudo apt install qemu-kvm libvirt-daemon-system libvirt-clients bridge-utils
   sudo systemctl enable --now libvirtd
   ```
3. **Docker and Docker Compose** installed
4. **PostgreSQL 18** running on host (for database)
5. **Storage base directory** — subdirs are auto-created by Lightstack on startup (SPEC-011 / #510);
   this is needed only if Lightstack runs as a non-root user that cannot create `/var/lib/lightstack`:
   ```bash
   sudo install -d -o "$USER" -g "$USER" -m 755 /var/lib/lightstack
   ```
6. **Configuration file** at `/opt/lightstack/config/lightstack.yaml`

## Configuration

Create `/opt/lightstack/config/lightstack.yaml`:

```yaml
database:
  url: "postgres://lightstack:YOUR_PASSWORD@localhost:5432/lightstack?sslmode=disable"
  max_connections: 20

keystone:
  host: "0.0.0.0"
  port: 35357
  jwt_secret: "CHANGE-ME-IN-PRODUCTION"
  token_ttl: 24h

nova:
  host: "0.0.0.0"
  port: 8774
  libvirt_mode: real
  libvirt_uri: "qemu:///system"
  instance_storage_path: "/var/lib/lightstack/instances"
  console_proxy_base_url: "http://YOUR_HOST_IP:6080/vnc_auto.html"

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
  local_storage_path: "/var/lib/lightstack/volumes"

glance:
  host: "0.0.0.0"
  port: 9292
  storage_mode: local
  local_storage_path: "/var/lib/lightstack/images"

metadata:
  host: "0.0.0.0"
  port: 8775

compute:
  node_id: "compute-node-1"
  node_name: "YOUR_HOSTNAME"
  tunnel_ip: "YOUR_HOST_IP"
```

## Database Setup

Create the PostgreSQL database and user:

```bash
sudo -u postgres psql << EOF
CREATE DATABASE lightstack;
CREATE USER lightstack WITH ENCRYPTED PASSWORD 'YOUR_PASSWORD';
GRANT ALL PRIVILEGES ON DATABASE lightstack TO lightstack;
ALTER DATABASE lightstack OWNER TO lightstack;
EOF
```

## Build and Deploy

1. **Build the Lightstack image**:
   ```bash
   cd /path/to/lightstack
   docker compose -f deployments/docker-compose-kvm.yml build
   ```

2. **Start lightstack**:
   ```bash
   docker compose -f deployments/docker-compose-kvm.yml up -d
   ```

3. **Check logs**:
   ```bash
   docker logs lightstack -f
   ```

4. **Verify services**:
   ```bash
   curl http://localhost:35357/v3
   ```

## Architecture

This deployment uses:

- **Host networking** (`network_mode: host`): Allows Lightstack to bind directly to host ports
- **Privileged mode**: Required for network namespace operations
- **Libvirt socket mount**: `/var/run/libvirt/libvirt-sock` for KVM access
- **Storage directories**: Mounted from host for persistence
- **Config mount**: `/opt/lightstack/config` (read-only)

## Why Not Pure Docker Compose?

The standard `docker-compose.yml` runs Lightstack in a container without libvirt access, suitable for:
- Development with stub mode
- Testing without real VMs
- macOS/Windows development

This `docker-compose-kvm.yml` is specifically for:
- Production deployment with real VMs
- Single-node KVM deployment
- Linux hosts with KVM support

## Troubleshooting

### Libvirt Permission Denied

If you see "permission denied" for libvirt socket:

```bash
# Add docker user to libvirt group
sudo usermod -a -G libvirt $(whoami)

# Or run container with user that has libvirt access
# Modify docker-compose-kvm.yml:
# user: "1000:libvirt"
```

### Database Connection Issues

Check PostgreSQL is listening on localhost:

```bash
ss -tulpn | grep 5432
```

Verify connection from container:

```bash
docker exec lightstack psql -h localhost -U lightstack -d lightstack -c "SELECT version();"
```

### Services Not Starting

Check all prerequisites are met:

```bash
# Check libvirt
systemctl status libvirtd

# Check PostgreSQL
systemctl status postgresql

# Check storage directories
ls -la /var/lib/lightstack/
```

## Stopping and Cleanup

```bash
# Stop lightstack
docker compose -f deployments/docker-compose-kvm.yml down

# Remove image
docker rmi deployments-lightstack

# Clean storage (CAUTION: Deletes all VMs, volumes, images)
sudo rm -rf /var/lib/lightstack/*
```

## See Also

- [Single-Node Deployment Guide](../docs/SINGLE_NODE_DEPLOYMENT.md) - Automated deployment script
- [Scaling Guide](../docs/SCALING.md) - Multi-node production deployment
- [Configuration Reference](../docs/CONFIGURATION.md) - Complete configuration options
