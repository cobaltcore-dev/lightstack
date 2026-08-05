# Horizon Dashboard Deployment Guide

**Lightstack Version**: v0.5.0+
**Last Updated**: 2026-03-13
**Status**: Production Ready

## Overview

This guide provides step-by-step instructions for deploying OpenStack Horizon dashboard with Lightstack as the backend.

> **Scope of compatibility.** Lightstack serves the OpenStack APIs Horizon consumes, and the dashboard is
> **usable for resource management**: network, volume and instance create work from the UI (#532, #533),
> and the noVNC console works in real mode (#534). A few other gaps remain. See the
> [Horizon Compatibility Report](compatibility/HORIZON_COMPATIBILITY_REPORT.md) (findings F1–F18) before
> relying on any UI workflow below.

## Prerequisites

- Lightstack v0.5.0 or higher running
- Docker and Docker Compose installed
- Network connectivity between Horizon and Lightstack services
- 2GB RAM minimum for Horizon container
- Port 80 (HTTP) available for Horizon dashboard

## Deployment Options

### Option 1: Simplified Horizon Setup (Recommended)

This approach uses a lightweight Horizon configuration without complex Kolla infrastructure.

#### Step 1: Create Deployment Directory

```bash
mkdir -p ~/lightstack-horizon
cd ~/lightstack-horizon
```

#### Step 2: Create Horizon Configuration

Create `local_settings.py`:

```python
# Lightstack Horizon Configuration
import os

# Basic Django Settings
DEBUG = False
ALLOWED_HOSTS = ['*']
SECRET_KEY = 'CHANGE-THIS-IN-PRODUCTION-' + os.urandom(32).hex()

# OpenStack Backend Configuration
OPENSTACK_HOST = "lightstack"  # Docker service name (or IP if not using Docker)
OPENSTACK_KEYSTONE_URL = "http://%s:35357/v3" % OPENSTACK_HOST
OPENSTACK_KEYSTONE_DEFAULT_ROLE = "_member_"

# API Version Configuration (matches Lightstack)
OPENSTACK_API_VERSIONS = {
    "identity": 3,      # Keystone v3
    "image": 2,         # Glance v2
    "volume": 3,        # Cinder v3
    "compute": 2.1,     # Nova v2.1
}

# Keystone v3 Configuration
OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True
OPENSTACK_KEYSTONE_DEFAULT_DOMAIN = "Default"

# Session Configuration (matches Lightstack token TTL)
SESSION_TIMEOUT = 14400  # 4 hours
SESSION_COOKIE_SECURE = False  # Set True with HTTPS in production
CSRF_COOKIE_SECURE = False     # Set True with HTTPS in production

# Console Access Configuration
# 'VNC', not 'novnc': Horizon's CONSOLES keys are AUTO/VNC/SPICE/RDP/SERIAL/MKS,
# so 'novnc' raises KeyError and the Console tab reports "unavailable" without ever
# calling Nova (SPEC-021 / #534).
CONSOLE_TYPE = 'VNC'

# Regional Settings
OPENSTACK_KEYSTONE_REGION_NAME = "RegionOne"
TIME_ZONE = "UTC"

# Service Feature Configuration
OPENSTACK_CINDER_FEATURES = {
    'enable_backup': False,  # Disable if not using Cinder backup
}

OPENSTACK_NEUTRON_NETWORK = {
    'enable_router': True,
    'enable_quotas': True,
    'enable_ipv6': False,
    'enable_distributed_router': False,
    'enable_ha_router': False,
    'enable_fip_topology_check': True,
}

# Logging Configuration
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
        'django': {
            'handlers': ['console'],
            'level': 'INFO',
        },
        'horizon': {
            'handlers': ['console'],
            'level': 'INFO',
        },
        'openstack_dashboard': {
            'handlers': ['console'],
            'level': 'INFO',
        },
    },
}

# Cache Configuration
CACHES = {
    'default': {
        'BACKEND': 'django.core.cache.backends.locmem.LocMemCache',
    }
}
```

#### Step 3: Create Docker Compose Configuration

Create `docker-compose.yml`:

```yaml
version: '3.8'

services:
  horizon:
    image: quay.io/openstack.kolla/horizon:2023.2-ubuntu-jammy
    container_name: lightstack-horizon
    hostname: horizon
    ports:
      - "80:80"
    environment:
      - KOLLA_INSTALL_TYPE=source
      - KOLLA_CONFIG_STRATEGY=COPY_ALWAYS
    volumes:
      - ./kolla-config:/var/lib/kolla/config_files:ro
      - horizon-static:/var/lib/kolla/venv/lib/python3.10/site-packages/static:rw
      - horizon-logs:/var/log/kolla/horizon:rw
    networks:
      - lightstack-network
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/dashboard/auth/login/"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 60s

networks:
  lightstack-network:
    external: true
    name: deployments_lightstack-network  # Must match Lightstack network name

volumes:
  horizon-static:
  horizon-logs:
```

#### Step 4: Create Kolla Configuration Structure

Create the Kolla configuration directory and files:

```bash
mkdir -p kolla-config
```

Create `kolla-config/config.json`:

```json
{
    "command": "/usr/sbin/apache2ctl -DFOREGROUND",
    "config_files": [
        {
            "source": "/var/lib/kolla/config_files/local_settings",
            "dest": "/etc/openstack-dashboard/local_settings.py",
            "owner": "horizon",
            "perm": "0644"
        }
    ],
    "permissions": [
        {
            "path": "/var/log/kolla/horizon",
            "owner": "horizon:horizon",
            "recurse": true
        },
        {
            "path": "/var/lib/kolla/venv/lib/python3.10/site-packages/static",
            "owner": "horizon:horizon",
            "recurse": true
        }
    ]
}
```

Copy your local_settings.py to the kolla-config directory:

```bash
cp local_settings.py kolla-config/local_settings
```

#### Step 5: Deploy Horizon

```bash
# Start services
docker compose up -d

# Check status
docker compose ps

# Watch logs
docker compose logs -f horizon
```

Wait for the health check to pass (may take 30-60 seconds).

#### Step 6: Access Dashboard

Open your browser to:
```
http://localhost/dashboard
```

**Login Credentials**:
- Domain: `Default`
- User Name: `admin`
- Password: `secret`

### Option 2: Development/Testing Setup

For quick testing without persistence, use this minimal configuration:

```yaml
version: '3.8'

services:
  horizon:
    image: quay.io/openstack.kolla/horizon:2023.2-ubuntu-jammy
    container_name: lightstack-horizon-dev
    ports:
      - "8080:80"
    environment:
      - KOLLA_INSTALL_TYPE=source
      - KOLLA_CONFIG_STRATEGY=COPY_ALWAYS
      - OPENSTACK_HOST=host.docker.internal  # For local Lightstack
      - OPENSTACK_KEYSTONE_URL=http://host.docker.internal:35357/v3
    volumes:
      - ./kolla-config:/var/lib/kolla/config_files:ro
    restart: "no"

networks:
  default:
    name: bridge
```

## Configuration Details

### Network Configuration

**Docker Network Name**: The `lightstack-network` must match your Lightstack deployment network. Check with:

```bash
docker network ls | grep lightstack
```

Common network names:
- `deployments_lightstack-network` (deployments/docker-compose.yml)
- `lightstack_default` (root docker-compose.yml)
- Custom name specified in your Lightstack compose file

Update `docker-compose.yml` accordingly:

```yaml
networks:
  lightstack-network:
    external: true
    name: YOUR_ACTUAL_NETWORK_NAME
```

### Service Discovery

Horizon resolves Lightstack services using the `OPENSTACK_HOST` setting. Options:

1. **Docker service name** (recommended): `OPENSTACK_HOST = "lightstack"`
   - Requires Horizon and Lightstack on same Docker network

2. **Host IP address**: `OPENSTACK_HOST = "192.168.1.100"`
   - Use for non-Docker Lightstack deployments

3. **Localhost (host.docker.internal)**: `OPENSTACK_HOST = "host.docker.internal"`
   - For Lightstack running on Docker host machine

### Console Access

The console proxy belongs to Lightstack, not to this Horizon deployment. Horizon only iframes the URL
Nova returns; in real mode that URL points at the proxy Lightstack deploys with itself (the chart's
`consoleProxy`, or the deploy scripts' `novnc` service), so nothing here runs one. Against a stub-mode
Lightstack the Console tab reports the console unavailable, which is correct: stub mode has no guest,
and the API refuses with `501`. Multi-node deploys no proxy in v1, and the API refuses with `400`
(SPEC-021). Horizon needs only:

```python
# 'VNC', not 'novnc': Horizon's CONSOLES keys are AUTO/VNC/SPICE/RDP/SERIAL/MKS,
# so 'novnc' raises KeyError and the Console tab reports "unavailable" without ever
# calling Nova (SPEC-021 / #534).
CONSOLE_TYPE = 'VNC'
```

## Verification

### 1. Health Checks

```bash
# Check container status
docker compose ps

# Verify Horizon is responding
curl -I http://localhost/dashboard/auth/login/

# Check Lightstack connectivity from Horizon
docker exec lightstack-horizon curl -s http://lightstack:35357/v3 | jq
```

### 2. Login Test

1. Navigate to `http://localhost/dashboard`
2. Enter credentials (Domain: Default, User: admin, Password: secret)
3. Verify successful login to Overview page

### 3. Feature Testing

Test key Horizon features:

```bash
# Instance Management
# - Navigate to Compute > Instances
# - Click "Launch Instance"; give it a name, then pick an image (Source), a flavor and
#   a network (create one under Network > Networks first if the project has none)
# - Verify the instance is listed and reaches Active
# - The wizard's default "Create New Volume: Yes" boots from the image without creating
#   a volume (finding F16, see the compatibility report)

# Network Topology
# - Navigate to Network > Network Topology
# - Verify topology visualization loads

# VNC Console (SPEC-021 -- finding F1 is fixed)
# - Launch or select an existing instance
# - Click instance name > Console tab
# - Real mode, single node: the guest's framebuffer, taking keyboard input
# - Stub mode or multi-node: "Console currently not available" is expected --
#   stub has no guest, multi-node deploys no console proxy in v1
# - The serial log works in every mode:
#     openstack console log show <server>

# Volume Management
# - Navigate to Volumes > Volumes
# - Click "Create Volume", enter a name and a size, and submit
# - Verify the volume is listed
```

## Troubleshooting

### Container Restart Loops

**Symptom**: `docker compose ps` shows "Restarting"

**Diagnosis**:
```bash
docker logs lightstack-horizon --tail 50
```

**Common Issues**:

1. **KOLLA_CONFIG_STRATEGY not set**:
   ```
   ERROR: KOLLA_CONFIG_STRATEGY is not set properly
   ```
   Solution: Add to docker-compose.yml environment:
   ```yaml
   - KOLLA_CONFIG_STRATEGY=COPY_ALWAYS
   ```

2. **Missing config.json**:
   ```
   FileNotFoundError: /var/lib/kolla/config_files/config.json
   ```
   Solution: Ensure `kolla-config/config.json` exists and is mounted

3. **Apache fails to start**:
   ```
   AH00015: Unable to open logs
   ```
   Solution: Add named volume for logs:
   ```yaml
   volumes:
     - horizon-logs:/var/log/kolla/horizon:rw
   ```

### Connection Refused

**Symptom**: Horizon shows "Unable to retrieve ..." errors

**Diagnosis**:
```bash
# From Horizon container
docker exec lightstack-horizon curl -v http://lightstack:35357/v3

# Check network connectivity
docker exec lightstack-horizon ping -c 3 lightstack
```

**Solutions**:

1. **Wrong network**: Verify both containers on same network:
   ```bash
   docker network inspect deployments_lightstack-network
   ```

2. **Wrong OPENSTACK_HOST**: Update `local_settings.py`:
   ```python
   OPENSTACK_HOST = "correct-service-name"
   ```

3. **Lightstack not running**: Start Lightstack services:
   ```bash
   cd ~/git/lightstack
   docker compose -f deployments/docker-compose.yml up -d
   ```

### Authentication Errors

**Symptom**: "Unable to authenticate" on login

**Checks**:

1. Verify Lightstack Keystone is responsive:
   ```bash
   curl http://localhost:35357/v3
   ```

2. Verify default user exists:
   ```bash
   export OS_AUTH_URL=http://localhost:35357/v3
   export OS_USERNAME=admin
   export OS_PASSWORD=secret
   export OS_PROJECT_NAME=default
   export OS_USER_DOMAIN_NAME=Default
   export OS_PROJECT_DOMAIN_NAME=Default

   openstack token issue
   ```

3. Check Horizon logs for authentication errors:
   ```bash
   docker logs lightstack-horizon | grep -i auth
   ```

### VNC Console Issues

**Symptom**: Console tab shows "unavailable" / "Unable to load Console" / "Unable to connect"

- **Stub mode, or multi-node**: expected. There is no guest (stub, `501`) or no proxy (multi-node,
  `400 Unavailable console type novnc`), and the tab says so.
- **Real mode, single node**: run `openstack console url show <server>` first. An error names the cause
  (`409` not running, `500` with the reason in Lightstack's log); a URL means the rest is between the
  browser and the proxy — its log shows `connecting to unix socket: …` for a token it resolved. The
  step-by-step checks are in `docs/specs/021-real-novnc-console/quickstart.md`.
- `CONSOLE_TYPE` must be `'VNC'`: `'novnc'` is not a Horizon `CONSOLES` key and fails before Nova is
  ever called.

**Fallback** — the serial console log works in every mode:

```bash
openstack console log show test-vm
```

### Performance Issues

**Symptom**: Dashboard is slow to load

**Solutions**:

1. **Increase container resources**:
   ```yaml
   services:
     horizon:
       deploy:
         resources:
           limits:
             memory: 2G
             cpus: '2'
   ```

2. **Enable caching**: Update `local_settings.py`:
   ```python
   CACHES = {
       'default': {
           'BACKEND': 'django.core.cache.backends.memcached.PyMemcacheCache',
           'LOCATION': 'memcached:11211',
       }
   }
   ```

   Add memcached service to docker-compose.yml:
   ```yaml
   memcached:
     image: memcached:alpine
     container_name: lightstack-memcached
     networks:
       - lightstack-network
   ```

3. **Reduce session timeout**: Lower SESSION_TIMEOUT in `local_settings.py`

## Production Considerations

### Security

1. **Change SECRET_KEY**: Generate unique secret:
   ```python
   SECRET_KEY = os.urandom(64).hex()
   ```

2. **Enable HTTPS**: Use reverse proxy (nginx, traefik) with TLS certificates:
   ```yaml
   services:
     nginx:
       image: nginx:alpine
       ports:
         - "443:443"
       volumes:
         - ./nginx.conf:/etc/nginx/nginx.conf:ro
         - ./certs:/etc/nginx/certs:ro
   ```

3. **Restrict ALLOWED_HOSTS**:
   ```python
   ALLOWED_HOSTS = ['horizon.example.com', '10.0.1.50']
   ```

4. **Enable secure cookies**:
   ```python
   SESSION_COOKIE_SECURE = True
   CSRF_COOKIE_SECURE = True
   ```

### High Availability

For production HA setup:

1. **Multiple Horizon instances**: Run 2+ Horizon containers behind load balancer
2. **Shared session storage**: Use Redis/Memcached for sessions
3. **Persistent volumes**: Use network storage for static files
4. **Health checks**: Configure load balancer health checks

Example with Redis sessions:

```python
SESSION_ENGINE = 'django.contrib.sessions.backends.cache'
CACHES = {
    'default': {
        'BACKEND': 'django_redis.cache.RedisCache',
        'LOCATION': 'redis://redis:6379/0',
    }
}
```

### Monitoring

Monitor Horizon health:

```bash
# Container metrics
docker stats lightstack-horizon

# Application logs
docker logs -f lightstack-horizon

# HTTP response times
curl -w "@curl-format.txt" -o /dev/null -s http://localhost/dashboard/

# Health endpoint
watch -n 10 'curl -f http://localhost/dashboard/auth/login/ || echo "DOWN"'
```

## Maintenance

### Updates

Update Horizon to newer version:

```bash
# Pull new image
docker compose pull horizon

# Recreate container
docker compose up -d horizon

# Verify
docker compose logs -f horizon
```

### Backups

Backup Horizon configuration:

```bash
tar czf horizon-backup-$(date +%Y%m%d).tar.gz \
  local_settings.py \
  docker-compose.yml \
  kolla-config/
```

### Cleanup

Remove Horizon deployment:

```bash
# Stop and remove containers
docker compose down -v

# Remove configuration
cd ~ && rm -rf lightstack-horizon
```

## Advanced Configuration

### Custom Branding

Customize Horizon appearance by mounting custom static files:

```yaml
volumes:
  - ./custom-logo.png:/var/lib/kolla/venv/lib/python3.10/site-packages/openstack_dashboard/static/dashboard/img/logo.png:ro
```

### Additional Panels

Install custom Horizon panels:

```bash
# Create Dockerfile
FROM quay.io/openstack.kolla/horizon:2023.2-ubuntu-jammy
RUN pip install horizon-custom-panel
COPY panel-config.py /etc/openstack-dashboard/enabled/
```

Build and use custom image:

```bash
docker build -t lightstack-horizon-custom .
```

Update docker-compose.yml:

```yaml
services:
  horizon:
    image: lightstack-horizon-custom
```

### Multi-Region Support

Configure Horizon for multiple Lightstack regions:

```python
AVAILABLE_REGIONS = [
    ('http://region1:35357/v3', 'RegionOne'),
    ('http://region2:35357/v3', 'RegionTwo'),
]
OPENSTACK_KEYSTONE_URL = AVAILABLE_REGIONS[0][0]
```

## Support

- **Documentation**: https://github.com/cobaltcore-dev/lightstack/tree/main/docs
- **Issues**: https://github.com/cobaltcore-dev/lightstack/issues
- **Spec**: [OpenStack Horizon 100% Compatibility](archive/specs/002-horizon-full-compatibility/)

## References

- OpenStack Horizon Documentation: https://docs.openstack.org/horizon/latest/
- Kolla Documentation: https://docs.openstack.org/kolla/latest/
- Lightstack API Documentation: [docs/API.md](./API.md)
- Lightstack Configuration Guide: [docs/CONFIGURATION.md](./CONFIGURATION.md)
