# Unified Lightstack + Horizon Deployment Guide

**📚 Complete Documentation**: See **[INDEX.md](INDEX.md)** for full documentation index with learning paths.


**lightstack Version**: Unreleased (post-v0.6.1)
**OpenStack Release**: Flamingo (2025.2)
**Last Updated**: 2026-10-12
**Status**: Feature-complete for the covered surface; see [the comprehensive review report](review/comprehensive-review-report-2026-10-12.md) for the issues that must be fixed before the v0.7.0 release

## Overview

This guide provides a complete, unified deployment of Lightstack and OpenStack Horizon dashboard in a single Docker Compose stack. This is the **recommended deployment method** for getting a complete OpenStack environment with web UI.

### What's Included

- **PostgreSQL 18**: Database backend
- **lightstack Services**: All five core OpenStack services (Keystone, Nova, Neutron, Cinder, Glance)
- **Horizon Dashboard**: OpenStack Flamingo (2025.2) web interface
- **memcached**: Horizon session cache

> This is the **stub-mode** stack: no real VMs and **no console proxy** — the noVNC
> proxy is deployed by the Helm chart in real mode. Console access from the CLI
> (`openstack console log show`) still works.

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Docker Network (lightstack-horizon-network)      │
├─────────────────────────────────────────────────────────────┤
│                                                               │
│  ┌──────────┐   ┌────────────────┐   ┌─────────────────┐  │
│  │PostgreSQL│◄──│  lightstack Services  │◄──│     Horizon     │  │
│  │  :5432   │   │   :35357-9292  │   │      :80        │  │
│  └──────────┘   └────────────────┘   └─────────────────┘  │
│                          ▲                                   │
│                          │                                   │
│                  ┌───────┴────────┐                         │
│                  │   memcached    │                         │
│                  │     :11211     │                         │
│                  └────────────────┘                         │
└─────────────────────────────────────────────────────────────┘
         │              │              │
   (internal —     Port 35357    Port 80
    not exposed)   (+ 8774,      (HTTP)
                    9696, 8776,
                    9292, 8775)
```

> PostgreSQL (`:5432`) is network-internal in this stack — the compose file
> never publishes it to the host; only the Lightstack API ports and Horizon's
> port 80 are exposed.

## Prerequisites

### Required

- **Docker Engine** 20.10+
- **Docker Compose** 2.0+
- **Available Ports**:
  - 80 (HTTP - Horizon dashboard)
  - 35357 (Keystone)
  - 8774 (Nova)
  - 8775 (Metadata)
  - 8776 (Cinder)
  - 9292 (Glance)
  - 9696 (Neutron)

  (PostgreSQL runs network-internal — no host port needed.)
- **System Resources**:
  - 4GB RAM minimum (8GB recommended)
  - 20GB disk space minimum
  - 2 CPU cores minimum (4 recommended)

### Platform Support

- ✅ **Linux x86_64 (AMD64)**: Full support (all services)
- ⚠️ **Linux ARM64**: Lightstack works natively; Horizon runs via emulation (slower)
- ✅ **macOS Intel**: Full support (Docker Desktop with Rosetta)
- ⚠️ **macOS Apple Silicon (ARM64)**: Lightstack works natively; Horizon requires emulation
  - **Note**: Horizon is an x86_64 image and will run under emulation
  - Lightstack itself is built as ARM64 and runs natively
  - For ARM64-only environments, use API-only deployment (see Alternative Deployment Options below)
- ⚠️ **Windows**: WSL2 required

## Quick Start (5 Minutes)

### Step 1: Clone Repository

```bash
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack/deployments
```

### Step 2: Review Configuration (Optional)

The deployment includes pre-configured settings suitable for development and testing:

```bash
# View Docker Compose configuration
cat docker-compose-horizon.yml

# View Horizon configuration
cat horizon-config/local_settings
```

**Default Settings**:
- Database: PostgreSQL on port 5432
- Lightstack Mode: Stub (no real VMs, suitable for testing)
- Horizon: Flamingo (2025.2) on port 80
- Default Credentials: admin/secret

### Step 3: Start All Services

The compose file **requires** a JWT secret — it refuses to start without one:

```bash
export LIGHTSTACK_JWT_SECRET=$(openssl rand -base64 32)
docker compose -f docker-compose-horizon.yml up -d
```

**Expected Output**:
```
[+] Running 5/5
 ✔ Network lightstack-horizon-network    Created
 ✔ Container lightstack-postgres         Started
 ✔ Container lightstack-memcached        Started
 ✔ Container lightstack                  Started
 ✔ Container lightstack-horizon          Started
```

### Step 4: Wait for Services to Be Ready

```bash
# Check container status
docker compose -f docker-compose-horizon.yml ps

# Watch logs until Horizon is ready
docker compose -f docker-compose-horizon.yml logs -f horizon

# Press Ctrl+C when you see "WSGI app 0 (mountpoint='/dashboard') ready"
```

Wait time: ~30-60 seconds depending on your system.

### Step 5: Access Horizon Dashboard

Open your browser to:
```
http://localhost/dashboard
```

**Login Credentials**:
- Domain: `Default`
- User Name: `admin`
- Password: `secret`

🎉 **You're done!** You now have a fully functional OpenStack environment with web UI.

## Alternative Deployment Options

### API-Only Deployment (ARM64 Native)

If you're on ARM64 (Apple Silicon, ARM servers) and want to avoid emulation overhead, use the API-only deployment which runs Lightstack natively:

```bash
# Use the standard docker-compose without Horizon
docker compose -f docker-compose.yml up -d

# Access via OpenStack CLI
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_DOMAIN_NAME=Default
openstack token issue
```

This gives you full OpenStack API access with native ARM64 performance, without the web UI.

### Native Horizon (x86_64 Only)

For x86_64 systems or when performance is critical, the unified deployment (`docker-compose-horizon.yml`) provides the best experience with native performance for all components.

## Detailed Configuration

### Environment Variables

You can customize the deployment by setting environment variables before starting:

```bash
# Database configuration — read by docker-compose-horizon.yml: the POSTGRES_*
# variables initialise the postgres container (initdb on an empty volume) and
# are interpolated into the lightstack container's LIGHTSTACK_DB_URL.
# The lightstack binary itself only reads the full LIGHTSTACK_DB_URL string.
export POSTGRES_PASSWORD=your-secure-password
export POSTGRES_DB=lightstack
export POSTGRES_USER=lightstack

# JWT secret for lightstack authentication (required by docker-compose-horizon.yml)
export LIGHTSTACK_JWT_SECRET=your-secure-jwt-secret-here

# Horizon configuration
export HORIZON_SECRET_KEY=your-horizon-secret-key
```

Service modes (stub/real, storage backends) are **not** controlled by environment variables — they are set in `config/lightstack.yaml` (`nova.libvirt_mode`, `neutron.networking_mode`, `cinder.storage_mode`, `glance.storage_mode`); mount a custom copy as shown below.

### Production Configuration

For production deployments, update these settings:

#### 1. Security Settings

**Database Password** - Edit `docker-compose-horizon.yml`:

```yaml
services:
  postgres:
    environment:
      POSTGRES_PASSWORD: CHANGE_THIS_PASSWORD

  lightstack:
    environment:
      - LIGHTSTACK_DB_URL=postgres://lightstack:CHANGE_THIS_PASSWORD@postgres:5432/lightstack?sslmode=disable
```

**JWT Secret** - Edit `docker-compose-horizon.yml`:

```yaml
services:
  lightstack:
    environment:
      - LIGHTSTACK_JWT_SECRET=GENERATE_SECURE_RANDOM_STRING_HERE
```

Generate a secure JWT secret:
```bash
openssl rand -hex 32
```

**Horizon Secret Key** - Edit `horizon-config/local_settings`:

```python
import secrets
SECRET_KEY = secrets.token_hex(64)
```

#### 2. Enable HTTPS

Use a reverse proxy (nginx, traefik) with SSL certificates:

```yaml
# Add to docker-compose-horizon.yml
services:
  nginx:
    image: nginx:alpine
    container_name: lightstack-nginx
    ports:
      - "443:443"
      - "80:80"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
      - ./certs:/etc/nginx/certs:ro
    networks:
      - lightstack-network
    depends_on:
      - horizon
```

Update `horizon-config/local_settings`:

```python
ALLOWED_HOSTS = ['your-domain.com']
SESSION_COOKIE_SECURE = True
CSRF_COOKIE_SECURE = True
```

#### 3. Real Mode (Linux Only)

For production with actual VMs, set the modes in `lightstack.yaml` and mount it into the container (there are no environment variables for these settings):

```yaml
# config/lightstack.yaml
nova:
  libvirt_mode: real        # "stub" or "real"
neutron:
  networking_mode: iptables  # "stub", "iptables", or "ebpf"
```

```yaml
# docker-compose-horizon.yml
services:
  lightstack:
    volumes:
      - ../config/lightstack.yaml:/app/config/lightstack.yaml:ro
      - /var/run/libvirt:/var/run/libvirt   # the socket's directory: survives a re-created socket
    privileged: true  # Required for networking
```

**Prerequisites**:
- libvirt + KVM installed on host
- User has permissions for libvirt socket

### Storage Backends

Storage modes are configured in `config/lightstack.yaml` (`cinder.storage_mode`, `glance.storage_mode`), not via environment variables. Mount your adjusted `lightstack.yaml` into the lightstack container as shown in [Real Mode](#3-real-mode-linux-only).

#### Local Storage (Default)

Suitable for development and small deployments:

```yaml
# config/lightstack.yaml
cinder:
  storage_mode: local  # "stub", "local", "rbd", or "local,rbd"
glance:
  storage_mode: local  # "stub", "local", "s3", or "local,s3"
```

```yaml
# docker-compose-horizon.yml
services:
  lightstack:
    volumes:
      - lightstack-images:/var/lib/lightstack/images
      - lightstack-volumes:/var/lib/lightstack/volumes
```

#### Ceph RBD Storage

For production with a Ceph cluster, use Cinder RBD. Cinder supports `rbd` and `local,rbd` (`cinder.storage_mode`); Glance accepts `rbd`/`local,rbd` values but its RBD operations are **unimplemented placeholders** — do not use RBD for Glance. For shared image storage, prefer Glance S3 backed by Ceph RGW (see below) together with Cinder RBD:

```yaml
# config/lightstack.yaml
cinder:
  storage_mode: rbd
  ceph_pool: volumes
  ceph_conf: /etc/ceph/ceph.conf
```

```yaml
# docker-compose-horizon.yml
services:
  lightstack:
    volumes:
      - /etc/ceph:/etc/ceph:ro
```

#### S3 Storage (Glance)

For AWS S3, MinIO, or Ceph RGW — Glance only (Cinder has no S3 backend):

```yaml
# docker-compose-horizon.yml
services:
  lightstack:
    environment:
      - LIGHTSTACK_GLANCE_S3_ENDPOINT=https://s3.amazonaws.com
      - LIGHTSTACK_GLANCE_S3_BUCKET=lightstack-images
      - LIGHTSTACK_GLANCE_S3_REGION=us-east-1
      # Credentials are read by the AWS SDK default chain:
      - AWS_ACCESS_KEY_ID=your-access-key
      - AWS_SECRET_ACCESS_KEY=your-secret-key
```

```yaml
# config/lightstack.yaml
glance:
  storage_mode: s3  # or "local,s3"
```

## Operations

### Starting Services

```bash
# Start all services
docker compose -f docker-compose-horizon.yml up -d

# Start specific service
docker compose -f docker-compose-horizon.yml up -d horizon

# Start with logs
docker compose -f docker-compose-horizon.yml up
```

### Stopping Services

```bash
# Stop all services (preserves data)
docker compose -f docker-compose-horizon.yml stop

# Stop and remove containers (preserves volumes)
docker compose -f docker-compose-horizon.yml down

# Stop and remove everything including data
docker compose -f docker-compose-horizon.yml down -v
```

### Viewing Logs

```bash
# All services
docker compose -f docker-compose-horizon.yml logs -f

# Specific service
docker compose -f docker-compose-horizon.yml logs -f horizon
docker compose -f docker-compose-horizon.yml logs -f lightstack

# Last 100 lines
docker compose -f docker-compose-horizon.yml logs --tail 100 horizon
```

### Checking Status

```bash
# Container status
docker compose -f docker-compose-horizon.yml ps

# Health status
docker compose -f docker-compose-horizon.yml ps --format "table {{.Name}}\t{{.Status}}\t{{.Health}}"

# Resource usage
docker stats --no-stream
```

### Restarting Services

```bash
# Restart all
docker compose -f docker-compose-horizon.yml restart

# Restart specific service
docker compose -f docker-compose-horizon.yml restart horizon

# Rebuild and restart (after config changes)
docker compose -f docker-compose-horizon.yml up -d --build lightstack
```

### Accessing Container Shells

```bash
# lightstack container
docker exec -it lightstack /bin/sh

# Horizon container
docker exec -it lightstack-horizon /bin/bash

# PostgreSQL container
docker exec -it lightstack-postgres psql -U lightstack
```

## Using OpenStack CLI

### Installing OpenStack CLI

```bash
# macOS
brew install openstackclient

# Linux (Ubuntu/Debian)
sudo apt install python3-openstackclient

# Using pip
pip install python-openstackclient
```

### Configure Environment

```bash
# Option 1: Export variables
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3

# Option 2: Create RC file
cat > ~/lightstack-openrc.sh <<'EOF'
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF

source ~/lightstack-openrc.sh
```

### Test Commands

```bash
# Test authentication
openstack token issue

# List endpoints
openstack catalog list

# Compute operations
openstack flavor list
openstack image list
openstack server list

# Network operations
openstack network list
openstack subnet list
openstack router list

# Volume operations
openstack volume list
openstack volume snapshot list
```

## Verification and Testing

### 1. Health Checks

```bash
# Check all containers are healthy
docker compose -f docker-compose-horizon.yml ps

# Expected: All containers show "healthy" status
```

### 2. API Endpoints

```bash
# Keystone (Identity)
curl -s http://localhost:35357/v3 | jq

# Nova (Compute)
curl -s http://localhost:8774/ | jq

# Neutron (Network)
curl -s http://localhost:9696/ | jq

# Cinder (Block Storage)
curl -s http://localhost:8776/ | jq

# Glance (Image)
curl -s http://localhost:9292/ | jq
```

### 3. Horizon Dashboard

1. Open http://localhost/dashboard
2. Login with admin/secret
3. Check "Overview" page loads
4. Verify "Compute > Instances" shows empty list
5. Click "Launch Instance" to test wizard
6. Check "Network > Network Topology" loads

### 4. Full Workflow Test

```bash
# Source environment
source ~/lightstack-openrc.sh

# Create network
openstack network create test-net
openstack subnet create test-subnet \
  --network test-net \
  --subnet-range 10.0.0.0/24

# Create instance (stub mode returns fake instance)
openstack server create \
  --flavor m1.small \
  --image cirros \
  --network test-net \
  test-vm

# List instances
openstack server list

# Create volume
openstack volume create --size 1 test-volume

# List volumes
openstack volume list

# Cleanup
openstack server delete test-vm
openstack volume delete test-volume
openstack subnet delete test-subnet
openstack network delete test-net
```

## Troubleshooting

### Platform-Specific Issues

#### ARM64 (Apple Silicon, ARM Servers)

**Symptom**: Horizon container restarts repeatedly

**Diagnosis**:
```bash
docker compose -f docker-compose-horizon.yml ps
# Look for "platform (linux/amd64) does not match" warnings
```

**Explanation**: Horizon (OpenStack Kolla) is an x86_64 image. On ARM64 systems, Docker runs it via emulation (QEMU). This works but may be slower and less stable.

**Solutions**:
1. **Continue with emulation** (works but slower):
   - Horizon may take 2-3x longer to start
   - Slightly higher resource usage
   - Fully functional once started

2. **Use API-only deployment** (recommended for ARM64):
   ```bash
   docker compose -f docker-compose.yml up -d
   # Use OpenStack CLI instead of web UI
   ```

3. **Use native ARM64 Horizon** (future):
   - Custom ARM64 Horizon build in development
   - Track progress in the [issue tracker](https://github.com/cobaltcore-dev/lightstack/issues)

**Verification**:
```bash
# Check lightstack (should be ARM64 and healthy)
docker inspect lightstack | jq '.[0].Architecture'  # Should show "arm64"
docker compose -f docker-compose-horizon.yml ps lightstack  # Should be "healthy"

# lightstack works natively on ARM64, only Horizon requires emulation
docker exec lightstack curl -s http://localhost:35357/v3 | jq '.version.status'
```

### Service Won't Start

**Symptom**: Container exits immediately after start

**Diagnosis**:
```bash
docker compose -f docker-compose-horizon.yml logs lightstack
```

**Common Issues**:

1. **Port already in use**:
   ```
   Error: bind: address already in use
   ```
   Solution: Change port mapping in docker-compose-horizon.yml or stop conflicting service

2. **Database connection failed**:
   ```
   Error: failed to connect to postgres
   ```
   Solution: Ensure postgres container is healthy:
   ```bash
   docker compose -f docker-compose-horizon.yml ps postgres
   docker compose -f docker-compose-horizon.yml logs postgres
   ```

3. **Migration failed**:
   ```
   Error: migration failed
   ```
   Solution: Reset database:
   ```bash
   docker compose -f docker-compose-horizon.yml down -v
   docker compose -f docker-compose-horizon.yml up -d
   ```

### Horizon Shows Connection Errors

**Symptom**: "Unable to retrieve..." errors in Horizon

**Diagnosis**:
```bash
# Check Horizon can reach lightstack
docker exec lightstack-horizon curl -s http://lightstack:35357/v3 | jq

# Check network connectivity
docker exec lightstack-horizon ping -c 3 lightstack
```

**Solutions**:

1. **Verify Lightstack is running**:
   ```bash
   docker compose -f docker-compose-horizon.yml ps lightstack
   curl -s http://localhost:35357/v3 | jq
   ```

2. **Check Horizon configuration**:
   ```bash
   docker exec lightstack-horizon cat /etc/openstack-dashboard/local_settings.py | grep OPENSTACK_HOST
   # Should show: OPENSTACK_HOST = "lightstack"
   ```

3. **Restart Horizon**:
   ```bash
   docker compose -f docker-compose-horizon.yml restart horizon
   ```

### VNC Console Doesn't Work

**Expected in this stack**: the compose stack is stub mode and ships **no console
proxy** — the Horizon console tab cannot connect to a guest. The noVNC proxy is
deployed by the Helm chart in real mode (see
[KUBERNETES_DEPLOYMENT.md](KUBERNETES_DEPLOYMENT.md)); its tokens are managed with
`lightstack console-tokens list|purge`.

**What still works**: `openstack console log show <server>` returns the (stub)
serial console log.

### Horizon Performance Issues

**Symptom**: Dashboard is slow to load

**Solutions**:

1. **Increase container resources**:
   ```yaml
   services:
     horizon:
       deploy:
         resources:
           limits:
             memory: 4G
             cpus: '2'
   ```

2. **Check system resources**:
   ```bash
   docker stats --no-stream
   ```

3. **Memcached is already enabled**: the stack ships a `lightstack-memcached`
   service (`memcached:1.6-alpine`) on the `lightstack-horizon-network`, and
   Horizon's `local_settings` is configured to use it. Verify it is healthy:
   ```bash
   docker compose -f deployments/docker-compose-horizon.yml ps memcached
   ```

### Database Issues

**Symptom**: Database errors in Lightstack logs

**Diagnosis**:
```bash
docker compose -f docker-compose-horizon.yml logs postgres
docker exec lightstack-postgres psql -U lightstack -c "\dt"
```

**Solutions**:

1. **Check database connectivity**:
   ```bash
   docker exec lightstack-postgres pg_isready -U lightstack
   ```

2. **Verify database schema**:
   ```bash
   docker exec lightstack-postgres psql -U lightstack -c "\dt" | wc -l
   # Should show ~20+ tables
   ```

3. **Reset database** (WARNING: Destroys all data):
   ```bash
   docker compose -f docker-compose-horizon.yml down -v
   docker volume rm deployments_postgres-data
   docker compose -f docker-compose-horizon.yml up -d
   ```

## Upgrading

### Upgrade Lightstack

```bash
# Pull latest code
cd ~/git/lightstack
git pull

# Rebuild lightstack image
cd deployments
docker compose -f docker-compose-horizon.yml build lightstack

# Restart with new image
docker compose -f docker-compose-horizon.yml up -d lightstack
```

### Upgrade Horizon

```bash
# Edit docker-compose-horizon.yml
# Change image version:
# FROM: quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble
# TO:   quay.io/openstack.kolla/horizon:2026.1-ubuntu-noble

# Pull new image
docker compose -f docker-compose-horizon.yml pull horizon

# Restart Horizon
docker compose -f docker-compose-horizon.yml up -d horizon
```

### Database Migration

Lightstack automatically runs migrations on startup. To manually run migrations:

```bash
docker exec lightstack /app/lightstack-migrate up
```

## Backup and Restore

### Backup

```bash
# Create backup directory
mkdir -p ~/lightstack-backups/$(date +%Y%m%d)

# Backup database
docker exec lightstack-postgres pg_dump -U lightstack lightstack > \
  ~/lightstack-backups/$(date +%Y%m%d)/database.sql

# Backup configuration
cp docker-compose-horizon.yml ~/lightstack-backups/$(date +%Y%m%d)/
cp -r horizon-config ~/lightstack-backups/$(date +%Y%m%d)/

# Backup volumes (if using local storage)
docker run --rm \
  -v deployments_lightstack-images:/data \
  -v ~/lightstack-backups/$(date +%Y%m%d):/backup \
  alpine tar czf /backup/images.tar.gz /data

docker run --rm \
  -v deployments_lightstack-volumes:/data \
  -v ~/lightstack-backups/$(date +%Y%m%d):/backup \
  alpine tar czf /backup/volumes.tar.gz /data
```

### Restore

```bash
# Restore database
cat ~/lightstack-backups/20260313/database.sql | \
  docker exec -i lightstack-postgres psql -U lightstack lightstack

# Restore configuration
cp ~/lightstack-backups/20260313/docker-compose-horizon.yml .
cp -r ~/lightstack-backups/20260313/horizon-config .

# Restore volumes
docker run --rm \
  -v deployments_lightstack-images:/data \
  -v ~/lightstack-backups/20260313:/backup \
  alpine tar xzf /backup/images.tar.gz -C /

docker run --rm \
  -v deployments_lightstack-volumes:/data \
  -v ~/lightstack-backups/20260313:/backup \
  alpine tar xzf /backup/volumes.tar.gz -C /

# Restart services
docker compose -f docker-compose-horizon.yml restart
```

## Monitoring

### Container Metrics

```bash
# Real-time stats
docker stats

# Resource limits
docker inspect lightstack --format='{{.HostConfig.Memory}}'
docker inspect lightstack --format='{{.HostConfig.NanoCpus}}'
```

### Application Metrics

```bash
# lightstack metrics endpoint (planned, not yet implemented — no
# Prometheus listener exists as of v0.6.x; see OPERATIONS.md "Monitoring")
# curl http://localhost:9090/metrics

# Database connections
docker exec lightstack-postgres psql -U lightstack -c \
  "SELECT count(*) FROM pg_stat_activity;"

# Horizon request logs
docker compose -f docker-compose-horizon.yml logs horizon | \
  grep "GET /dashboard" | tail -20
```

### Health Monitoring

```bash
# Automated health check script
cat > health-check.sh <<'EOF'
#!/bin/bash
set -e

echo "Checking lightstack services..."
curl -f http://localhost:35357/v3 > /dev/null || exit 1
echo "✓ Keystone OK"

curl -f http://localhost:8774/ > /dev/null || exit 1
echo "✓ Nova OK"

curl -f http://localhost:9696/ > /dev/null || exit 1
echo "✓ Neutron OK"

curl -f http://localhost:80/dashboard/auth/login/ > /dev/null || exit 1
echo "✓ Horizon OK"

echo "All services healthy!"
EOF

chmod +x health-check.sh
./health-check.sh
```

## Production Deployment Checklist

- [ ] Change default passwords (postgres, admin user)
- [ ] Generate secure JWT secret
- [ ] Generate secure Horizon SECRET_KEY
- [ ] Enable HTTPS with valid certificates
- [ ] Configure ALLOWED_HOSTS in Horizon
- [ ] Enable secure cookies (SESSION_COOKIE_SECURE, CSRF_COOKIE_SECURE)
- [ ] Set up backup schedule
- [ ] Configure monitoring and alerting
- [ ] Review and harden firewall rules
- [ ] Set resource limits on containers
- [ ] Configure log rotation
- [ ] Test disaster recovery procedures
- [ ] Document custom configuration
- [ ] Set up high availability (if required)
- [ ] Configure external storage backend (Ceph, S3)
- [ ] Review and apply security updates regularly

## Integration with Existing Infrastructure

### External Database

Use an existing PostgreSQL instance:

```yaml
services:
  lightstack:
    environment:
      - LIGHTSTACK_DB_URL=postgres://user:pass@external-db.example.com:5432/lightstack?sslmode=require

# Remove postgres service
```

### External Load Balancer

Place behind load balancer:

```yaml
services:
  horizon:
    # Remove port mapping
    # ports:
    #   - "80:80"
    expose:
      - "80"
    environment:
      - USE_X_FORWARDED_FOR=True
```

Configure load balancer:
- Backend: http://horizon:80
- Health check: /dashboard/auth/login/
- Session affinity: Enabled

### Container Orchestration

For Kubernetes deployment, see separate guide: [`docs/KUBERNETES_DEPLOYMENT.md`](KUBERNETES_DEPLOYMENT.md)

## Support and Resources

- **Documentation**: https://github.com/cobaltcore-dev/lightstack/tree/main/docs
- **Issues**: https://github.com/cobaltcore-dev/lightstack/issues
- **OpenStack Horizon Docs**: https://docs.openstack.org/horizon/latest/
- **lightstack Project**: https://github.com/cobaltcore-dev/lightstack

## FAQ

**Q: Can I use this in production?**
A: Yes, with proper security hardening (HTTPS, secure passwords, resource limits).

**Q: Does this work on Apple Silicon (M1/M2)?**
A: Yes — lightstack runs natively on ARM64; the Horizon (x86_64 Kolla) container runs under QEMU emulation, which works but is slower. For API/CLI/SDK work, the API-only stack (`deployments/docker-compose.yml`) has no x86_64-only components. See [MULTIARCH.md](MULTIARCH.md).

**Q: Can I run real VMs?**
A: Yes, on Linux with libvirt/KVM. Set `nova.libvirt_mode: real` in `config/lightstack.yaml` (and `neutron.networking_mode` for real networking).

**Q: How do I add more users?**
A: Use OpenStack CLI or Horizon's Identity panel to create users.

**Q: Can I integrate with Active Directory?**
A: No — identity is PostgreSQL-backed and managed via the Identity API or Horizon. The domain-config endpoint returns an empty LDAP stub; no LDAP backend is implemented.

**Q: What's the difference between stub and real mode?**
A: Stub mode returns fake data (for testing). Real mode creates actual VMs and networks.

**Q: How do I upgrade to newer OpenStack release?**
A: Update Horizon image tag in docker-compose-horizon.yml to desired release.

## License

This deployment configuration is part of the Lightstack project and is licensed under Apache License 2.0.

---

**Last Updated**: 2026-10-12 | **lightstack Version**: Unreleased (post-v0.6.1) | **Horizon Version**: Flamingo (2025.2)
