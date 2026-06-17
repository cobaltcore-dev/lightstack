# Phase 1 Data Model: SPEC-007 Helm Prerequisites

**No schema changes.** This feature reads/writes existing tables and reads runtime config. This document records the entities the bootstrap touches, the fixed seed identifiers it relies on, and the state transitions it performs.

## Configuration inputs (not persisted)

| Input | Source | Type | Default | Used by |
|-------|--------|------|---------|---------|
| `admin_user` | `config.go` → `KeystoneConfig.AdminUser` (yaml) | string | `admin` (when empty) | WS-2 |
| `admin_password` | `config.go` → `KeystoneConfig.AdminPassword` (yaml) | string (secret) | _unset_ → boot with default + warn | WS-2 |
| `O3K_ENDPOINT_SCHEME` | environment | `http` \| `https` (case-insensitive, trimmed) | `http` | WS-3 |
| `O3K_ENDPOINT_HOST` | environment (existing) | string | `localhost` | WS-3 (composes) |

`KeystoneConfig.AdminUser` / `AdminPassword` already exist (`internal/common/config.go:48-49`) — no struct change. `O3K_ENDPOINT_SCHEME` is read directly from the environment (mirrors `O3K_ENDPOINT_HOST`) — no struct field.

## Entities (existing tables — unchanged schema)

### User — `users` (`001_initial_schema`, `domain_id` added in `008_add_domains`)
- `id` UUID PK, `name` VARCHAR UNIQUE, `password_hash` VARCHAR (bcrypt), `enabled` BOOLEAN DEFAULT true, `domain_id` UUID → `domains(id)`
- **Seed admin** (`002_seed_data`): `id=00000000-…-0001`, `name='admin'`, `password_hash=bcrypt('secret')`, `enabled=true`; `domain_id` backfilled to `Default` (`…0100`) by `009`
- Login filters by `(name, domain_id)` (`auth.go:168`) and rejects `enabled=false` (`auth.go:179`)
- WS-2 writes: `password_hash` (default-name path), or inserts a new user **with `domain_id=…0100`** (custom name), or sets `enabled=false` (seeded admin when custom name)

### Domain — `domains` (`008_add_domains`, seeded `009`)
- `id` UUID PK, `name` UNIQUE — **Seed `Default`**: `id=00000000-…-0100`
- WS-2 reads only (target `domain_id` for a created custom admin; login resolves users within this domain)

### Project — `projects`
- `id` UUID PK, `name` UNIQUE — **Seed `default`**: `id=00000000-…-0002`
- WS-2 reads only (target project for the admin role grant)

### Role — `roles`
- `id` UUID PK, `name` UNIQUE — **Seed `admin`**: `id=00000000-…-0003`
- WS-2 reads only

### RoleAssignment — `role_assignments`
- PK `(user_id, project_id, role_id)`
- **Seed**: `(…0001 admin, …0002 default, …0003 admin)`
- WS-2 writes: inserts `(custom_user_id, …0002, …0003)` when provisioning a custom admin (FR-004b)

### Service / Endpoint — `services`, `endpoints` (`040_keystone_catalog`)
- `endpoints`: `interface` (only `public` seeded), `url`, `region`
- Stored `url` is `http://o3k:<port>/...` (seeded `localhost` in `040`, rewritten to `o3k:` by `052_fix_endpoint_hostnames`); the existing `O3K_ENDPOINT_HOST` rewrite maps `http://o3k:` → `http://<host>:`
- WS-3 **reads only** and rewrites the URL *scheme at presentation time* (after the host rewrite); rows are never modified (FR-013)

## State transitions — admin provisioning (WS-2)

```
start
 │  admin_password configured?
 ├─ no  → log prominent warning; leave seed admin (admin/secret live for dev); DONE
 └─ yes
     │  admin_user == "admin" (or empty)?
     ├─ yes → stored hash already verifies cfg password?
     │         ├─ yes → no-op (steady state; also covers admin_password=secret)
     │         └─ no → stored hash verifies default 'secret'?
     │                  ├─ yes → UPDATE users SET password_hash=bcrypt(cfg)
     │                  │         WHERE id=…0001 AND password_hash=<observed>  (optimistic, idempotent)
     │                  │         + log "admin password provisioned" (no value)
     │                  └─ no  → leave untouched (operator/API-rotated)   ← no clobber
     └─ no (custom name)
            INSERT users(name=cfg.user, password_hash=bcrypt(cfg), enabled=true, domain_id=…0100) ON CONFLICT (name) DO NOTHING
            INSERT role_assignments(new_user, …0002, …0003) ON CONFLICT DO NOTHING
            UPDATE users SET enabled=false WHERE name='admin'                  (neutralize seed)
            + log "admin <name> provisioned" (no value)
```

**Invariants**
- Idempotent on every start; once a non-default password exists, the password is never overwritten (FR-002, FR-003).
- Concurrency-safe via `ON CONFLICT` + optimistic `UPDATE … WHERE password_hash=<observed>`; concurrent nodes converge to one admin (FR-008a).
- Admin role/project scoping is established only for the configured admin; no other identity changes (FR-007).
- Password value is never written to logs (FR-006b).

## State — catalog scheme (WS-3)

Stateless transform at presentation time. A shared `internal/common` helper resolves the scheme once (`EndpointScheme()`, warn-once on invalid) and `ForceScheme(url, scheme)` rewrites the leading scheme of every advertised URL — applied in the Keystone catalog paths (`substituteURLTemplates`, `buildHardcodedCatalog`) **and** in `common.BaseURL` self-links / pagination (FR-013a). No persisted state; process-fixed, so the 24h per-project catalog cache is unaffected.

The advertised form is `https://<host>:<service-port>` (scheme + host only; port kept). This resolves only under **per-service-port TLS** (port-based exposure); the SPEC-008 chart owns that (see spec Assumptions → TLS exposure model).
