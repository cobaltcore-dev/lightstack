# Operations Guide

**📚 Complete Documentation**: See **[INDEX.md](INDEX.md)** for full documentation index with learning paths.


Day-to-day operations, monitoring, and maintenance for Lightstack deployments.

---

## Table of Contents

- [Overview](#overview)
- [Daily Operations](#daily-operations)
- [Monitoring](#monitoring)
- [Backup and Recovery](#backup-and-recovery)
- [Maintenance](#maintenance)
- [Troubleshooting](#troubleshooting)
- [Security](#security)
- [Performance](#performance)
- [High Availability](#high-availability)

---

## Overview

This guide covers operational tasks for running Lightstack in production environments.

**Target audience:** System administrators, DevOps engineers, SREs

---

## Daily Operations

### Check System Status

```bash
# Docker deployment
docker compose ps

# Expected output:
# NAME           STATUS
# lightstack            Up (healthy)
# lightstack-postgres   Up (healthy)

# Systemd deployment
sudo systemctl status lightstack
sudo systemctl status postgresql
```

### View Logs

```bash
# Docker
docker compose logs lightstack --tail=100 --follow

# Systemd
sudo journalctl -u lightstack -f

# Check for errors
docker compose logs lightstack | grep -i error
```

### Resource Usage

```bash
# Docker container stats
docker stats lightstack lightstack-postgres

# System resources
top
htop
vmstat 1
```

### Check API Health

```bash
# Keystone
curl http://localhost:35357/v3
# Should return: {"version": {...}}

# Nova
curl http://localhost:8774/v2.1
# Should return: {"versions": [...]}

# Neutron
curl http://localhost:9696/v2.0
# Should return: {"versions": [...]}

# All services (Metadata :8775 included; Placement :8778 is a stub whose
# version discovery answers too, if you run it reachable)
for port in 35357 8774 9696 8776 9292 8775 8778; do
  echo "Port $port: $(curl -s http://localhost:$port | jq -r '.version.status // .versions[0].status // "ERROR"')"
done
```

### Database Health

```bash
# Docker
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "SELECT version();"

# Check database size
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT pg_size_pretty(pg_database_size('lightstack')) AS size;
"

# Check table sizes
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT schemaname, tablename, pg_size_pretty(pg_total_relation_size(schemaname||'.'||tablename)) AS size
  FROM pg_tables
  WHERE schemaname = 'public'
  ORDER BY pg_total_relation_size(schemaname||'.'||tablename) DESC
  LIMIT 10;
"
```

### Active Resources

```bash
# Using OpenStack CLI
source ~/.lightstack-env

# Count active resources
openstack server list --all-projects | wc -l
openstack network list --all-projects | wc -l
openstack volume list --all-projects | wc -l

# Resource usage by project
openstack server list --all-projects -f json | jq 'group_by(.Project) | map({project: .[0].Project, count: length})'
```

---

## Monitoring

### Metrics to Monitor

**Service Health:**
- API response times (target: <50ms p95)
- API error rates (target: <1%)
- Service uptime (target: 99.9%)

**Resource Usage:**
- CPU usage (target: <80%)
- Memory usage (target: <85%)
- Disk usage (target: <80%)
- Database connections (target: <80% of max)

**Business Metrics:**
- Total VMs
- Total networks
- Total volumes
- Quota utilization per project

### Health Check Endpoints

```bash
# Keystone health
curl http://localhost:35357/v3

# Nova health
curl http://localhost:8774/v2.1

# Database connectivity (via API)
openstack token issue
```

### Log Monitoring

**Important log patterns to watch:**

```bash
# Errors
docker compose logs lightstack | grep -i "ERROR"

# Authentication failures
docker compose logs lightstack | grep "authentication failed"

# Database issues
docker compose logs lightstack | grep "database"

# Slow queries (if debug logging enabled)
docker compose logs lightstack | grep "slow query"
```

### Alerting Rules

**Critical alerts:**
- Service down (any API unreachable for >1 minute)
- Database connection failure
- Disk usage >90%
- Memory usage >95%

**Warning alerts:**
- API response time >100ms (p95)
- Error rate >5%
- Disk usage >80%
- High database connection usage (>80%)

### Prometheus Metrics (Future)

```bash
# Endpoint (when implemented)
curl http://localhost:9090/metrics
```

**Key metrics:**
- `lightstack_api_requests_total{service="nova",method="POST",status="200"}`
- `lightstack_api_request_duration_seconds{service="nova"}`
- `lightstack_database_connections{state="active"}`
- `lightstack_instances_total{state="active"}`

---

## Backup and Recovery

### Database Backup

**Automated daily backup:**

```bash
#!/bin/bash
# /usr/local/bin/lightstack-backup.sh

BACKUP_DIR="/backups/lightstack"
DATE=$(date +%Y%m%d-%H%M%S)
BACKUP_FILE="$BACKUP_DIR/lightstack-$DATE.sql.gz"

mkdir -p $BACKUP_DIR

# Create backup
docker exec lightstack-postgres pg_dump -U lightstack lightstack | gzip > $BACKUP_FILE

# Keep only last 7 days
find $BACKUP_DIR -name "lightstack-*.sql.gz" -mtime +7 -delete

echo "Backup completed: $BACKUP_FILE"
```

**Crontab entry:**
```bash
# Daily backup at 2 AM
0 2 * * * /usr/local/bin/lightstack-backup.sh
```

**Manual backup:**
```bash
# Backup
docker exec lightstack-postgres pg_dump -U lightstack lightstack > lightstack-backup.sql

# Compressed backup
docker exec lightstack-postgres pg_dump -U lightstack lightstack | gzip > lightstack-backup.sql.gz
```

### Database Restore

```bash
# Stop lightstack (prevent writes during restore)
docker compose stop lightstack

# Restore from backup
cat lightstack-backup.sql | docker exec -i lightstack-postgres psql -U lightstack lightstack

# Or compressed:
gunzip -c lightstack-backup.sql.gz | docker exec -i lightstack-postgres psql -U lightstack lightstack

# Start lightstack
docker compose start lightstack
```

### Configuration Backup

```bash
# Backup configuration
cp deployments/docker-compose.yml deployments/docker-compose.yml.backup-$(date +%Y%m%d)
cp config/lightstack.yaml config/lightstack.yaml.backup-$(date +%Y%m%d)

# Or tar everything:
tar -czf lightstack-config-$(date +%Y%m%d).tar.gz deployments/ config/
```

### Disaster Recovery Plan

**Recovery Time Objective (RTO):** <1 hour
**Recovery Point Objective (RPO):** <24 hours (daily backups)

**Recovery steps:**
1. Install Lightstack on new system
2. Restore configuration files
3. Restore database from backup
4. Start services
5. Verify all APIs respond
6. Check resource counts

```bash
# Recovery script
#!/bin/bash
set -e

# 1. Setup
cd /opt/lightstack
docker compose down -v

# 2. Restore config (from backup location)
cp /backups/config/docker-compose.yml deployments/
cp /backups/config/lightstack.yaml config/

# 3. Start database only
docker compose up -d postgres
sleep 10

# 4. Restore database
gunzip -c /backups/lightstack/latest.sql.gz | docker exec -i lightstack-postgres psql -U lightstack lightstack

# 5. Start lightstack
docker compose up -d lightstack

# 6. Verify
sleep 10
curl http://localhost:35357/v3 || echo "ERROR: Keystone not responding"
openstack token issue || echo "ERROR: Authentication failed"

echo "Recovery complete!"
```

---

## Maintenance

### Update Lightstack

```bash
# Backup first!
./lightstack-backup.sh

# Pull latest code
cd /opt/lightstack
git pull

# Rebuild image
docker compose build lightstack

# Stop and start (runs migrations automatically)
docker compose down
docker compose up -d

# Verify
docker compose ps
docker compose logs lightstack --tail=50
```

### Database Migrations

```bash
# Check current migration version
docker exec lightstack /app/lightstack-migrate status

# Run pending migrations
docker exec lightstack /app/lightstack-migrate up

# Rollback last migration (if needed)
docker exec lightstack /app/lightstack-migrate down 1
```

> ⚠️ **Rolling back requires care.** Some migrations were edited in place after shipping
> (e.g. `065`, the console-token columns, and `071`, the placement catalog rows), so a
> database that already ran the original version keeps it. Migrate the schema down
> **before** reverting the binary — and note that down-migrations may lose data added by
> the up side (the console columns and catalog rows above are dropped with it). See
> [UPGRADING.md](UPGRADING.md).

### Database Maintenance

```bash
# Vacuum database (reclaim space)
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "VACUUM ANALYZE;"

# Reindex (improve query performance)
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "REINDEX DATABASE lightstack;"

# Check for table bloat
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT schemaname, tablename,
         pg_size_pretty(pg_total_relation_size(schemaname||'.'||tablename)) AS size
  FROM pg_tables
  WHERE schemaname = 'public'
  ORDER BY pg_total_relation_size(schemaname||'.'||tablename) DESC;
"
```

### Log Rotation

**Docker logs:**
```yaml
# docker-compose.yml
services:
  lightstack:
    logging:
      driver: "json-file"
      options:
        max-size: "10m"
        max-file: "3"
```

**Systemd logs:**
```bash
# /etc/systemd/journald.conf
SystemMaxUse=1G
SystemKeepFree=2G
```

### Cleanup Old Data

```bash
# Delete old deleted VMs (soft-deleted)
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  DELETE FROM instances
  WHERE deleted = true
    AND deleted_at < NOW() - INTERVAL '30 days';
"

# Tokens: nothing to clean up in the database — tokens are stateless JWTs and
# there is no `tokens` table. They expire on their own per `keystone.token_ttl`.
# noVNC console tokens can be inspected and revoked with:
#   lightstack console-tokens list
#   lightstack console-tokens purge
```

---

## Troubleshooting

### Service Won't Start

**Check logs:**
```bash
docker compose logs lightstack
docker compose logs postgres
```

**Common issues:**
- Database not ready → Wait and retry
- Port conflict → Check `lsof -i:35357`
- Migration failure → Check database state
- Config error → Validate YAML syntax

### High CPU Usage

```bash
# Check container stats
docker stats lightstack

# Check processes inside container
docker exec lightstack top

# Check for slow queries (enable query logging)
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  ALTER DATABASE lightstack SET log_min_duration_statement = 1000;
"
```

### High Memory Usage

```bash
# Check memory usage
docker stats lightstack

# Restart if memory leak suspected
docker compose restart lightstack
```

### Database Connection Exhaustion

```bash
# Check active connections
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT count(*) FROM pg_stat_activity WHERE datname = 'lightstack';
"

# Kill idle connections
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT pg_terminate_backend(pid)
  FROM pg_stat_activity
  WHERE datname = 'lightstack'
    AND state = 'idle'
    AND state_change < NOW() - INTERVAL '10 minutes';
"

# Increase max_connections in config
# config/lightstack.yaml:
# database:
#   max_connections: 50
```

### Disk Space Issues

```bash
# Check disk usage
df -h

# Check Docker volumes
docker system df

# Clean up old images/containers
docker system prune -a

# Check database size
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT pg_size_pretty(pg_database_size('lightstack'));
"
```

---

## Security

### Security Checklist

**Configuration:**
- ✅ Changed default admin password
- ✅ Changed JWT secret
- ✅ Using SSL for database connection
- ✅ Firewall configured (only necessary ports open)
- ✅ Strong passwords for all accounts

**Network:**
- ✅ API behind reverse proxy (nginx, traefik)
- ✅ TLS/SSL enabled for all external access
- ✅ Internal services not exposed to internet

**Database:**
- ✅ Database password changed from default
- ✅ Database access restricted by IP
- ✅ Regular backups enabled
- ✅ Backup encryption enabled

### Rotate JWT Secret

```bash
# 1. Generate new secret
NEW_SECRET=$(openssl rand -hex 32)

# 2. Update config
# Edit docker-compose.yml:
# LIGHTSTACK_JWT_SECRET: "$NEW_SECRET"

# 3. Restart lightstack
docker compose restart lightstack

# 4. All existing tokens invalidated - users must re-authenticate
```

### Change Admin Password

```bash
# Via OpenStack CLI (recommended)
openstack user set --password <new-secure-password> admin
# or, to change the credentials you authenticate with:
openstack user password set
```

> Do not reset passwords with SQL: password hashing is bcrypt in the application
> (Go), not pgcrypto in the database, so a `crypt(..., gen_salt('bf'))` `UPDATE`
> produces a hash Lightstack cannot verify. Rotate via the Identity API.

### Audit Logs

```bash
# Review recent authentication attempts
docker compose logs lightstack | grep "authentication"

# Review API access
docker compose logs lightstack | grep "POST\|DELETE"

# Failed login attempts
docker compose logs lightstack | grep "authentication failed"
```

### Firewall Configuration

```bash
# Allow only necessary ports
ufw allow 35357/tcp  # Keystone (if external)
ufw allow 8774/tcp   # Nova (if external)
ufw allow 9696/tcp   # Neutron (if external)

# Or allow only from specific IPs
ufw allow from 192.168.1.0/24 to any port 35357
```

---

## Performance

### Optimization Tips

**Database:**
```yaml
database:
  max_connections: 50          # Increase for high concurrency
  min_connections: 2
  max_conn_lifetime: 30m
  max_conn_idle_time: 10m
```

**PostgreSQL tuning:**
```sql
-- /var/lib/postgresql/18/docker/postgresql.conf
shared_buffers = 256MB         # 25% of RAM
effective_cache_size = 1GB     # 50-75% of RAM
work_mem = 16MB
maintenance_work_mem = 128MB
```

**Indexes:**
```sql
-- Ensure key indexes exist
CREATE INDEX IF NOT EXISTS idx_instances_project ON instances(project_id);
CREATE INDEX IF NOT EXISTS idx_instances_status ON instances(status);
CREATE INDEX IF NOT EXISTS idx_networks_project ON networks(project_id);
```

### Performance Monitoring

```bash
# API latency
time openstack server list

# Database query time
docker exec lightstack-postgres psql -U lightstack -d lightstack -c "
  SELECT query, mean_exec_time, calls
  FROM pg_stat_statements
  ORDER BY mean_exec_time DESC
  LIMIT 10;
"
```

### Scaling Guidelines

**Single-node limits:**
- ~100-200 concurrent VMs
- ~50-100 concurrent API requests
- ~1000 total VMs (database limited)

**When to scale:**
- CPU usage consistently >80%
- Memory usage >85%
- API response time >100ms (p95)
- Database connections >80% of max

**Scaling options:**
1. Vertical: Increase CPU/RAM
2. Horizontal: Add compute nodes (multi-node setup)
3. Database: Use managed PostgreSQL (RDS, Cloud SQL)

---

## High Availability

### Database HA

**Option 1: PostgreSQL Replication**
```yaml
# Primary-Replica setup
services:
  postgres-primary:
    image: postgres:18.4-alpine

  postgres-replica:
    image: postgres:18.4-alpine
```

> The `POSTGRES_REPLICATION_MODE` / `POSTGRES_MASTER_HOST` variables shown in earlier
> versions of this guide are not features of the stock `postgres` image — replication
> must be configured manually (or via Patroni; see [SCALING.md](SCALING.md)).

**Option 2: Managed Database**
- AWS RDS PostgreSQL
- Google Cloud SQL
- Azure Database for PostgreSQL

### Lightstack HA

**Load Balancer:**
```
           ┌─────────────┐
           │   HAProxy   │
           │  (VIP: IP)  │
           └──────┬──────┘
                  │
      ┌───────────┼───────────┐
      │           │           │
   ┌──▼──┐     ┌──▼──┐     ┌──▼──┐
   │ lightstack1│     │ lightstack2│     │ lightstack3│
   └─────┘     └─────┘     └─────┘
      │           │           │
      └───────────┼───────────┘
                  │
           ┌──────▼──────┐
           │  PostgreSQL │
           │  (Primary)  │
           └─────────────┘
```

**HAProxy config:**
```
frontend lightstack_keystone
    bind *:35357
    default_backend lightstack_keystone_backend

backend lightstack_keystone_backend
    balance roundrobin
    option httpchk GET /v3
    server lightstack1 10.0.0.1:35357 check
    server lightstack2 10.0.0.2:35357 check
    server lightstack3 10.0.0.3:35357 check
```

### CLI Subcommands

The `lightstack` binary ships operational subcommands (its only flags are `--config`
and `--migrations`):

```bash
# Reclaim host state for deleted instances
# (--dry-run reports what would be reclaimed without doing it)
lightstack cleanup [--dry-run]

# Liveness/readiness probe for wrappers, orchestrators and health scripts
lightstack healthcheck [--ready|--live]

# Inspect or revoke noVNC console tokens
lightstack console-tokens list
lightstack console-tokens purge
```

For the health script, prefer the built-in probe over a hand-rolled curl:

```bash
# Script: /usr/local/bin/lightstack-healthcheck.sh
#!/bin/bash
lightstack healthcheck --ready && exit 0 || exit 1
```

---

## Summary

**Key operations:**
- ✅ Daily health checks
- ✅ Log monitoring
- ✅ Regular backups
- ✅ Security audits
- ✅ Performance monitoring
- ✅ Database maintenance

**Automation opportunities:**
- Automated backups (cron)
- Log rotation (systemd/Docker)
- Health monitoring (Prometheus)
- Alert notifications (PagerDuty, Slack)

**Next steps:**
- [CONFIGURATION.md](CONFIGURATION.md) - Detailed configuration
- [DOCKER_DEPLOYMENT.md](DOCKER_DEPLOYMENT.md) - Docker-specific operations
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) - Detailed troubleshooting (if created)
