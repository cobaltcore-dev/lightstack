# Installation Guide

**📚 Complete Documentation**: See **[INDEX.md](INDEX.md)** for full documentation index with learning paths.


Complete installation guide for Lightstack (OpenStack-compatible cloud platform).

---

## Table of Contents

- [Prerequisites](#prerequisites)
- [Installation Methods](#installation-methods)
  - [Docker Compose (Recommended)](#docker-compose-recommended)
  - [Binary Installation](#binary-installation)
- [First Run](#first-run)
- [Verification](#verification)
- [Troubleshooting](#troubleshooting)
- [Next Steps](#next-steps)

---

## Prerequisites

### System Requirements

**Minimum:**
- CPU: 2 cores
- RAM: 4GB
- Disk: 10GB free space
- OS: Linux (Ubuntu 20.04+, Debian 11+, RHEL 8+)

**Recommended:**
- CPU: 4+ cores
- RAM: 8GB+
- Disk: 50GB+ free space
- OS: Ubuntu 22.04 LTS or macOS (ARM64/AMD64)

### Required Software

**For Docker Compose Installation:**
- Docker 20.10+ ([install guide](https://docs.docker.com/engine/install/))
- Docker Compose V2 (included with Docker Desktop)

**For Binary Installation:**
- PostgreSQL 18+ ([install guide](https://www.postgresql.org/download/))
- Go 1.27+ (for building from source)

---

## Installation Methods

### Docker Compose (Recommended)

Docker Compose provides the easiest way to get Lightstack running with all dependencies.

#### Step 1: Clone Repository

```bash
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack
```

#### Step 2: Start Services

```bash
docker compose -f deployments/docker-compose.yml up -d
```

This starts:
- **PostgreSQL** on port 5432
- **Redis** on port 6379
- **lightstack APIs** on ports 35357, 8774, 9696, 8776, 9292, 8775
  (Placement also listens on 8778, but it is not published by compose)

#### Step 3: Wait for Services

```bash
# Check status
docker compose -f deployments/docker-compose.yml ps

# Wait for healthy status
docker compose -f deployments/docker-compose.yml logs -f lightstack
```

First startup takes 30-60 seconds as migrations run automatically.

#### Step 4: Configure CLI Environment

```bash
# Create environment file
cat > ~/.lightstack-env << 'EOF'
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF

# Load environment
source ~/.lightstack-env
```

#### Step 5: Install OpenStack CLI

```bash
# Using pipx (recommended)
brew install pipx  # macOS
# or: apt install pipx  # Ubuntu/Debian

pipx install python-openstackclient
```

✅ **Installation Complete!** Jump to [Verification](#verification).

---

### Binary Installation

For advanced users who want to run Lightstack directly without Docker.

#### Step 1: Install PostgreSQL

**Ubuntu/Debian:**
```bash
sudo apt update
sudo apt install postgresql-18 postgresql-client-18
sudo systemctl start postgresql
sudo systemctl enable postgresql
```

**macOS:**
```bash
brew install postgresql@18
brew services start postgresql@18
```

#### Step 2: Create Database

```bash
sudo -u postgres psql << EOF
CREATE DATABASE lightstack;
CREATE USER lightstack WITH PASSWORD 'secret';
GRANT ALL PRIVILEGES ON DATABASE lightstack TO lightstack;
EOF
```

#### Step 3: Build Lightstack

```bash
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack

# Build
go build -o lightstack ./cmd/lightstack
go build -o lightstack-migrate ./cmd/lightstack-migrate

# Move binaries
sudo mv lightstack /usr/local/bin/
sudo mv lightstack-migrate /usr/local/bin/
```

#### Step 4: Configure Lightstack

```bash
# Create config directory
sudo mkdir -p /etc/lightstack

# Copy config
sudo cp config/lightstack.yaml /etc/lightstack/

# Edit config
sudo nano /etc/lightstack/lightstack.yaml
```

Update database URL:
```yaml
database:
  url: "postgres://lightstack:secret@localhost/lightstack?sslmode=disable"
```

#### Step 5: Run Migrations

```bash
lightstack-migrate up
```

#### Step 6: Start Lightstack

```bash
# Run in foreground
lightstack --config /etc/lightstack/lightstack.yaml

# Or create systemd service (see docs/OPERATIONS.md)
```

✅ **Installation Complete!** Jump to [Verification](#verification).

---

## First Run

### Default Credentials

```
Domain:   Default
Username: admin
Password: secret
Project:  default
```

⚠️ **Change these in production!**

### Environment Setup

All OpenStack CLI commands need these environment variables:

```bash
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
```

Save to `~/.lightstack-env` and source it:
```bash
source ~/.lightstack-env
```

---

## Verification

### 1. Check Services

**Docker Compose:**
```bash
docker compose -f deployments/docker-compose.yml ps

# Should show:
# lightstack            Up (healthy)
# lightstack-redis      Up (healthy)
# lightstack-postgres   Up (healthy)
```

**Binary:**
```bash
curl http://localhost:35357/v3
# Should return JSON with version info
```

### 2. Test Authentication

```bash
source ~/.lightstack-env
openstack token issue
```

**Expected output:**
```
+------------+------------------+
| Field      | Value            |
+------------+------------------+
| expires    | 2026-03-08...    |
| id         | eyJhbGc...       |
| project_id | 00000000-...     |
| user_id    | 00000000-...     |
+------------+------------------+
```

### 3. List Resources

```bash
# List flavors
openstack flavor list

# List networks
openstack network list

# Show quotas
openstack quota show --compute
```

### 4. Create Test VM

```bash
# Create network
openstack network create test-net

# Create VM
openstack server create \
  --flavor m1.small \
  --image cirros \
  --network test-net \
  test-vm

# Verify
openstack server list
```

✅ **If all commands succeed, Lightstack is working correctly!**

---

## Troubleshooting

### Issue: Authentication Fails (401)

**Symptoms:**
```
invalid credentials (HTTP 401)
```

**Solutions:**
1. Check credentials match defaults (Domain: "Default" with capital D)
2. Verify Lightstack is running: `docker compose -f deployments/docker-compose.yml ps` or `curl http://localhost:35357/v3`
3. Check database has domain:
   ```bash
   docker exec lightstack-postgres psql -U lightstack -d lightstack \
     -c "SELECT * FROM domains;"
   ```

### Issue: Port Already in Use

**Symptoms:**
```
Bind for 0.0.0.0:35357 failed: port is already allocated
```

**Solutions:**
```bash
# Find what's using the port
lsof -i:35357

# Kill the process or change port in deployments/docker-compose.yml
```

### Issue: Database Connection Failed

**Symptoms:**
```
failed to connect to postgres
```

**Solutions:**
1. Verify PostgreSQL is running:
   ```bash
   docker compose -f deployments/docker-compose.yml ps postgres
   # or: sudo systemctl status postgresql
   ```

2. Check database exists:
   ```bash
   docker exec lightstack-postgres psql -U lightstack -l
   ```

3. Test connection:
   ```bash
   psql "postgres://lightstack:secret@localhost/lightstack?sslmode=disable"
   ```

### Issue: Migrations Failed

**Symptoms:**
```
migration failed: relation "users" does not exist
```

**Solutions:**
```bash
# Check migration status
docker exec lightstack /app/lightstack-migrate status

# Run migrations manually
docker exec lightstack /app/lightstack-migrate up

# Reset and rerun (⚠️ deletes all data)
docker exec lightstack /app/lightstack-migrate reset
docker exec lightstack /app/lightstack-migrate up
```

### Issue: Service Not Healthy

**Symptoms:**
```
lightstack   Up 2 minutes (unhealthy)
```

**Solutions:**
```bash
# Check logs
docker compose -f deployments/docker-compose.yml logs lightstack --tail=50

# Common issues:
# - Database not ready: wait 10 more seconds
# - Port conflict: check lsof output
# - Config error: check lightstack.yaml syntax
```

### Issue: OpenStack CLI Not Found

**Symptoms:**
```
command not found: openstack
```

**Solutions:**
```bash
# Install with pipx
pipx install python-openstackclient

# Or with pip
pip install python-openstackclient

# Verify
openstack --version
```

### Getting Help

1. **Check logs:**
   ```bash
   docker compose -f deployments/docker-compose.yml logs lightstack
   ```

2. **Check database:**
   ```bash
   docker exec lightstack-postgres psql -U lightstack -d lightstack
   ```

3. **Test APIs directly:**
   ```bash
   curl http://localhost:35357/v3
   curl http://localhost:8774/v2.1/
   ```

4. **GitHub Issues:** Report bugs at repository issues page

---

## Next Steps

### Learn the Basics
- **[Quickstart Guide](QUICKSTART.md)** - Create your first VM in 5 minutes
- **[Operations Guide](OPERATIONS.md)** - Day-to-day management tasks

### Configure Your Installation
- **[Configuration Guide](CONFIGURATION.md)** - Networking, storage, security
- **[Docker Deployment](DOCKER_DEPLOYMENT.md)** - Docker-specific configuration

### Advanced Topics
- **[Multi-Architecture](MULTIARCH.md)** - Building for ARM/AMD64
- **[Architecture](ARCHITECTURE.md)** - System design and components
- **[API Reference](API.md)** - OpenStack API compatibility

### Production Deployment
See `docs/OPERATIONS.md` for:
- Systemd service setup
- Monitoring and logging
- Backup strategies
- Security hardening
- High availability

---

## Quick Reference

### Common Commands

```bash
# Docker Compose
docker compose -f deployments/docker-compose.yml up -d          # Start services
docker compose -f deployments/docker-compose.yml down           # Stop services
docker compose -f deployments/docker-compose.yml ps             # Check status
docker compose -f deployments/docker-compose.yml logs -f lightstack    # View logs

# OpenStack CLI
source ~/.lightstack-env             # Load environment
openstack server list         # List VMs
openstack network list        # List networks
openstack volume list         # List volumes
openstack quota show          # Show quotas

# Database
docker exec lightstack-postgres psql -U lightstack -d lightstack  # Connect
docker exec lightstack /app/lightstack-migrate status                    # Migration status
```

### Default Ports

| Service | Port | Description |
|---------|------|-------------|
| Keystone | 35357 | Identity/Auth |
| Nova | 8774 | Compute |
| Neutron | 9696 | Network |
| Cinder | 8776 | Block Storage |
| Glance | 9292 | Images |
| Placement | 8778 | Placement API (stub; not published by compose) |
| Metadata | 8775 | VM Metadata |
| PostgreSQL | 5432 | Database |
| Redis | 6379 | Cache |

---

## Support

- **Documentation:** `/docs` directory
- **Examples:** `docs/OPERATIONS.md`
- **Issues:** GitHub Issues
- **Community:** GitHub Discussions

---

**Congratulations! Lightstack is now installed and ready to use!** 🚀
