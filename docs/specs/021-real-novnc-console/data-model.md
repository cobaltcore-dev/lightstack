# Phase 1 Data Model: a real console / noVNC (SPEC-021)

**Feature**: `021-real-novnc-console` | **Date**: 2026-07-30, amended 2026-10-01 | **Migration**: `065_console_auth_tokens` (edited in place: nothing is deployed)

## Entities

### Console token — `console_auth_tokens` (new table, store of record)

One row per issued console link. Short-lived by construction: created on a console request, deleted on expiry or on any operation that could move the instance's console endpoint.

| Column | Type | Constraints | Purpose |
|---|---|---|---|
| `token` | `text` | **PK** | The opaque value carried in the console URL. Hex-encoded — deliberately, because the projection's parser splits on `": "` and hex cannot contain it (research.md R4). |
| `instance_id` | `uuid` | `NOT NULL`, `REFERENCES instances(id) ON DELETE CASCADE` | The single instance this token opens. The cascade gives FR-014's delete case for free. |
| `project_id` | `uuid` | `NOT NULL` | The owning project, for the compatibility view and for audit. Issuance authorization happens before insert (FR-013). |
| `socket` | `text` | `NOT NULL`, `CHECK (socket LIKE '/%')` | The guest's VNC unix socket, at its fixed per-instance path `<state_path>/console-sockets/<instance-uuid>.sock` (FR-017). It is read back from libvirt per request (FR-004), never derived, and is never another guest's. Replaces the `host` + `port` of the original design. |
| `domain_id` | `integer` | `NOT NULL`, `CHECK (domain_id > 0)` | The libvirt domain ID of the run the token was issued for (FR-009). libvirt assigns a new ID on every start, so a token whose guest now runs under another ID is revoked by the sweep, even though its socket path is unchanged (FR-014a). |
| `created_at` | `timestamptz` | `NOT NULL DEFAULT now()` | Issue time, for observability (FR-028). |
| `expires_at` | `timestamptz` | `NOT NULL` | Refusal boundary (FR-010). Enforced by deleting the projection entry — see the invariant below. |

**Indexes**

| Index | Rationale |
|---|---|
| PK on `token` | The compatibility view's lookup (`os-console-auth-tokens/{token}`). |
| `(instance_id)` | Revocation on every endpoint-moving operation — a delete-by-instance (FR-014). |
| `(expires_at)` | The reconciler's sweep, so reaping stays a single indexed statement rather than a scan. |

**Why a table rather than the files alone**: the projection format carries only `token: unix_socket:<path>`. Expiry, instance, project and run (FR-010, FR-014, FR-013, FR-014a) have nowhere to live in it.

**No `console_type` column.** An earlier draft carried one "so a second supported type needs no schema change" — future-proofing for a type FR-025 explicitly refuses, which Article VII (YAGNI, no future-proofing) forbids. The response echoes the *requested* type from the request body, so nothing needs it persisted. Adding it back is one migration on the day a second console type is actually supported.

### Console endpoint (not persisted)

Where a running instance's framebuffer actually listens: its unix socket (a fixed per-instance path) plus the domain ID of the current run. Both are read from the hypervisor on **every** console request (FR-004) and never cached on the instance row. The path stays the same across restarts; the domain ID changes on every start. They reach the database only as a snapshot inside a token row, which is why the reconciler compares each token with the socket and domain ID its guest has now.

### Token projection (derived state, on disk)

`<state_path>/console-tokens/` — one file per live token, contents `<token>: unix_socket:<path>`. Fully described in [contracts/console-token-projection.md](./contracts/console-token-projection.md).

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
                                        │  resumed/deleted, or its
                                        │  guest's socket changed
                                        └──────────────────────────┘
```

There is no `USED` state. The proxy resolves tokens from the directory without telling Lightstack, so a connection is not observable and cannot be counted — which is why single-use semantics are explicitly not required (FR-010a) and why a token stays usable for reconnects throughout its window.

**Transition rules**

| Trigger | Effect |
|---|---|
| Authorized console request | Insert row, write file. Opportunistically delete this instance's already-expired tokens in the same path. |
| `expires_at` passes | Reconciler deletes file then row. Effective validity is `TTL + reconcile interval` (plan.md, Complexity Tracking). |
| `os-stop`, `os-start`, `reboot`, suspend, resume, shelve | Delete all of that instance's tokens, so a link never outlives the console it was issued for. |
| Instance deleted (incl. `forceDelete`) | Same, via the FK cascade for rows, plus explicit file removal. |
| Reconciler sweep (real mode) | Delete every token whose guest now serves another socket, runs under another domain ID, or is not running. These are the moves that never reach the API: a guest powering itself off, a crash, a restart such as `virsh start`. A readback error keeps them for the next sweep. |
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
| `socket` and `domain_id` must come from the hypervisor; a request that cannot obtain them is refused rather than defaulted. A socket outside `console-sockets` (a domain defined before 2026-10-01) is refused as misconfigured. | FR-003, FR-006, FR-017 |
| No row may exist without its file, and no file without its row, except transiently during deletion — where the file goes first. | FR-023b |
| Token values are never logged in reusable form. | FR-028 |

### Console socket directory (node-local, prepared by Lightstack)

`<state_path>/console-sockets/`. It holds nothing but the guests' VNC sockets, which QEMU itself creates and removes on an orderly close.

| Property | Value | Source |
|---|---|---|
| Owner | QEMU's uid:gid, from libvirt's capabilities (`secmodel` `dac`, `baselabel type='kvm'`, else `type='qemu'`) | FR-017a, research R15 |
| Mode | `0750`: QEMU creates and removes sockets; the `kvm` group can read but not write; the proxy reaches it via `DAC_OVERRIDE`, read-only | FR-017a, FR-023c |
| Prepared | at real-mode startup, right after the libvirt connection; until ready, every guest start is refused | FR-017a, research R16 |
| Mounted | into Lightstack at the same host path by every real-mode artifact; into the proxy read-only | FR-017a, FR-023c |
| Removed | by the uninstall purge, with `console-tokens` | FR-017b |

## Configuration (not an entity, but new persisted operator state)

| Setting | Default | Purpose |
|---|---|---|
| `nova.console_proxy_base_url` | derived from the endpoint settings | Browser-reachable **origin** only; the client page and token query are appended by code (FR-020). |
| `nova.console_token_ttl` | `10m` | Token lifetime (FR-010). |
| `nova.console_reconcile_interval` | `30s` | Reaper period; sets how far past expiry a token can survive. |
| `state_path` (existing) | `/var/lib/lightstack` | In real mode at most **50** characters, so a socket path stays within QEMU's 108-byte limit (FR-017, research R13). |
