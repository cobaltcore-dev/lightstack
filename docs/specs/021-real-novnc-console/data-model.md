# Phase 1 Data Model: a real console / noVNC (SPEC-021)

**Feature**: `021-real-novnc-console` | **Date**: 2026-07-30 | **Migration**: `065_console_auth_tokens`

## Entities

### Console token — `console_auth_tokens` (new table, store of record)

One row per issued console link. Short-lived by construction: created on a console request, deleted on expiry or on any operation that could move the instance's console endpoint.

| Column | Type | Constraints | Purpose |
|---|---|---|---|
| `token` | `text` | **PK** | The opaque value carried in the console URL. Hex-encoded — deliberately, because the projection's parser splits on `": "` and hex cannot contain it (research.md R4). |
| `instance_id` | `uuid` | `NOT NULL`, `REFERENCES instances(id) ON DELETE CASCADE` | The single instance this token opens. The cascade gives FR-014's delete case for free. |
| `project_id` | `uuid` | `NOT NULL` | The owning project, for the compatibility view and for audit. Issuance authorization happens before insert (FR-013). |
| `host` | `text` | `NOT NULL` | Where the guest console listens. Loopback in the supported topology (FR-017); stored because the ticket asks for it and it costs nothing. |
| `port` | `integer` | `NOT NULL`, `CHECK (port > 0)` | The port libvirt actually assigned, read per request (FR-004). Never derived. |
| `created_at` | `timestamptz` | `NOT NULL DEFAULT now()` | Issue time, for observability (FR-028). |
| `expires_at` | `timestamptz` | `NOT NULL` | Refusal boundary (FR-010). Enforced by deleting the projection entry — see the invariant below. |

**Indexes**

| Index | Rationale |
|---|---|
| PK on `token` | The compatibility view's lookup (`os-console-auth-tokens/{token}`). |
| `(instance_id)` | Revocation on stop/start/reboot/resume — a delete-by-instance (FR-014). |
| `(expires_at)` | The reconciler's sweep, so reaping stays a single indexed statement rather than a scan. |

**Why a table rather than the files alone**: the projection format carries only `token: host:port`. Expiry, instance, and project — required by FR-010, FR-014 and FR-013 — have nowhere to live in it.

**No `console_type` column.** An earlier draft carried one "so a second supported type needs no schema change" — future-proofing for a type FR-025 explicitly refuses, which Article VII (YAGNI, no future-proofing) forbids. The response echoes the *requested* type from the request body, so nothing needs it persisted. Adding it back is one migration on the day a second console type is actually supported.

### Console endpoint (not persisted)

Where a running instance's framebuffer actually listens: `host` + `port`, read from the hypervisor on **every** console request (FR-004) and never cached on the instance row. It is a property of the running domain, not of its record; libvirt may assign a different port each start. It reaches the database only as a snapshot inside a token row, which is why token rows must die when the endpoint can move.

### Token projection (derived state, on disk)

`/var/lib/o3k/console-tokens/` — one file per live token, contents `<token>: <host>:<port>`. Fully described in [contracts/console-token-projection.md](./contracts/console-token-projection.md).

**Invariant**: the table is authoritative; the directory is derived, and **fail-closed** — losing it stops tokens from working, never the reverse. The dangerous direction is a file outliving its row, because websockify has no expiry check and will resolve whatever it finds. Every deletion therefore removes the file **before or with** the row, never after.

## State transitions

```text
                    ┌──────────────────────────────────────────────┐
   console request  │                                              │
   (authorized)     ▼                                              │
        ──────►  ISSUED ──────────────────────────────────────►  GONE
                 row + file             expiry reaped          (no row,
                                        │  instance stopped/    no file)
                                        │  started/rebooted/
                                        │  resumed/deleted
                                        └──────────────────────────┘
```

There is no `USED` state. The proxy resolves tokens from the directory without telling O3K, so a connection is not observable and cannot be counted — which is why single-use semantics are explicitly not required (FR-010a) and why a token stays usable for reconnects throughout its window.

**Transition rules**

| Trigger | Effect |
|---|---|
| Authorized console request | Insert row, write file. Opportunistically delete this instance's already-expired tokens in the same path. |
| `expires_at` passes | Reconciler deletes file then row. Effective validity is `TTL + reconcile interval` (plan.md, Complexity Tracking). |
| `os-stop`, `os-start`, `reboot`, suspend, resume | Delete all of that instance's tokens — the endpoint may move, and a stale entry could resolve to a port since reassigned to a **different** instance. |
| Instance deleted | Same, via the FK cascade for rows, plus explicit file removal. |
| Service start | One sweep, so a long-idle deployment does not retain stale entries. |

## Columns removed

Migration `065` drops two columns added by `005_add_compute_features`, both provably dead:

| Column | Why it goes |
|---|---|
| `instances.console_vnc_port` | Was `5900 + (hash(instance_id) mod 1000)` — fabricated. The real port is now read per request (FR-004), so nothing reads or writes this. |
| `instances.console_vnc_password` | Generated and stored on every console request since `005` and **never applied to a domain** — the graphics element carries no password attribute. Dead on arrival. |

**Reversibility** (Constitution): `down` re-adds both as nullable, matching `005`. Contents are not restored; they were fabricated, so there is nothing to preserve.

## Validation rules

| Rule | Source |
|---|---|
| A token resolves to exactly one instance; the caller cannot name a target at connect time. | FR-012 |
| A token is issued only for the caller's project, unless the caller holds `admin`. | FR-013 |
| `expires_at > created_at`, bounded by the configured TTL (default 10 minutes). | FR-010 |
| `port` must come from the hypervisor; a request that cannot obtain one is refused rather than defaulted. | FR-003, FR-006 |
| No row may exist without its file, and no file without its row, except transiently during deletion — where the file goes first. | FR-023b |
| Token values are never logged in reusable form. | FR-028 |

## Configuration (not an entity, but new persisted operator state)

| Setting | Default | Purpose |
|---|---|---|
| `nova.console_proxy_base_url` | derived from the endpoint settings | Browser-reachable **origin** only; the client page and token query are appended by code (FR-020). |
| `nova.console_token_ttl` | `10m` | Token lifetime (FR-010). |
| `nova.console_reconcile_interval` | `30s` | Reaper period; sets how far past expiry a token can survive. |
