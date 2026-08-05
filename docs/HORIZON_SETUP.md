# Horizon Setup Guide for Lightstack

**IMPORTANT**: Horizon dashboard integration is **extended functionality**, not a core Lightstack feature. Horizon requires independent setup, configuration, and (optionally) rebranding to work with lightstack.

---

## Overview

Lightstack serves the APIs the OpenStack Horizon dashboard consumes (its UI is **usable for resource management**; open gaps are listed in `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`). Horizon itself is:
- **Optional**: Lightstack works perfectly without Horizon (CLI/API/SDK are primary interfaces)
- **Independent**: Horizon runs as a separate service (Django application)
- **Configurable**: Requires specific configuration to connect to Lightstack
- **Rebrandable**: Can be customized with Lightstack branding (optional)

---

## Version Requirements

| Component | Version | Requirement |
|-----------|---------|-------------|
| Lightstack | v0.5.0+ | Required |
| OpenStack | Flamingo 2025.2+ | Required |
| Horizon | Flamingo 2025.2+ | Required |
| Earlier Versions | Zed, Yoga, etc. | **NOT SUPPORTED** |

**Why Flamingo only?**
- Lightstack is designed for modern OpenStack (2025.2+)
- Earlier versions use deprecated APIs and patterns
- Maintaining backward compatibility would compromise lightstack's lightweight design

---

## Deployment Options

### Option 1: Unified Deployment (Recommended)

Deploy Lightstack and Horizon together using `docker-compose-horizon.yml`:

```bash
cd lightstack/deployments
docker compose -f docker-compose-horizon.yml up -d
```

**What's included**:
- PostgreSQL 18 database
- Lightstack services (Keystone, Nova, Neutron, Cinder, Glance)
- Horizon Flamingo 2025.2
- noVNC console proxy
- Memcached (for Django sessions)

**Access**: http://localhost/dashboard

**Documentation**: [UNIFIED_DEPLOYMENT.md](UNIFIED_DEPLOYMENT.md)

---

### Option 2: Separate Horizon Deployment

Deploy Horizon separately to existing Lightstack installation:

```bash
cd lightstack/deployments
docker compose -f docker-compose-horizon.yml up -d horizon
```

**Prerequisites**:
- Lightstack services already running
- Lightstack accessible at known hostname/IP

**Documentation**: [HORIZON_DEPLOYMENT.md](HORIZON_DEPLOYMENT.md)

---

## Configuration

### Step 1: Horizon local_settings.py

Horizon must be configured to connect to Lightstack services:

```python
# deployments/horizon-config/local_settings
OPENSTACK_HOST = "lightstack"  # Docker service name or IP
OPENSTACK_KEYSTONE_URL = "http://%s:35357/v3" % OPENSTACK_HOST

# API Versions (Flamingo compatible)
OPENSTACK_API_VERSIONS = {
    "identity": 3,      # Keystone v3
    "image": 2,         # Glance v2
    "volume": 3,        # Cinder v3
    "compute": 2,       # Nova v2.1 (Horizon expects integer 2)
}

# Keystone Configuration
OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True
OPENSTACK_KEYSTONE_DEFAULT_DOMAIN = "Default"

# Session Configuration (matches lightstack token TTL)
SESSION_TIMEOUT = 14400  # 4 hours
SESSION_ENGINE = 'django.contrib.sessions.backends.cache'

# Console Access (CONSOLE_TYPE only -- Horizon has no console-proxy setting)
# 'VNC', not 'novnc': Horizon's CONSOLES keys are AUTO/VNC/SPICE/RDP/SERIAL/MKS,
# so 'novnc' raises KeyError and the Console tab reports "unavailable" without ever
# calling Nova (SPEC-021 / #534).
CONSOLE_TYPE = 'VNC'
```

**Key Configuration Points**:
- `OPENSTACK_HOST`: Must resolve to Lightstack service
- `OPENSTACK_API_VERSIONS`: Match Lightstack implemented versions
- `SESSION_TIMEOUT`: Should match Lightstack JWT token TTL (default 24h, recommended 4h for Horizon)
- `CONSOLE_TYPE`: One of Horizon's `CONSOLES` keys — `AUTO`, `VNC`, `SPICE`, `RDP`,
  `SERIAL`, `MKS`. Use `VNC` for Lightstack's noVNC console. Not `'novnc'`: that is not a key,
  so Horizon reports the console as unavailable without ever calling Nova.

---

### Step 2: Mount Configuration

Mount your configuration into the Horizon container:

```yaml
# docker-compose-horizon.yml
services:
  horizon:
    image: quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble
    volumes:
      - ./horizon-config/local_settings:/etc/openstack-dashboard/local_settings.py:ro
```

---

### Step 3: Network Configuration

Horizon must communicate with Lightstack services:

```yaml
# docker-compose-horizon.yml
services:
  horizon:
    environment:
      - OPENSTACK_HOST=lightstack
      - OPENSTACK_KEYSTONE_URL=http://lightstack:35357/v3
    networks:
      - lightstack-network  # Must be on same network as lightstack
```

---

## Optional: Horizon Rebranding

Horizon can be customized with Lightstack branding (logo, colors, name):

### Rebranding Script (Future)

A standalone rebranding script will be provided to:
- Replace OpenStack logos with Lightstack logos
- Update site title and branding text
- Customize color scheme
- Add custom CSS/JavaScript

**Status**: Planned for future release

**Current Workaround**: Use `local_settings.py` configuration:

```python
# Site branding
SITE_BRANDING = "lightstack Cloud Dashboard"
SITE_BRANDING_LINK = "/"

# Custom product name
CUSTOM_DASHBOARD_NAME = "lightstack Horizon Dashboard"
CUSTOM_PRODUCT_NAME = "lightstack OpenStack Platform"
CUSTOM_WELCOME_MESSAGE = "Welcome to lightstack - Lightweight OpenStack"
```

---

## Default Credentials

```text
Domain: Default
Username: admin
Password: secret
```

**⚠️ Change these credentials in production!**

---

## Verification

### Step 1: Access Horizon

Open http://localhost/dashboard (or your configured URL)

### Step 2: Login

Use default credentials or your configured credentials.

### Step 3: Test Functionality

**Project → Compute → Instances:**
- Launch instance
- Start/stop instance
- Access VNC console
- Delete instance

**Project → Network → Network Topology:**
- View network visualization
- Create network
- Create router

**Project → Volumes:**
- Create volume
- Attach volume to instance
- Create snapshot

---

## Troubleshooting

### Issue: "Connection to Keystone failed"

**Symptoms**: Login page loads but authentication fails

**Causes**:
- Lightstack services not running
- Incorrect `OPENSTACK_KEYSTONE_URL`
- Network connectivity issue

**Fix**:
```bash
# Verify lightstack is running
docker ps | grep lightstack

# Test Keystone endpoint
curl http://localhost:35357/v3

# Check Horizon logs
docker logs lightstack-horizon
```

---

### Issue: "API version mismatch"

**Symptoms**: Pages load but data doesn't display

**Causes**:
- Incorrect `OPENSTACK_API_VERSIONS` in `local_settings.py`
- Using non-Flamingo Horizon image

**Fix**:
```python
# Verify API versions match lightstack
OPENSTACK_API_VERSIONS = {
    "identity": 3,
    "image": 2,
    "volume": 3,
    "compute": 2,  # NOT 2.1 - Horizon expects integer
}
```

---

### Issue: "Session expired immediately after login"

**Symptoms**: Logged out immediately after authentication

**Causes**:
- `SESSION_TIMEOUT` too short
- Memcached not running
- Time sync issue between containers

**Fix**:
```python
# Increase session timeout
SESSION_TIMEOUT = 14400  # 4 hours

# Verify Memcached
docker ps | grep memcached

# Check container time sync
docker exec lightstack-horizon date
docker exec lightstack date
```

---

### Issue: "noVNC console not working"

**Symptoms**: VNC console button doesn't work or shows blank screen

**Causes**:
- noVNC proxy not running
- Incorrect `nova.console_proxy_base_url` in `lightstack.yaml` — it must be an origin the
  BROWSER can reach (Horizon has no proxy setting of its own; Lightstack returns the full URL)
- No console token issued — check with `lightstack console-tokens list`
- Nova not configured for console access

**Fix**:
```bash
# Verify noVNC proxy is running
docker ps | grep novnc

# Test noVNC endpoint
curl http://localhost:6080

# Check Nova console configuration (an ORIGIN — scheme://host:port, no path)
grep console_proxy_base_url config/lightstack.yaml

# Inspect the console tokens the proxy resolves against.
# In a container the binary is /app/lightstack and is not on $PATH.
docker exec lightstack /app/lightstack console-tokens list
```

---

## Known Limitations

### Identity Projects Page (404)

**Status**: Known Horizon/Kolla configuration issue (NOT a Lightstack bug)

**Symptoms**: `/dashboard/identity/projects/` returns HTTP 404

**Root Cause**: Django panel registration issue in Kolla Horizon image

**Workaround**: Use OpenStack CLI or API directly:
```bash
# List projects
openstack project list

# Create project
openstack project create --domain Default my-project

# Delete project
openstack project delete my-project
```

**Resolution**: This is a Horizon packaging issue, not a Lightstack API issue. The Keystone `/v3/projects` API endpoint works perfectly.

**Documentation**: [HORIZON_PROJECTS_ISSUE.md](HORIZON_PROJECTS_ISSUE.md)

---

## Performance Considerations

### Database Connection Pooling

Horizon makes many API calls per page load. Ensure Lightstack has sufficient database connections:

```yaml
# config/lightstack.yaml
database:
  max_open_conns: 50  # Increase for high Horizon traffic
  max_idle_conns: 10
```

---

### Session Caching

Use Memcached for Django sessions (NOT database backend):

```python
# local_settings.py
SESSION_ENGINE = 'django.contrib.sessions.backends.cache'

CACHES = {
    'default': {
        'BACKEND': 'django.core.cache.backends.memcached.PyMemcacheCache',
        'LOCATION': 'memcached:11211',
    }
}
```

---

## Security Recommendations

### Production Deployment

1. **Change default credentials**:
```bash
openstack user set --password NEW_PASSWORD admin
```

2. **Enable HTTPS**:
```yaml
# docker-compose-horizon.yml
services:
  horizon:
    environment:
      - SESSION_COOKIE_SECURE=True
      - CSRF_COOKIE_SECURE=True
```

3. **Restrict ALLOWED_HOSTS**:
```python
# local_settings.py
ALLOWED_HOSTS = ['your-domain.com']  # NOT ['*']
```

4. **Change JWT secret**:
```yaml
# config/lightstack.yaml
keystone:
  jwt_secret: "USE-STRONG-RANDOM-SECRET"
```

---

## Architecture

### Communication Flow

```
┌─────────────┐      HTTP      ┌──────────────┐      HTTP      ┌─────────────┐
│   Browser   │ ───────────────>│   Horizon    │ ───────────────>│     lightstack     │
│             │                 │  (Django)    │                 │  Services   │
└─────────────┘                 └──────────────┘                 └─────────────┘
                                      │                                 │
                                      │ Session                         │ JWT
                                      ↓                                 ↓
                                ┌──────────────┐                 ┌─────────────┐
                                │  Memcached   │                 │ PostgreSQL  │
                                └──────────────┘                 └─────────────┘
```

**Key Points**:
- Horizon is stateless (sessions in Memcached)
- Lightstack uses JWT tokens (no session database)
- All API calls go through Lightstack (Horizon → Lightstack → PostgreSQL)
- noVNC proxy provides console access (separate WebSocket connection)

---

## Summary

**Horizon is extended functionality**:
- Lightstack works perfectly without Horizon (CLI/API/SDK are primary)
- Horizon requires independent setup and configuration
- Version requirement: Flamingo 2025.2+ only
- Rebranding script (future): Optional customization for Lightstack branding

**Deployment Guides**:
- [UNIFIED_DEPLOYMENT.md](UNIFIED_DEPLOYMENT.md) - Complete setup
- [HORIZON_DEPLOYMENT.md](HORIZON_DEPLOYMENT.md) - Separate Horizon
- [HORIZON_INTEGRATION.md](HORIZON_INTEGRATION.md) - Architecture details
- [QUICK_REFERENCE.md](QUICK_REFERENCE.md) - Command cheatsheet

**Support**:
- API compatibility: broad. **UI workflows**: network, volume and instance create work; the noVNC console and a few other gaps remain — see the [Horizon Compatibility Report](compatibility/HORIZON_COMPATIBILITY_REPORT.md)
- Known issues: Identity projects page 404 (Horizon bug, not lightstack)
- Primary interfaces: OpenStack CLI, API, SDK
- Web UI: Optional Horizon dashboard
