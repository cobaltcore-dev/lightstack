# Docker Deployment Guide

Complete guide for deploying Lightstack using Docker and Docker Compose.

---

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Docker Compose Deployment](#docker-compose-deployment)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [Management](#management)
- [Troubleshooting](#troubleshooting)
- [Advanced Topics](#advanced-topics)

---

## Overview

Lightstack provides a production-ready Docker Compose deployment that includes:

- **lightstack service** - All OpenStack APIs in one container
- **PostgreSQL** - Persistent database storage
- **Health checks** - Automatic service monitoring
- **Volume persistence** - Data survives container restarts
- **Multi-architecture** - Works on ARM64 (Apple Silicon) and AMD64 (Intel/AMD)

---

## Prerequisites

### Required Software

**Docker Desktop (Recommended):**
- macOS: [Install Docker Desktop](https://docs.docker.com/desktop/install/mac-install/)
- Windows: [Install Docker Desktop](https://docs.docker.com/desktop/install/windows-install/)
- Linux: [Install Docker Engine](https://docs.docker.com/engine/install/)

**Minimum versions:**
- Docker Engine 20.10+
- Docker Compose V2 (included in Docker Desktop)

**Verify installation:**
```bash
docker --version
docker compose version
```

### System Requirements

**Minimum:**
- CPU: 2 cores
- RAM: 4GB
- Disk: 10GB free space

**Recommended:**
- CPU: 4+ cores
- RAM: 8GB+
- Disk: 50GB+ free space

---

## Docker Compose Deployment

### Step 1: Clone Repository

```bash
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack
```

### Step 2: Review Configuration

**File:** `docker-compose.yml`

```yaml
services:
  postgres:
    image: postgres:18.3-alpine
    container_name: lightstack-postgres
    environment:
      POSTGRES_DB: lightstack
      POSTGRES_USER: lightstack
      POSTGRES_PASSWORD: secret
    volumes:
      - postgres_data:/var/lib/postgresql
    ports:
      - "5432:5432"
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U lightstack"]
      interval: 5s
      timeout: 5s
      retries: 5

  lightstack:
    build:
      context: .
      dockerfile: deployments/docker/Dockerfile
    container_name: lightstack
    depends_on:
      postgres:
        condition: service_healthy
    ports:
      - "5001:35357"   # Keystone (identity)
      - "8774:8774"    # Nova (compute)
      - "9696:9696"    # Neutron (network)
      - "8776:8776"    # Cinder (volumes)
      - "9292:9292"    # Glance (images)
      - "8775:8775"    # Metadata service
    environment:
      LIGHTSTACK_DB_URL: "postgres://lightstack:secret@postgres:5432/lightstack?sslmode=disable"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:35357/v3"]
      interval: 10s
      timeout: 5s
      retries: 3
      start_period: 30s
    restart: unless-stopped

volumes:
  postgres_data:
```

### Step 3: Start Services

```bash
docker compose up -d
```

**Expected output:**
```
[+] Running 3/3
 ✔ Network lightstack_default   Created
 ✔ Container lightstack-postgres       Started (healthy)
 ✔ Container lightstack                Started (healthy)
```

### Step 4: Verify Deployment

```bash
# Check service status
docker compose ps
```

**Expected output:**
```
NAME           IMAGE                COMMAND                  SERVICE    STATUS
lightstack            lightstack       "/bin/sh -c '/app/o3…"   lightstack        Up (healthy)
lightstack-postgres   postgres:18.3-alpine   "docker-entrypoint.s…"   postgres   Up (healthy)
```

```bash
# Check logs
docker compose logs lightstack --tail=20
```

**Expected healthy logs:**
```
lightstack | 2026-03-07T21:45:12Z [INFO] Running migrations...
lightstack | 2026-03-07T21:45:13Z [INFO] Migrations complete
lightstack | 2026-03-07T21:45:13Z [INFO] Starting lightstack services...
lightstack | 2026-03-07T21:45:13Z [INFO] Keystone listening on :35357
lightstack | 2026-03-07T21:45:13Z [INFO] Nova listening on :8774
lightstack | 2026-03-07T21:45:13Z [INFO] Neutron listening on :9696
lightstack | 2026-03-07T21:45:13Z [INFO] Cinder listening on :8776
lightstack | 2026-03-07T21:45:13Z [INFO] Glance listening on :9292
```

### Step 5: Test APIs

```bash
# Test Keystone
curl http://localhost:5001/v3

# Test Nova
curl http://localhost:8774/v2.1

# Test Neutron
curl http://localhost:9696/v2.0
```

All should return JSON with version information.

---

## Architecture

### Container Layout

```
┌─────────────────────────────────────────────────┐
│  Docker Host (macOS/Linux/Windows)             │
├─────────────────────────────────────────────────┤
│                                                  │
│  ┌───────────────────────────────────────┐     │
│  │  lightstack Container                         │     │
│  │  ┌──────────────────────────────────┐ │     │
│  │  │  lightstack Binary (ARM64 or AMD64)     │ │     │
│  │  │  - Keystone :35357 → :5001       │ │     │
│  │  │  - Nova :8774                    │ │     │
│  │  │  - Neutron :9696                 │ │     │
│  │  │  - Cinder :8776                  │ │     │
│  │  │  - Glance :9292                  │ │     │
│  │  │  - Metadata :8775                │ │     │
│  │  └──────────────────────────────────┘ │     │
│  └───────────────┬───────────────────────┘     │
│                  │                              │
│  ┌───────────────┴───────────────────────┐     │
│  │  lightstack-postgres Container                │     │
│  │  ┌──────────────────────────────────┐ │     │
│  │  │  PostgreSQL 16                   │ │     │
│  │  │  Database: lightstack            │ │     │
│  │  │  Volume: postgres_data           │ │     │
│  │  └──────────────────────────────────┘ │     │
│  └───────────────────────────────────────┘     │
│                                                  │
└─────────────────────────────────────────────────┘
```

### Network Flow

```
Client (OpenStack CLI)
    ↓
http://localhost:5001/v3 (Keystone)
    ↓
lightstack Container :35357 (internal)
    ↓
Authentication & Token Generation
    ↓
PostgreSQL Container :5432
    ↓
User/Project Verification
```

### Volume Persistence

```
Docker Volume: postgres_data
    ↓
Mounted at: /var/lib/postgresql (in postgres container)
    ↓
Contains:
    - All database files
    - Users, projects, roles
    - VMs, networks, volumes metadata
    - Survives container restarts
```

> ⚠️ **Volume migration required when upgrading an existing deployment.** The
> Postgres data-volume mount path changed from `/var/lib/postgresql/data` to
> `/var/lib/postgresql` (Postgres 18+ stores its data in a versioned
> subdirectory such as `18/docker/`, so the volume must be mounted one level
> up). A pre-existing named volume that was mounted at the old `/data` path
> will appear **empty** after `docker compose down && up` — the data is still
> in the volume, but Postgres no longer reads it, so the apparent data loss is
> silent.

**Before upgrading**, back up the database:

```bash
docker exec lightstack-postgres pg_dump -U lightstack lightstack > lightstack-backup.sql
```

**After upgrading**, if the database appears empty, restore from the backup:

```bash
docker exec -i lightstack-postgres psql -U lightstack lightstack < lightstack-backup.sql
```

---

## Configuration

### Environment Variables

Override defaults by editing `docker-compose.yml`:

```yaml
lightstack:
  environment:
    # Database
    LIGHTSTACK_DB_URL: "postgres://lightstack:secret@postgres:5432/lightstack?sslmode=disable"

    # JWT Secret (CHANGE IN PRODUCTION!)
    LIGHTSTACK_JWT_SECRET: "your-secret-key-here"

    # Token TTL
    LIGHTSTACK_TOKEN_TTL: "24h"

    # Service Ports (internal)
    LIGHTSTACK_KEYSTONE_PORT: "35357"
    LIGHTSTACK_NOVA_PORT: "8774"
    LIGHTSTACK_NEUTRON_PORT: "9696"
    LIGHTSTACK_CINDER_PORT: "8776"
    LIGHTSTACK_GLANCE_PORT: "9292"

    # Logging
    LIGHTSTACK_LOG_LEVEL: "info"  # debug, info, warn, error
    LIGHTSTACK_LOG_FORMAT: "json"  # json, text
```

### Port Mapping

Change external ports without modifying Lightstack code:

```yaml
lightstack:
  ports:
    - "5001:35357"   # External:Internal
    - "8774:8774"
```

**Why 5001 instead of 5000?**
- macOS uses port 5000 for AirPlay/AirTunes
- Solution: Map external 5001 to internal 35357

### Custom Configuration File

Mount your own `lightstack.yaml`:

```yaml
lightstack:
  volumes:
    - ./my-custom-config.yaml:/app/config/lightstack.yaml:ro
```

### PostgreSQL Configuration

```yaml
postgres:
  environment:
    POSTGRES_DB: lightstack
    POSTGRES_USER: lightstack
    POSTGRES_PASSWORD: secret  # CHANGE IN PRODUCTION!
    POSTGRES_INITDB_ARGS: "--encoding=UTF8 --locale=en_US.UTF-8"
  volumes:
    - postgres_data:/var/lib/postgresql
    # Optional: Custom init scripts
    - ./init-scripts:/docker-entrypoint-initdb.d:ro
```

---

## Management

### Start/Stop Services

```bash
# Start all services
docker compose up -d

# Stop all services (keeps data)
docker compose down

# Stop and remove volumes (DELETES ALL DATA!)
docker compose down -v

# Restart single service
docker compose restart lightstack
docker compose restart postgres
```

### View Logs

```bash
# All services
docker compose logs -f

# Specific service
docker compose logs -f lightstack
docker compose logs -f postgres

# Last N lines
docker compose logs lightstack --tail=50

# Since timestamp
docker compose logs --since="2026-03-07T20:00:00"
```

### Execute Commands in Containers

```bash
# lightstack container
docker exec -it lightstack /bin/sh

# PostgreSQL container
docker exec -it lightstack-postgres psql -U lightstack -d lightstack

# Run migrations manually
docker exec lightstack /app/lightstack-migrate up

# Check migration status
docker exec lightstack /app/lightstack-migrate status
```

### Database Management

```bash
# Connect to database
docker exec -it lightstack-postgres psql -U lightstack -d lightstack

# List tables
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "\dt"

# Query instances
docker exec lightstack-postgres psql -U lightstack -d lightstack \
  -c "SELECT id, name, status FROM instances;"

# Backup database
docker exec lightstack-postgres pg_dump -U lightstack lightstack > backup.sql

# Restore database
cat backup.sql | docker exec -i lightstack-postgres psql -U lightstack lightstack
```

### Update Lightstack

```bash
# Pull latest code
git pull

# Rebuild image
docker compose build lightstack

# Restart with new image
docker compose up -d
```

---

## Troubleshooting

### Service Won't Start

**Symptom:** Container exits immediately

**Debug:**
```bash
# Check exit reason
docker compose logs lightstack

# Common issues:
# - Database not ready: wait 10 seconds
# - Port conflict: change port in docker-compose.yml
# - Migration failure: check postgres logs
```

### Port Already in Use

**Symptom:** `Bind for 0.0.0.0:5001 failed: port is already allocated`

**Solution:**
```bash
# Find what's using the port
lsof -i:5001

# Option 1: Kill the process
kill -9 <PID>

# Option 2: Change lightstack port
# Edit docker-compose.yml:
ports:
  - "5002:35357"   # Use different port
```

### Database Connection Failed

**Symptom:** `failed to connect to postgres`

**Debug:**
```bash
# Check postgres is running
docker compose ps postgres

# Check postgres logs
docker compose logs postgres

# Test connection from lightstack container
docker exec lightstack nc -zv postgres 5432
```

**Solution:**
```bash
# Restart postgres
docker compose restart postgres

# Wait for healthy status
docker compose ps
```

### Health Check Failing

**Symptom:** `lightstack   Up 2 minutes (unhealthy)`

**Debug:**
```bash
# Check what health check expects
docker inspect lightstack | jq '.[0].Config.Healthcheck'

# Manually run health check
docker exec lightstack curl -f http://localhost:35357/v3
```

**Common fixes:**
```bash
# Wait longer (first start takes 30s for migrations)
docker compose ps

# Check lightstack is actually listening
docker exec lightstack netstat -tlnp

# Restart service
docker compose restart lightstack
```

### Migrations Failed

**Symptom:** `migration failed: relation "users" does not exist`

**Solution:**
```bash
# Check migration status
docker exec lightstack /app/lightstack-migrate status

# Reset and rerun (⚠️ DELETES ALL DATA)
docker exec lightstack /app/lightstack-migrate reset
docker exec lightstack /app/lightstack-migrate up

# Or: Fresh start
docker compose down -v
docker compose up -d
```

### Container Out of Memory

**Symptom:** Container killed by OOM killer

**Solution:**
```bash
# Increase Docker memory limit (Docker Desktop → Settings → Resources)

# Or limit container memory in docker-compose.yml:
lightstack:
  deploy:
    resources:
      limits:
        memory: 512M
      reservations:
        memory: 256M
```

---

## Advanced Topics

### Multi-Architecture Support

Lightstack Docker images support both ARM64 and AMD64 architectures.

**Build for current platform:**
```bash
docker compose build lightstack
```

**Build for specific platform:**
```bash
docker buildx build \
  --platform linux/amd64 \
  -t lightstack:amd64 \
  -f deployments/docker/Dockerfile \
  .
```

**Build for both platforms:**
```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t lightstack:latest \
  -f deployments/docker/Dockerfile \
  .
```

See [MULTIARCH.md](MULTIARCH.md) for detailed multi-architecture guide.

### Production Deployment

**Recommendations for production:**

1. **Change default credentials:**
   ```yaml
   postgres:
     environment:
       POSTGRES_PASSWORD: "<strong-random-password>"

   lightstack:
     environment:
       LIGHTSTACK_JWT_SECRET: "<strong-random-secret>"
   ```

2. **Use secrets management:**
   ```yaml
   services:
     postgres:
       environment:
         POSTGRES_PASSWORD_FILE: /run/secrets/db_password
       secrets:
         - db_password

   secrets:
     db_password:
       file: ./secrets/db_password.txt
   ```

3. **Enable TLS:**
   - Use reverse proxy (nginx, traefik)
   - Mount TLS certificates
   - Configure Lightstack to listen on HTTPS

4. **Resource limits:**
   ```yaml
   lightstack:
     deploy:
       resources:
         limits:
           cpus: '2.0'
           memory: 2G
         reservations:
           cpus: '0.5'
           memory: 512M
   ```

5. **Logging:**
   ```yaml
   lightstack:
     logging:
       driver: "json-file"
       options:
         max-size: "10m"
         max-file: "3"
   ```

6. **Backups:**
   ```bash
   # Automated daily backup
   0 2 * * * docker exec lightstack-postgres pg_dump -U lightstack lightstack | gzip > /backups/lightstack-$(date +\%Y\%m\%d).sql.gz
   ```

### Monitoring

**Health check endpoint:**
```bash
curl http://localhost:5001/v3
```

**Prometheus metrics** (if enabled):
```bash
curl http://localhost:9090/metrics
```

**Container stats:**
```bash
docker stats lightstack lightstack-postgres
```

### Custom Dockerfile

Create your own Dockerfile based on ours:

```dockerfile
FROM lightstack:latest

# Add custom CA certificates
COPY custom-ca.crt /usr/local/share/ca-certificates/
RUN update-ca-certificates

# Add custom scripts
COPY my-init.sh /app/init.sh
RUN chmod +x /app/init.sh

CMD ["/app/init.sh"]
```

---

## Performance Tuning

### PostgreSQL Tuning

```yaml
postgres:
  command:
    - "postgres"
    - "-c"
    - "shared_buffers=256MB"
    - "-c"
    - "effective_cache_size=1GB"
    - "-c"
    - "max_connections=100"
```

### Lightstack Connection Pool

```yaml
lightstack:
  environment:
    LIGHTSTACK_DB_MAX_CONNECTIONS: "20"
    LIGHTSTACK_DB_MAX_IDLE: "5"
    LIGHTSTACK_DB_CONN_MAX_LIFETIME: "1h"
```

---

## Summary

**Docker Compose deployment provides:**
- ✅ One-command deployment (`docker compose up -d`)
- ✅ Automatic health checks
- ✅ Data persistence
- ✅ Easy updates (`docker compose pull && docker compose up -d`)
- ✅ Multi-architecture support (ARM64 + AMD64)
- ✅ Production-ready configuration

**Next steps:**
- [QUICKSTART.md](QUICKSTART.md) - Create your first VM
- [CONFIGURATION.md](CONFIGURATION.md) - Advanced configuration
- [OPERATIONS.md](OPERATIONS.md) - Production deployment
- [MULTIARCH.md](MULTIARCH.md) - Multi-architecture builds
