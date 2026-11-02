# Lightstack + Horizon Quick Reference

**Deployment**: `docker-compose-horizon.yml`
**Documentation**: [docs/UNIFIED_DEPLOYMENT.md](UNIFIED_DEPLOYMENT.md)

## Quick Start

```bash
# Start everything (both secrets are required — no insecure defaults render)
cd lightstack/deployments
export LIGHTSTACK_JWT_SECRET="$(openssl rand -base64 32)"
export LIGHTSTACK_ADMIN_PASSWORD="$(openssl rand -base64 24)"
docker compose -f docker-compose-horizon.yml up -d

# Access Horizon
open http://localhost/dashboard
# Login: Domain=Default, User=admin, Password=$LIGHTSTACK_ADMIN_PASSWORD

# Check status
docker compose -f docker-compose-horizon.yml ps

# View logs
docker compose -f docker-compose-horizon.yml logs -f
```

## Essential Commands

```bash
# Container Management
docker compose -f docker-compose-horizon.yml ps              # Status
docker compose -f docker-compose-horizon.yml restart horizon # Restart
docker compose -f docker-compose-horizon.yml stop            # Stop all
docker compose -f docker-compose-horizon.yml down            # Remove containers
docker compose -f docker-compose-horizon.yml down -v         # Remove all (including data)

# Logs
docker compose -f docker-compose-horizon.yml logs -f lightstack     # lightstack logs
docker compose -f docker-compose-horizon.yml logs -f horizon # Horizon logs
docker compose -f docker-compose-horizon.yml logs --tail 100 # Last 100 lines

# Shell Access
docker exec -it lightstack /bin/sh                    # lightstack container
docker exec -it lightstack-horizon /bin/bash          # Horizon container
docker exec -it lightstack-postgres psql -U lightstack       # Database
```

## Service Ports

| Service | Port | URL |
|---------|------|-----|
| Horizon | 80 | http://localhost/dashboard |
| Keystone | 35357 | http://localhost:35357/v3 |
| Nova | 8774 | http://localhost:8774/ |
| Neutron | 9696 | http://localhost:9696/ |
| Cinder | 8776 | http://localhost:8776/ |
| Glance | 9292 | http://localhost:9292/ |
| Metadata | 8775 | http://localhost:8775/ |
| noVNC | 6080 | http://localhost:6080/ |
| PostgreSQL | 5432 | localhost:5432 |

## OpenStack CLI Setup

```bash
# Export environment (the admin password you set when starting the stack)
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD="${LIGHTSTACK_ADMIN_PASSWORD:?set LIGHTSTACK_ADMIN_PASSWORD (see Quick Start above)}"
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3

# Test commands
openstack token issue
openstack server list
openstack network list
openstack volume list
openstack image list
```

## Common Operations

```bash
# Create network
openstack network create test-net
openstack subnet create test-subnet --network test-net --subnet-range 10.0.0.0/24

# Create instance
openstack server create --flavor m1.small --image cirros --network test-net test-vm

# Create volume
openstack volume create --size 1 test-volume

# Attach volume
openstack server add volume test-vm test-volume

# Cleanup
openstack server delete test-vm
openstack volume delete test-volume
openstack subnet delete test-subnet
openstack network delete test-net
```

## Troubleshooting Quick Fixes

```bash
# Service won't start
docker compose -f docker-compose-horizon.yml logs <service>
docker compose -f docker-compose-horizon.yml restart <service>

# Horizon connection errors
docker exec lightstack-horizon curl http://lightstack:35357/v3
docker compose -f docker-compose-horizon.yml restart horizon

# Database issues
docker compose -f docker-compose-horizon.yml ps postgres
docker exec lightstack-postgres pg_isready -U lightstack

# Reset everything (destroys data!)
docker compose -f docker-compose-horizon.yml down -v
docker compose -f docker-compose-horizon.yml up -d
```

## Configuration Files

```
deployments/
├── docker-compose-horizon.yml       # Main compose file
└── horizon-config/
    ├── config.json                  # Kolla configuration
    └── local_settings                # Horizon settings
```

## Health Checks

```bash
# All services
curl -f http://localhost:35357/v3 && \
curl -f http://localhost:8774/ && \
curl -f http://localhost:9696/ && \
curl -f http://localhost:80/dashboard/auth/login/ && \
echo "All services OK"

# Container health
docker compose -f docker-compose-horizon.yml ps --format "table {{.Name}}\t{{.Status}}\t{{.Health}}"
```

## Backup

```bash
# Database
docker exec lightstack-postgres pg_dump -U lightstack lightstack > backup-$(date +%Y%m%d).sql

# Configuration
tar czf config-backup-$(date +%Y%m%d).tar.gz docker-compose-horizon.yml horizon-config/

# Restore database
cat backup-20260313.sql | docker exec -i lightstack-postgres psql -U lightstack lightstack
```

## Credentials

- **Horizon URL**: http://localhost/dashboard
- **Domain**: Default
- **Username**: admin
- **Password**: the `LIGHTSTACK_ADMIN_PASSWORD` you exported before `up -d` (no default — the guard refuses admin/secret)
- **Project**: default

## Production Checklist

- [ ] Change postgres password
- [ ] Generate secure JWT secret
- [ ] Generate secure Horizon SECRET_KEY
- [ ] Enable HTTPS
- [ ] Configure ALLOWED_HOSTS
- [ ] Enable secure cookies
- [ ] Set up backups
- [ ] Configure monitoring
- [ ] Review firewall rules
- [ ] Set resource limits

## Support

- **Full Guide**: [docs/UNIFIED_DEPLOYMENT.md](UNIFIED_DEPLOYMENT.md)
- **Horizon Deployment**: [docs/HORIZON_DEPLOYMENT.md](HORIZON_DEPLOYMENT.md)
- **Issues**: https://github.com/cobaltcore-dev/lightstack/issues
