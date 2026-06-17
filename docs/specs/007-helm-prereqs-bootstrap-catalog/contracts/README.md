# Phase 1 Contracts: SPEC-007 Helm Prerequisites

This feature changes **no API surface** — it adjusts which credential authenticates and the *scheme* of catalog URLs. The contracts below are the observable invariants the implementation must preserve; they are exercised by the **SPEC-000 contract tests** (gophercloud / python-openstackclient) per FR-016, plus targeted unit/integration tests.

Affected endpoint (unchanged shape): **`POST /v3/auth/tokens`** (Keystone v3 identity) — returns a token and the service `catalog`.

---

## Contract C1 — Admin authentication after bootstrap (WS-2)

**Given** a deployment configured with `admin_password=<P>` (default `admin_user`), **after** first-boot provisioning:

| Request (password auth, user `admin`) | Expected |
|---|---|
| password = `<P>` | `201 Created`, token issued, scoped roles include `admin` on project `default` |
| password = `secret` | `401 Unauthorized` |

**Custom `admin_user=<U>`** (≠ `admin`):

| Request | Expected |
|---|---|
| user `<U>`, password `<P>` | `201 Created`, token issued, `admin` role on `default` |
| user `admin`, password `secret` | `401 Unauthorized` (seeded admin `enabled=false`) |

**No password configured**: `admin/secret` still authenticates (`201`), and a startup warning is emitted. (Install-blocking is the chart's job, not the binary's.)

**Invariants**: request/response JSON shape of `/v3/auth/tokens` is byte-identical to today; only *which* credential succeeds changes. Password value never appears in logs.

---

## Contract C2 — Service-catalog scheme (WS-3)

The `token.catalog[].endpoints[]` array is structurally identical regardless of scheme; only `url`'s scheme varies.

`O3K_ENDPOINT_SCHEME` unset / `http` (default):

```json
{ "interface": "public", "region": "RegionOne", "url": "http://<host>:9292" }
```

`O3K_ENDPOINT_SCHEME=https`:

```json
{ "interface": "public", "region": "RegionOne", "url": "https://<host>:9292" }
```

**Invariants**:
- Every endpoint URL across every service uses the configured scheme (FR-010); default and unset/invalid → `http` (FR-011, FR-014).
- The scheme also applies to resource **self-links / pagination** (`common.BaseURL`), not only the catalog (FR-013a).
- All other fields — service `type`, `name`, endpoint `interface`, `region`, `id`, host, port, path — are unchanged (FR-013). No microversion bump.
- Composes with `O3K_ENDPOINT_HOST` (host and scheme both reflected).

**Exposure note**: advertised URLs are `https://<host>:<service-port>` (port kept). They resolve only when the deployment terminates TLS **per service port** (port-based). The SPEC-008 chart owns that; see spec Assumptions → TLS exposure model.

---

## Verification mapping

| Contract | Verified by |
|----------|-------------|
| C1 | `internal/keystone/bootstrap_test.go` (unit: provision / idempotent / custom-user / no-password) + integration (`test/`): configure password → `openstack token issue` succeeds, `secret` rejected |
| C2 | `internal/keystone/auth_catalog_test.go` + `internal/common/baseurl_test.go` (unit: http default / https / invalid→http, catalog + self-links) + integration: `O3K_ENDPOINT_SCHEME=https` → `openstack catalog list` URLs and resource self-links are `https://` |
| C1 + C2 | **SPEC-000 contract tests** pass with both workstreams enabled (FR-016, SC-007) |
