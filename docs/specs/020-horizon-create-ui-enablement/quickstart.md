# Quickstart — Horizon create/write UI enablement (SPEC-020)

**Feature**: F2/F3/F7/F8 (cloud-storage#533) | **Branch**: `feature/533` | **Plan**: [plan.md](./plan.md)

The one-page orientation for picking this feature up. Full detail: [spec.md](./spec.md),
[research.md](./research.md), [contracts/](./contracts/).

---

## What's broken, in one line

Horizon's **read** panels work, but every **create/write** UI fails — because Horizon loads quota/limits/
capability metadata up front and hides actions, blanks widgets, and refuses to open modals when it can't get
it. The APIs themselves are fine (CLI creates networks, volumes and servers successfully).

| Finding | Symptom | Root (confirmed by code inspection) |
|---|---|---|
| **F7** | No Launch Instance action; wizard's Networks step has no selectable rows | Compute limits inconsistent; network objects lack `project_id`; subnet ids not resolvable in the subnet list |
| **F8** | Create Volume button opens nothing | **Cinder has no `/v3/extensions`** (404) — the form raises while building |
| **F2** | Overview: "Unable to retrieve limits information" | One of the three aggregated legs (Nova/Cinder/Neutron) fails or disagrees |
| **F3** | No Create Network action in the Project panel | Network quota fetch fails/absent → Horizon hides the action. **Not** a policy issue |

> **Superseded (2026-07-29):** this table records the roots as first inferred from code. Implementation found
> three different ones — F2/F3 the quota read surface (Neutron's quota details lacked `reserved`), F8 an
> unregistered `GET /v3/groups/detail` (FR-021), F7 three response-shape and request-parsing defects
> (FR-022–FR-024) plus the boot-from-volume mapping. See spec.md *Assumptions*.

---

## The one rule before you code

**Trace first (FR-014).** The ticket says so, and the plan gates Phase B on it. Drive each of the four
create UIs and capture what Horizon actually calls:

```bash
# 1) Bring up the stub stack (SPEC-018 stub runbook)
docker compose -f deployments/docker-compose-horizon.yml up -d

# 2) Watch Lightstack's structured request log while you click through each panel
docker compose -f deployments/docker-compose-horizon.yml logs -f lightstack | grep -Ei 'quota|limits|extensions|types|networks|subnets'

# 3) When a panel breaks, read Horizon's traceback (this is where the real cause is named)
docker compose -f deployments/docker-compose-horizon.yml exec horizon sh -c 'tail -80 /var/log/apache2/*error*'
```

Panels to drive: Project → **Overview** (F2); **Network → Networks** (F3); **Volumes → Create Volume** (F8);
**Compute → Instances → Launch Instance** (F7).

Reconcile the captured call map against the 14 confirmed gaps in
[research R1](./research.md#r1--confirmed-code-level-gaps-from-inspection-of-this-branch); fold in anything new.

---

## What changes (no migration, no new modules)

| Service | File | Change |
|---|---|---|
| Nova | `internal/nova/quotas.go`, `handlers.go` | One limits helper (defaults ⊕ `quotas`); drop the shadowing 100/200/512000 map → upstream **10/20/51200**; add `os-quota-sets/:id/detail`; real floating-ip / SG / server-group usage |
| Neutron | `internal/neutron/network.go` | Add `quotas/:id/default` + `.json` aliases; project-scope the quota + emit `project_id`/`tenant_id`; advertise `quota_details`; add `project_id` to networks; align subnet-reference scope |
| Cinder | `internal/cinder/quotas.go`, `volumes.go` | **Add `/v3/extensions`**; add `?usage=true` on `quota-sets/:id` (block storage has no `/detail`) and `/defaults`; make limits read `cinder_quotas`; de-duplicate the twin limits handlers |

**Watch out — gin param names:** a new Cinder route must reuse the param name already claimed at that
position, or the binary panics at startup — e.g. `/v3/quota-sets/:tenant/defaults` alongside
`/v3/quota-sets/:id` fails with *`':tenant' in new path … conflicts with existing wildcard ':id'`*.
So every route here uses `:id`.

Registration **order** is irrelevant: gin splices a static child ahead of an existing wildcard at the
same level, so `/v3/extensions` and `/v3/groups/detail` win over the `/v3/:project_id` group
(`internal/cinder/volumes.go:196`) whichever is registered first — verified in both orders against the
vendored v1.12.0. *(An earlier revision of this line warned the opposite; corrected 2026-07-29, and
recorded at `plan.md`, `contracts/cinder-extensions-and-quota-sets.md` and `volumes.go:112`.)*

---

## Tests

There are **no unit tests** for quota/limits today — those are net-new. But **contract and script suites do
exist** and already cover these endpoints (`test/contract/{nova/limits,cinder/quotas,neutron/quotas}_test.go`,
`test/quota_test.sh`, `test/quota_admin_test.sh`); they are the regression gate and **`make test` does not run
them**.

```bash
# new unit tests — only the extension listings run without a database;
# the quota handlers resolve their target project, so most need the DB-backed run below
go test ./internal/nova/... ./internal/neutron/... ./internal/cinder/...

# DB-backed cases (usage counts, table overrides)
export LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable'
go test ./internal/...

# the pre-existing regression gate — needs a running Lightstack, excluded from `make test`
make test-contract
./test/quota_test.sh && ./test/quota_admin_test.sh && ./test/horizon_compat_test.sh
```

Reassuring, and already verified while planning: those suites assert limits **value-agnostically**
(`GreaterOrEqual(…, 0)`) and authenticate as `admin`/`secret`, so neither the limit-value change (FR-003) nor
the admin-only quota writes (FR-020) should break them — confirm rather than assume.

Harness: each package has `newNovaTestRouter()` / `newNeutronTestRouter()` / `newCinderTestRouter()` wiring the
real routes behind a stub auth middleware. Scope a request with `X-Test-Project` / `X-Test-User` /
`X-Test-Roles`; **omit `X-Test-Roles` to get a non-admin caller** (that's how the FR-002a cases are written).
Use a throwaway project id so you don't perturb the 13 quota rows migration `007` seeds.

**The load-bearing assertion** (fails today, must pass after): the limits endpoint and the quota-set report the
**same** value for the same resource — Nova `instances` is 100 via `/limits` but 10 via `/os-quota-sets`.

---

## Definition of done

1. All four workflows work in the browser: launch an instance, create a volume, create a network, Overview
   limits render.
2. Verified in **both** modes (FR-017): stub via `deployments/docker-compose-horizon.yml`; real via Lightstack on
   **k3s** (`charts/lightstack`, real mode) + **Horizon via Helm** (`deployments/horizon-chart/`) + **Ceph/RGW via
   Rook on minikube** (`deployments/rook/setup-rook-rgw.sh`).
3. Horizon stays **stock** — no config/policy patches (FR-018). Anything that would need one is a documented
   finding, not a fix.
4. Existing suites still green (FR-016); F2/F3/F7/F8 tagged fixed in
   `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md`.
