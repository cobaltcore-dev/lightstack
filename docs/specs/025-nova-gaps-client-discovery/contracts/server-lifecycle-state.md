# Contract: lifecycle state and self-heal (US4 / B12)

**Covers**: FR-022 – FR-028
**Applies to**: `POST /v2.1/servers/{id}/action` with `os-stop`, `os-start`, `reboot`; `GET /v2.1/servers/{id}`

## The defect in one line

`lifecycleAction`'s **real-mode** branch (`nova/handlers.go:1741-1756`) calls the hypervisor and writes
**nothing** to the record. Only the stub branch persists state. So a successful stop leaves `ACTIVE` /
`power_state=1` recorded — regardless of anything else going wrong.

## Required behaviour

| Action | Hypervisor outcome | Response | Record after |
|---|---|---|---|
| `os-stop` | shut down | `202` | `SHUTOFF`, `power_state=4` (FR-022) |
| `os-start` | started | `202` | `ACTIVE`, `power_state=1` (FR-023) |
| `os-start` | **guest absent** | `202` | re-defined, started, `ACTIVE` (FR-025) |
| `os-start` | guest absent, re-define fails | *action impossible* | honest state, not "running" (FR-025c) |
| any | server not reachable by this caller | *absent* | unchanged |
| any | internal fault | *internal fault* | unchanged |

Three distinguishable outcomes, using upstream's statuses (FR-026, FR-026a). Today every libvirt failure
is a flat `500`, which is exactly what makes the divergence unreadable from outside.

**FR-026b**: a self-heal that cannot complete is an *action that cannot be performed*, not an internal
fault. The distinction matters to a client deciding whether to retry: a state problem will not resolve
itself, an outage might.

## Self-heal (FR-025, FR-025a)

Re-create from **stored state** — nothing from the original create request is needed:

| `VMSpec` field | Source |
|---|---|
| `UUID`, `Name` | `instances.id` |
| `VCPUs`, `MemoryMB`, `DiskGB` | `flavors` via `instances.flavor_id` |
| `ImagePath` | the volume attached at `/dev/vda` when the server booted from one — chosen as the boot path chooses it — otherwise the per-instance overlay |
| `Networks` | the instance's ports: `device_id = <instance>`, in the instance's own project |
| `Volumes` | the instance's other volume attachments, at their devices — upstream's hard reboot defines the guest from all of its block devices |
| `CloudInit` | the instance's config-drive ISO |

Requires extracting the assembly currently inline at `nova/handlers.go:830-838` inside the VM-creation
goroutine. That extraction **is** the work; the data is already there.

Runs inside the existing 10-second action context — one attempt, no retry, no backoff (FR-025e,
Article III). Exceeding the budget is a failure under FR-025c, not a held-open request.

## Recording the repair (FR-025b, FR-025d)

On the `instance_actions` row for the triggering `os-start`, via its existing `message` column, plus a
warning-level log. **No new action name and no new API surface.**

## Divergence (FR-024 – FR-024d)

```
change the product made      ──> persisted immediately, no reconciliation needed   FR-024a
change the product did not   ──> corrected within state_reconcile_interval          FR-024b
reading a server             ──> never contacts the hypervisor                      FR-024c
```

FR-024c is binding: a server listing must not slow down in proportion to instance count. The reconciler
follows `neutron.NetworkReconciler` / `console.Reconciler` exactly (constructor + interval clamp +
`Start(ctx)` + `Stop()`), started from `cmd/lightstack/main.go` beside the existing two.

## Stub mode (FR-028)

Unchanged: transitions are recorded, no hypervisor is contacted, self-heal is a no-op.

## Test obligations

- Unit: real-mode stop persists `SHUTOFF`; start persists `ACTIVE`. Both currently fail (RED).
- Unit: the three failure outcomes map to three distinct statuses.
- Real host: ten stop/start cycles, `server show` vs `virsh list --all` agreeing after each.
- Real host: remove the domain out of band → next `os-start` succeeds; the repair is on the action record.
- Real host: after the fix, `test/contract/nova` no longer hangs (see #548 F15).
- **Diagnostic to run once** (research.md R2): log the libvirt URI and `DomainGetState` around the stop,
  and compare `virsh -c <the URI Lightstack used>` rather than the host default — the root cause of the
  disappearance is still open, and the fix does not depend on it.
