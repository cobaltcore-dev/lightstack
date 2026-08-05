# Contract — Neutron network selectability in the launch wizard (F7)

**Service**: Networking (Neutron) v2.0, port 9696
**Requirements**: FR-007, FR-010
**Depends on**: #532 / SPEC-019 F4 (the deduplicated, filter-honoring network list) — merged to `main` as
`0a4dde0`, which this branch is based on.

> The launch wizard's Networks step shows "Available 2" but renders **no selectable rows**. F4 fixed the
> *duplication*; this contract makes the surviving rows *renderable and selectable*.

---

## C1. Network response — add `project_id` (FR-007)

Every network object (list, show, create) must carry `project_id` **alongside** the existing `tenant_id`.

**Response fields**

| Field | Today | Required |
|---|---|---|
| `id`, `name`, `status`, `admin_state_up`, `shared`, `mtu`, `created_at`, `updated_at` | present | unchanged |
| `router:external` | present (always `false` — SPEC-019) | unchanged |
| `subnets` | list of subnet **ids** | unchanged in shape (see C2) |
| `tenant_id` | present | **kept**, but now names the **owning** project rather than the caller (an FR-016 exception) |
| **`project_id`** | **absent** | **added** — same value as `tenant_id` |

**Consistency rule**: the network object is hand-built in **three** places that must stay identical —
`CreateNetwork` (`internal/neutron/network.go:522`), `ListNetworks` (`:704`), `GetNetwork` (`:809`). Adding the
field to only some of them is a defect; the tests assert all three.

---

## C2. Subnet-reference integrity (FR-007) — the core F7 fix

**Rule**: every id in a network's `subnets` array **MUST be resolvable** from the subnet listing the *same
caller* can read.

**Why**: Horizon expands a network's `subnets` id list against a separate subnet listing and substitutes the
resolved object per id. An id the subnet list does not return becomes a **null** entry, and a wizard/table row
built from it fails to render — the same failure family as F9's null-field volume-table crash (the SPEC-018
report explicitly groups them).

**Today the two paths disagree:**

| Path | Scope | Pagination |
|---|---|---|
| `networkSubnetIDs` (`:1287-1304`) — populates `subnets` | `WHERE network_id = $1` — **no project predicate** | none |
| `ListSubnets` (`:977-984`) — what Horizon expands against | `WHERE (s.project_id = $1 OR n.shared = true)` | **LIMIT / OFFSET** |

Two concrete divergences follow: (a) a subnet whose `project_id` differs from the caller's on a non-shared
network is referenced but not listable; (b) with enough subnets, pagination truncates the listing while the
per-network id array stays complete.

**Required**: align the two so the sets cannot disagree for a given caller.

---

## C3. Subnet response fields — `project_id` deferred (G15)

Subnet responses carry `tenant_id` but **no** `project_id`. Upstream subnets carry both. The launch wizard
renders without it, so adding it the same way as C1 is **deferred** (research.md G15).

Fields already emitted: `id`, `name`, `network_id`, `cidr`, `gateway_ip`, `ip_version`, `enable_dhcp`,
`dns_nameservers`, `allocation_pools` (non-null list — SPEC-019), `created_at`, `updated_at`.

---

## C4. End-to-end behavior (FR-008, FR-010)

1. A network created through the Project panel appears in the wizard's Networks step as a **selectable row**.
2. Selecting it lets the Networks step validate, so Launch becomes available.
3. The launched instance is created, reaches **ACTIVE**, and appears in the Instances panel — matching what
   `openstack server create` already produces (which the SPEC-018 real pass proved works, including a real
   libvirt VM booting from an RGW image with a reachable fixed IP).

---

## Not in this contract

- A full external/provider-network model — out of scope; `router:external` stays `false` (SPEC-019).
- Inlining full subnet **objects** into the network response — rejected: Neutron v2.0 specifies `subnets` as a
  list of ids, and diverging would break Art. I.

## Test assertions (FR-015)

1. `GET /v2.0/networks` returns `project_id` on every network, equal to `tenant_id` (fails today: absent).
2. `GET /v2.0/networks/{id}` and `POST /v2.0/networks` responses carry `project_id` too (all three build sites).
3. **Integrity**: for each network the caller can list, every id in its `subnets` array appears in
   `GET /v2.0/subnets` for that same caller — the direct F7 regression guard.
4. The integrity property still holds for a **shared** network owned by another project.
5. Each network still appears exactly once (no regression of the #532 F4 dedup).
