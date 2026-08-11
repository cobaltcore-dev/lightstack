# Contract: network project ownership (US3 / B11)

**Covers**: FR-012, FR-013, FR-014, FR-015, FR-016, FR-017, FR-018
**Applies to**: `POST /v2.0/networks`, and the owner field of every network response

## Request

`CreateNetworkRequest.Network` gains both spellings, as pointers so absent is distinguishable:

```json
{ "network": { "name": "n1", "project_id": "<uuid>", "tenant_id": "<uuid>" } }
```

Upstream's handling, from `neutron_lib/api/attributes.py` (research R2, verified from source):

1. `populate_project_info` — one spelling present populates the other; both present and different →
   `400 "'project_id' and 'tenant_id' do not match"`.
2. `_validate_privileges` — value differs from the authenticated project and
   `not context.can_set_project_id` → `400 "Specifying 'project_id' other than the authenticated
   project in request is not allowed for the current user."`
3. `can_set_project_id` = `is_admin or is_service_role or …`. O3K has no service-account concept, so
   **administrator is the whole privileged set**.

## Decision table

| Caller | Body project | Outcome | Status |
|---|---|---|---|
| any | absent | owner = caller (today's behaviour) | `201` |
| any | equals caller's | owner = caller | `201` |
| **administrator** | another project, exists | **owner = that project** | `201` |
| administrator | another project, does not exist | refused | **`400`** |
| non-administrator | another project | refused | **`400`** |
| any | both spellings, disagreeing | refused | **`400`** |

All three refusals share `400` (FR-016) so a client has one rule rather than three. The refusal must
not disclose whether some other project exists.

> **Not `403`, and not symmetric with compute.** #549 refuses cross-project *server* creation
> outright. That is correct there and wrong here: upstream Nova has no project attribute on server
> create to honour, upstream Neutron does. Spec A10/A11 record this so the asymmetry is not later
> "corrected" into a regression.

## Contract tests

| ID | Given | When | Then |
|---|---|---|---|
| **N1** | administrator, second project `$OTHER` | `POST /v2.0/networks` with `project_id: $OTHER` | `201`; response `project_id` == `$OTHER`; **and the stored row's `project_id` == `$OTHER`** — assert against storage, not just the echo |
| **N2** | non-administrator, another project | same | `400`; no network created in either project |
| **N3** | any caller | `project_id` and `tenant_id` set to different values | `400` |
| **N4** | administrator, a random unused uuid as project | same | `400`, not `500` — the FK must never surface as a storage error |
| **N5** | administrator, a non-uuid string as project | same | `400` naming the field |
| **N6** | a network owned by project A, marked shared | tenant B lists networks | B sees it, reported with **A** as owner — not B |
| **N7** | a network owned by A | `GET /v2.0/networks/{id}` as A, then as an administrator | both report A |
| **N8** | the ticket's own probe | `openstack network create --project $OTHER b11-net` then `openstack network show b11-net -c project_id` | returns `$OTHER` |
| **N9** | a refused cross-project request | inspect logs | one entry naming the caller's project and the requested project |
| **N10** | `GetNetwork`'s 30-minute cache | A creates, B reads via shared, A reads | every read reports A — the cached entry must be caller-independent |

**N8 is the acceptance check that cannot pass on half the fix.** It reads back through `GetNetwork`,
which echoes the caller today (`network.go:760-761`) and does not even select `project_id`. Binding the
request without fixing the emitters leaves this red for a different reason than it is red now.

## Response

All four network emitters report the **row's** owner:

| Emitter | Location | Change |
|---|---|---|
| `CreateNetwork` | `network.go:512-513` | report the resolved owner, not the caller |
| `ListNetworks` | `network.go:686-687` | **add `project_id` to the `SELECT`**, then report it |
| `GetNetwork` | `network.go:760-761` | **add `project_id` to the `SELECT`**, then report it |
| `UpdateNetwork` | — | same |

Both `tenant_id` and `project_id` keys are emitted, carrying the same value (existing behaviour; the
legacy spelling stays).

The corrected shape already exists in this file for subnets — `ListSubnets:1157-1159` and
`GetSubnet:1202-1204`, with the comment "tenant_id from the row, not the caller". #545 fixed subnets
and left networks. Copy it.

## Sequencing note

This contract and `external-networks.md` modify the **same three response builders** and the **same
two `SELECT`s**. Sequence them; do not run them in parallel.
