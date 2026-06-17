# Quickstart: Verify SPEC-007 Helm Prerequisites

Manual verification of both workstreams against a running O3K (stub mode is sufficient — neither feature needs Linux/real mode).

## Prerequisites

```bash
make db-up                 # PostgreSQL
# build after implementing
make build                 # -> bin/o3k
```

## WS-2 — Admin-password bootstrap

### A. Default name, configured password (the common case)

```bash
# config/o3k.yaml:
#   keystone:
#     admin_user: admin          # optional; defaults to admin
#     admin_password: "S3cr3t-Changed!"

bin/o3k --config config/o3k.yaml   # watch logs for: "admin password provisioned" (no value printed)

# configured password works:
OS_PASSWORD="S3cr3t-Changed!" openstack token issue          # -> succeeds
# old default rejected:
OS_PASSWORD="secret"          openstack token issue          # -> 401
```

### B. Idempotent restart / no clobber

```bash
# Restart with the SAME config: still works, no duplicate/errors.
# Rotate via the API (NOT config), then restart with config unchanged:
openstack user password set --password "Rotated!" --original-password "S3cr3t-Changed!"
bin/o3k --config config/o3k.yaml     # restart
OS_PASSWORD="Rotated!"        openstack token issue           # -> still succeeds (config did NOT overwrite)
OS_PASSWORD="S3cr3t-Changed!" openstack token issue           # -> 401
```

### C. Custom admin user (seeded `admin` neutralized)

```bash
#   keystone: { admin_user: ops, admin_password: "OpsPass!" }
bin/o3k --config config/o3k.yaml
OS_USERNAME=ops   OS_PASSWORD="OpsPass!" openstack token issue   # -> succeeds, has admin role on 'default'
OS_USERNAME=admin OS_PASSWORD="secret"   openstack token issue   # -> 401 (seeded admin enabled=false)
```

### D. No password configured (dev default + warning)

```bash
#   keystone: { }   # no admin_password
bin/o3k --config config/o3k.yaml     # logs a PROMINENT warning about the well-known default
OS_PASSWORD="secret" openstack token issue                      # -> still succeeds (binary does not block; chart does)
```

**Check**: the admin password value never appears in any log line (SC-008).

## WS-3 — Configurable catalog scheme

```bash
# default / unset -> http
unset O3K_ENDPOINT_SCHEME
bin/o3k --config config/o3k.yaml
openstack catalog list -f json | grep -o '"url":[^,]*' | head     # -> http://...

# https
O3K_ENDPOINT_SCHEME=https O3K_ENDPOINT_HOST=o3k.example.com bin/o3k --config config/o3k.yaml
openstack catalog list -f json | grep -o '"url":[^,]*' | head     # -> https://o3k.example.com:...

# invalid -> warn once, fall back to http (does not crash)
O3K_ENDPOINT_SCHEME=ftp bin/o3k --config config/o3k.yaml          # logs one warning; catalog uses http://

# self-links / pagination honor the scheme too (FR-013a), not just the catalog
O3K_ENDPOINT_SCHEME=https O3K_ENDPOINT_HOST=o3k.example.com bin/o3k --config config/o3k.yaml
openstack server list --long -f json | grep -o 'https\?://[^"]*' | head   # -> https:// in links
```

**Check**: between `http` and `https` runs the catalog is identical except the URL scheme — same services, interfaces, hosts, ports, paths (SC-006); resource self-links also flip to `https://`.

**Note**: the advertised URLs are `https://<host>:<service-port>` (port kept). They only *resolve* end-to-end when TLS is terminated **per service port** (port-based exposure). In plain `bin/o3k` runs you're inspecting the advertised scheme, not reaching a TLS listener; the working-TLS path is the SPEC-008 chart's job.

## Regression gate

```bash
go test ./internal/keystone/...        # unit
./scripts/test-all.sh                  # integration
# SPEC-000 contract tests must pass with both workstreams enabled (SC-007, FR-016)
```
