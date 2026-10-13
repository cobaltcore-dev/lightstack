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

**File:** `deployments/docker-compose.yml`

```yaml
services:
  postgres:
    image: postgres:18.4-alpine
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

  redis:
    image: redis:7-alpine
    container_name: lightstack-redis
    command: redis-server --appendonly yes
    volumes:
      - redis_data:/data
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5

  lightstack:
    build:
      context: ..
      dockerfile: build/package/Dockerfile
    container_name: lightstack
    depends_on:
      postgres:
        condition: service_healthy
      redis:
        condition: service_healthy
    ports:
      - "35357:35357"   # Keystone (identity)
      - "8774:8774"     # Nova (compute)
      - "9696:9696"     # Neutron (network)
      - "8776:8776"     # Cinder (volumes)
      - "9292:9292"     # Glance (images)
      - "8775:8775"     # Metadata service
    environment:
      LIGHTSTACK_DB_URL: "postgres://lightstack:secret@postgres:5432/lightstack?sslmode=disable"
      LIGHTSTACK_JWT_SECRET: "change-me-in-production"
      LIGHTSTACK_ENDPOINT_HOST: "localhost"
      LIGHTSTACK_REDIS_URL: "redis://redis:6379/0"
      LIGHTSTACK_CACHE_ENABLED: "true"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:35357/v3"]
      interval: 10s
      timeout: 5s
      retries: 3
    restart: unless-stopped

volumes:
  postgres_data:
  redis_data:
```

### Step 3: Start Services

```bash
docker compose -f deployments/docker-compose.yml up -d
```

**Expected output:**
```
[+] Running 4/4
 ✔ Container lightstack-postgres       Started (healthy)
 ✔ Container lightstack-redis          Started (healthy)
 ✔ Container lightstack                Started (healthy)
```

### Step 4: Verify Deployment

```bash
# Check service status
docker compose -f deployments/docker-compose.yml ps
```

**Expected output:**
```
NAME           IMAGE                COMMAND                  SERVICE    STATUS
lightstack            lightstack       "/bin/sh -c '/app/o3…"   lightstack        Up (healthy)
lightstack-redis      redis:7-alpine   "docker-entrypoint.s…"   redis       Up (healthy)
lightstack-postgres   postgres:18.4-alpine   "docker-entrypoint.s…"   postgres   Up (healthy)
```

```bash
# Check logs
docker compose -f deployments/docker-compose.yml logs lightstack --tail=20
```

**Expected healthy logs** (illustrative — the binary logs plain-text startup lines
plus zerolog JSON for request logs; look for the migration run and the per-service
listener lines, with no repeated errors or restarts):
```
Running database migrations...
Database connection established (pool: max=50, min=2)
  - Keystone:              http://localhost:35357
  - Nova:                  http://localhost:8774
  - Neutron:               http://localhost:9696
  - Cinder:                http://localhost:8776
  - Glance:                http://localhost:9292
  - Placement:             http://localhost:8778
```

### Step 5: Test APIs

```bash
# Test Keystone
curl http://localhost:35357/v3

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
│  │  │  - Keystone :35357                │ │     │
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
│  │  │  PostgreSQL 18.4                  │ │     │
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
http://localhost:35357/v3 (Keystone)
    ↓
lightstack Container :35357
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
> will appear **empty** after `docker compose -f deployments/docker-compose.yml down && ... up` — the data is still
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

Override defaults by editing `deployments/docker-compose.yml`. The binary reads these environment variables:

```yaml
lightstack:
  environment:
    # Database (connection string — the binary does not read separate
    # DB_USER/DB_PASSWORD/DB_NAME variables)
    LIGHTSTACK_DB_URL: "postgres://lightstack:secret@postgres:5432/lightstack?sslmode=disable"

    # JWT Secret (CHANGE IN PRODUCTION!)
    LIGHTSTACK_JWT_SECRET: "your-secret-key-here"

    # Host/scheme used in advertised service-catalog endpoint URLs
    LIGHTSTACK_ENDPOINT_HOST: "localhost"
    LIGHTSTACK_ENDPOINT_SCHEME: "http"

    # Set by the compose file, but NOT read by the binary —
    # the cache is configured via the `cache:` block in lightstack.yaml only
    LIGHTSTACK_REDIS_URL: "redis://redis:6379/0"
    LIGHTSTACK_CACHE_ENABLED: "true"
```

Other supported variables include `LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER` (test stacks only), `LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS`, and the `LIGHTSTACK_GLANCE_S3_*` family for Glance S3 storage.

Settings without an environment equivalent — such as the Keystone token TTL (`keystone.token_ttl`) and the service listen ports (`keystone.port`, `nova.port`, etc.) — are configured in `lightstack.yaml`.

### Port Mapping

Change external ports without modifying Lightstack code:

```yaml
lightstack:
  ports:
    - "35357:35357"   # External:Internal
    - "8774:8774"
```

To expose Keystone on a different host port, change only the left side, e.g. `"35358:35357"`.

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
docker compose -f deployments/docker-compose.yml up -d

# Stop all services (keeps data)
docker compose -f deployments/docker-compose.yml down

# Stop and remove volumes (DELETES ALL DATA!)
docker compose -f deployments/docker-compose.yml down -v

# Restart single service
docker compose -f deployments/docker-compose.yml restart lightstack
docker compose -f deployments/docker-compose.yml restart postgres
```

### View Logs

```bash
# All services
docker compose -f deployments/docker-compose.yml logs -f

# Specific service
docker compose -f deployments/docker-compose.yml logs -f lightstack
docker compose -f deployments/docker-compose.yml logs -f postgres

# Last N lines
docker compose -f deployments/docker-compose.yml logs lightstack --tail=50

# Since timestamp
docker compose -f deployments/docker-compose.yml logs --since="2026-03-07T20:00:00"
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
docker compose -f deployments/docker-compose.yml build lightstack

# Restart with new image
docker compose -f deployments/docker-compose.yml up -d
```

---

## Troubleshooting

### Service Won't Start

**Symptom:** Container exits immediately

**Debug:**
```bash
# Check exit reason
docker compose -f deployments/docker-compose.yml logs lightstack

# Common issues:
# - Database not ready: wait 10 seconds
# - Port conflict: change port in deployments/docker-compose.yml
# - Migration failure: check postgres logs
```

### Port Already in Use

**Symptom:** `Bind for 0.0.0.0:35357 failed: port is already allocated`

**Solution:**
```bash
# Find what's using the port
lsof -i:35357

# Option 1: Kill the process
kill -9 <PID>

# Option 2: Change lightstack port
# Edit deployments/docker-compose.yml:
ports:
  - "35358:35357"   # Use different port
```

### Database Connection Failed

**Symptom:** `failed to connect to postgres`

**Debug:**
```bash
# Check postgres is running
docker compose -f deployments/docker-compose.yml ps postgres

# Check postgres logs
docker compose -f deployments/docker-compose.yml logs postgres

# Test connection from lightstack container
docker exec lightstack nc -zv postgres 5432
```

**Solution:**
```bash
# Restart postgres
docker compose -f deployments/docker-compose.yml restart postgres

# Wait for healthy status
docker compose -f deployments/docker-compose.yml ps
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
docker compose -f deployments/docker-compose.yml ps

# Check lightstack is actually listening
docker exec lightstack netstat -tlnp

# Restart service
docker compose -f deployments/docker-compose.yml restart lightstack
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
docker compose -f deployments/docker-compose.yml down -v
docker compose -f deployments/docker-compose.yml up -d
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
docker compose -f deployments/docker-compose.yml build lightstack
```

**Build for specific platform:**
```bash
docker buildx build \
  --platform linux/amd64 \
  -t lightstack:amd64 \
  -f build/package/Dockerfile \
  .
```

**Build for both platforms:**
```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t lightstack:latest \
  -f build/package/Dockerfile \
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
curl http://localhost:35357/v3
```

**Prometheus metrics** — planned, not yet implemented (no `/metrics` endpoint or
9090 listener exists as of v0.6.x; see [OPERATIONS.md](OPERATIONS.md#monitoring)
for what the health endpoints provide today):
```bash
# curl http://localhost:9090/metrics   # (the endpoint above, once implemented)
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

Connection pool settings are configured in `lightstack.yaml` (not via environment variables):

```yaml
database:
  max_connections: 50
  min_connections: 2
  max_conn_lifetime: 1h
  max_conn_idle_time: 10m
  health_check_period: 30s
```

Mount the adjusted file as shown in [Custom Configuration File](#custom-configuration-file).

---

## Summary

**Docker Compose deployment provides:**
- ✅ One-command deployment (`docker compose -f deployments/docker-compose.yml up -d`)
- ✅ Automatic health checks
- ✅ Data persistence
- ✅ Easy updates (`docker compose -f deployments/docker-compose.yml build && docker compose -f deployments/docker-compose.yml up -d`)
- ✅ Multi-architecture support (ARM64 + AMD64)
- ✅ Production-ready configuration

**Next steps:**
- [QUICKSTART.md](QUICKSTART.md) - Create your first VM
- [CONFIGURATION.md](CONFIGURATION.md) - Advanced configuration
- [OPERATIONS.md](OPERATIONS.md) - Production deployment
- [MULTIARCH.md](MULTIARCH.md) - Multi-architecture builds
