# Phase 0 Research: Nova Gaps & Client Discovery

**Feature**: `025-nova-gaps-client-discovery` | **Date**: 2026-08-31
**Method**: reading the tree (primary), plus Context7 against `/openstack/nova` for upstream behaviour.

> Context7 **is** connected in this session. Earlier features in this stack (#546) fell back to fetching
> opendev directly because it was not; that workaround is unnecessary here.

---

## R1 — Self-link precedence and blast radius

**Decision**: `common.BaseURL` prefers `c.Request.Host`, falls back to `LIGHTSTACK_ENDPOINT_HOST`, and applies
its `defaultPort` argument in **both** branches. Every self-link emitter in the product routes through
it. No opt-out setting.

**Rationale**: today the function returns early on `LIGHTSTACK_ENDPOINT_HOST` and never consults the request,
which is precisely why keystoneauth discards `endpoint_override` (B3). Behind a proxy or ingress the
request already carries the hostname the deployment wants advertised, so the configured value is only
needed when there is none. The dropped-port half is the *second* defect in the same function, found
during the #534 review: `BaseURL(ctx, 0) -> "https://host:443"` is pinned by its own test, proving the
port argument is ignored outside the configured-host branch.

**Blast radius measured, not estimated** — four distinct failure forms, all currently unresolvable for a
remote client:

| Site | Form |
|---|---|
| `internal/cinder/volumes.go:2253` | hardcoded `http://lightstack:8776/v3/` — a hostname that exists only inside the compose file |
| `internal/neutron/network.go:255` | relative `"/v2.0"` |
| `internal/placement/placement.go:39,60` | hardcoded `http://` scheme |
| `internal/nova/server_update.go:105`, `internal/keystone/groups.go:47,121,156,221`, `internal/keystone/handlers.go:588,656,739,778,825` | `c.Request.Host + "/path"` — no scheme |

**Alternatives considered**:
- *Forwarded-host header only* — rejected in clarify Q2: a client talking directly to the service (the
  endpoint-override case) sends no such header, so B3 would survive the fix meant to remove it.
- *Keep configured-wins, fix only the port and add the missing links* — rejected: leaves B3 open, which
  is the ticket's ask.
- *An opt-out setting* — rejected: it preserves the defect for anyone who sets it, and the need it
  addresses is already met by the proxy putting its hostname in the request.

**Consequence to handle**: `internal/common/baseurl_test.go:43,49` asserts the current precedence, using
the literal request host `"ignored:1"`. It must be rewritten **before** the change, as the RED step.

---

## R2 — Why the libvirt domain disappears (B12): RESOLVED on the host, 2026-09-01 — it did not

**Outcome: hypothesis 2 below was right, and the premise of the question was wrong.** The domain never
disappeared. The ACPI shutdown had not completed, the guest was still running, and the `500` came from
`DomainCreate` against an already-active domain — an error the outer layer flattened into
`NewInternalServerError`. The host diagnostic falsified hypotheses 1 and 4 (one bind-mounted socket; the
domain survives the stop as *shut off* in the same libvirtd Lightstack uses) and hypothesis 3 was never needed.
Full evidence and the per-hypothesis verdicts are in [BASELINE.md](BASELINE.md) § "R2 diagnostic — CLOSED".

The decision below stands as written and was the right call: none of the three mechanisms needed the
answer, and all three are still required — a guest genuinely can vanish, and the sweep still owns the drift
nothing in the request path hears about. What the answer added was the **three further defects the original
framing had hidden**, all invisible until R1 ran against real guest timing: the optimistic stop (FR-022a),
the fault-instead-of-conflict that latched (FR-026c), and the one-directional sweep (FR-024e).

**Decision**: do not gate this feature on finding the cause. Implement FR-024a (persist on action),
FR-025 (self-heal), FR-024b (periodic sweep) — which together produce correct behaviour for *any* cause
— and run the diagnostic below during the real-mode pass to close the question for the record.

**What reading the tree established**:
- Domains are **persistent**: `createVMReal` calls `DomainDefineXML` then `DomainCreate`
  (`pkg/hypervisor/libvirt.go:120,125`). A `DomainDefineXML` domain survives shutdown as *shut off*.
- The stop is **graceful, not destructive**: `stopVMReal` calls `DomainShutdown`, never destroy or
  undefine (`libvirt.go:285`).
- The XML sets **no `<on_poweroff>` override** (`pkg/hypervisor/xml_template.go:49-80`), so libvirt's
  default applies; for a persistent domain that stops it, it does not undefine it.
- Only two places undefine: `deleteVMReal` (`libvirt.go:192`) and the Helm pre-delete purge
  (`internal/teardown/teardown.go:219`). Neither runs during an ordinary stop.
- The stored identifier is libvirt's own: `createVMReal` returns the formatted `domain.UUID` and
  `handlers.go:872` writes it to `instances.libvirt_domain_id`. The XML also sets `<uuid>` from the
  instance id, so the two should agree.

**So on the evidence in the tree the domain should still be there** — which is why this stays open.
Ranked hypotheses for the host pass:

1. **The stop never reached the domain the start looked for** — two libvirtd instances in play (a
   container socket versus the host's `qemu:///system`). Would explain both the failed lookup *and* the
   ticket's `virsh` showing nothing, without either being wrong.
2. **The guest's ACPI shutdown never completed**, the domain is running, and the 500 has a different
   cause that the wrapped error text ("domain not found: %w") obscures at the outer layer.
3. **A name collision**: domain names are `instance-<first 8 chars of uuid>` (`handlers.go:832`), so two
   instances sharing an 8-character prefix collide in the name space while differing by UUID.
4. **Something in the deployment reaped it** — the teardown path or an operator action between the two
   requests.

**Diagnostic procedure** (cheap, and settles it): before the stop, log `DomainGetState` and the libvirt
connection URI; after the stop, call `DomainLookupByUUID` immediately and log the result; compare
`virsh -c <the URI Lightstack actually used> list --all` rather than the host default.

**Why the fix is safe regardless**: FR-024a makes the record honest for changes the product made;
FR-025 makes the start succeed whatever removed the guest; FR-024b catches the rest. None of the three
needs to know the cause. If hypothesis 1 is right, the self-heal simply re-creates the domain in the
libvirtd Lightstack is actually talking to — which is the correct outcome anyway.

---

## R3 — Self-heal: where the guest definition comes from

**Decision**: extract the `hypervisor.VMSpec` assembly out of the VM-creation goroutine so it can be
rebuilt from stored state, then re-define and start via the existing `CreateVM` path.

**Rationale**: verified feasible field by field against `handlers.go:830-838` — every input is already
persisted, so nothing from the original create request is needed:

| `VMSpec` field | Source after the fact |
|---|---|
| `UUID` | `instances.id` |
| `Name` | derived from `instances.id` |
| `VCPUs`, `MemoryMB`, `DiskGB` | the `flavors` row via `instances.flavor_id` |
| `ImagePath` | the per-instance overlay under the instance's directory — or, for a server booted from a volume, that volume (its `/dev/vda` attachment); added in review |
| `Networks` | `ports WHERE device_id = <instance>` in the instance's project — the same rows US6's join reads |
| `Volumes` | the instance's other volume attachments (added in review: a revived guest must keep its data volumes) |
| `CloudInit` | the config-drive ISO in the instance's own directory |

**The blocker is shape, not data**: the assembly is ~150 lines inline inside a goroutine closure that
also does image fetching and overlay staging. Extracting it is the actual work, and it is worth doing
regardless — it is the only reason the start path cannot currently rebuild a guest.

**Alternatives considered**:
- *Store the generated XML in a column* — rejected: it would go stale against flavor or port changes,
  and duplicates state that is already authoritative elsewhere.
- *Ask libvirt for the definition* — impossible by construction; if the domain is gone so is its XML.

---

## R4 — Representing a failed boot's reason (`fault`)

**Decision**: add `instances.fault_code`, `fault_message`, `fault_created` (all nullable, migration
`067`) and emit upstream's `fault` object in server detail when the server is in `ERROR`.

**Rationale**: FR-016 requires the reason be readable from the server's own record. Nothing in Lightstack
emits `fault` today — `grep '"fault"' internal/nova/` returns nothing, and no migration defines such a
column. Upstream's shape is `{"code", "created", "message", "details"}`, present on server detail for a
faulted server, and Article I fixes the shape.

**Alternatives considered**:
- *Reuse `instance_actions.message`* — attractive because the column exists and is already tenant-visible
  through `os-instance-actions`. Rejected: a user diagnosing a failed boot runs `server show`, not
  `server event list`; the complaint B13 describes is precisely that `server show` says nothing. Also
  loses the upstream response shape.
- *Log only* — rejected by FR-016 explicitly.

**Note**: `details` is omitted. Upstream populates it with a traceback for administrators only; Lightstack has
no traceback to offer and inventing one would be worse than omitting an optional field.

---

## R5 — The B14 refactor is a signature change, not a resolution change

**Decision**: change the 17 affected handlers in `internal/nova/advanced_actions.go` from
`func (svc *Service) X(c *gin.Context)` to `func (svc *Service) X(c *gin.Context, instanceID, projectID
string)` and update their `serverActions` entries from `_, _` to `id, proj`. Add no lookups.

**Rationale — this materially shrinks the change the ticket describes.** The dispatch table's signature
is *already* `func(*Service, *gin.Context, any, string, string)` and `ServerAction` already passes the
resolved id and owning project into every entry (`handlers.go:1615-1621, 1502`). Handlers that need them
already receive them (`pause`, `unpause`, `lock`, `unlock`, `forceDelete`, `rebuild`, `rescue`). The
broken ones simply declare a narrower signature, discard the values as `_, _`, and re-read
`c.Param("id")` — which is the raw, unresolved reference. SPEC-023's own comment at `handlers.go:1525`
says these were "left as-is so this refactor changes dispatch only", i.e. it deferred exactly this.

**Verified**: none of the 17 is reachable by any route other than the action dispatcher — a grep for
each handler name outside the table returns nothing. So no call site needs its own resolution, and
authorization is untouched (the dispatcher already authorized before dispatching).

**Alternatives considered**:
- *Call `resolveServerOr404` inside each handler* — rejected: it would resolve twice per request and
  reintroduce the 25-way divergence risk SPEC-023 removed by resolving once.

---

## R6 — Reconciler pattern to follow

**Decision**: `internal/nova/state_reconciler.go`, a ticker loop with a constructor taking an interval
that clamps non-positive values to a default, started as a goroutine from `cmd/lightstack/main.go` and stopped
on shutdown. Action-history reclamation lives beside it as a second sweep on the same object.

**Rationale**: two accepted precedents with the same shape —
`neutron.NewNetworkReconciler(svc, cfg.Neutron.ReconcileInterval)` / `.Start(ctx)` /
`.PollInterval()` / `.Stop()` (`internal/neutron/reconciler.go:31,44,67,70`; started at
`cmd/lightstack/main.go:213`), and `console.NewReconciler(store, cfg.Nova.ConsoleReconcileInterval).Run(ctx)`
with a `sweep` (`internal/console/reconciler.go:33,42,62`; started at `main.go:266`). Matching one of
them exactly is cheaper to review than a third dialect. The neutron one clamps `<= 0` and exposes the
**effective** interval for logging — worth copying, because a deployment that sets `0` should not get a
busy loop.

**Why one object for two sweeps**: they share a ticker and a lifecycle, and the history sweep is a single
`DELETE ... WHERE start_time < $1`. A second file for one statement is not warranted.

---

## R7 — Microversion gate for deleted-server action history

**Decision**: gate the deleted-server read on a requested microversion of **2.21 or higher**, read from
`OpenStack-API-Version` (falling back to `X-OpenStack-Nova-API-Version`). Treat an absent header as the
advertised current version, `2.90`, so ordinary clients are unaffected.

**Rationale**: confirmed via Context7 against `/openstack/nova` — *"Action information of deleted
instances can be returned for requests starting with microversion 2.21"*, and *"Access is restricted by
default to administrative users or the owner of the server."* The second half independently confirms
clarify Q4's answer; the first half adds a constraint the spec did not have.

**The awkward part, stated plainly**: Lightstack has **no microversion negotiation**. It emits the header
(`handlers.go:288`) and never reads it; comments elsewhere reference microversion-gated behaviour
(`advanced_actions.go:1222`, "2.45+ returns image_id") that is in fact unconditional. This gate is
therefore the product's first. That is accepted because Article I is non-negotiable and shipping a new
incompatibility while fixing an old one is incoherent — but generalising microversion handling is a
product-wide concern and is **out of scope**, flagged for its own ticket.

**Alternatives considered**:
- *Serve ungated* — rejected on Article I.
- *Build general negotiation now* — rejected as disproportionate; it would dwarf the ten defects.

---

## R8 — F16's service boundary

**Decision**: add a narrow `CinderService` interface to the compute service — one method to create a
bootable volume from an image — mirroring the existing `NeutronService`. Wire the real implementation in
`cmd/lightstack/main.go`.

**Rationale**: the compute service holds `vmManager`, `cache`, `imageStore`, `neutronSvc` and
`consoleStore` — **no block-storage handle** (`internal/nova/handlers.go` `Service` struct). The
established pattern for exactly this problem is already there:

```go
type NeutronService interface {
    AllocatePortForInstance(ctx context.Context, networkID, projectID, instanceID string) (any, error)
}
```

Both halves F16 needs already exist and must be reused, not reimplemented: the block-storage service
creates a bootable volume from a raw base image and records `volumes.source_image_id`
(`internal/cinder/volumes.go:443-472`), and the compute service already boots from a root volume,
records the attachment at `/dev/vda` and marks the volume in use (`internal/nova/handlers.go:716-780`).

**Alternatives considered**:
- *Direct SQL and storage calls from compute* — rejected: duplicates the volume-creation logic and would
  drift from it, and it is what the `NeutronService` precedent exists to avoid.
- *An HTTP call to the block-storage service* — rejected: same process, and Article VI plus the
  synchronous-operations principle make an in-process call the obvious choice.

---

## R9 — Default boot-volume size

**Decision**: the flavor's `disk_gb`, floored at the image's `min_disk_gb`. Refuse a size — requested or
defaulted — that is below the image's minimum, before any volume is created.

**Rationale**: both inputs exist (`flavors.disk_gb NOT NULL`, `images.min_disk_gb INTEGER DEFAULT 0`,
`migrations/001_initial_schema.up.sql:41,181`). The volume *is* the root disk, so the flavor is the size
the user chose implicitly. `min_disk_gb` defaults to `0`, so it cannot be the sole source without
producing a zero-size volume.

**Honesty about upstream**: Context7 returned upstream's request *examples*, which all carry an explicit
`volume_size`, and documented `volume_type`'s default — but no documented default for `volume_size`
itself. This choice is consistent with upstream's known behaviour of falling back to the flavor's root
disk for image-sourced block devices, but that was **not** verified line-by-line in this pass. Recorded
as a local decision rather than a claim of fidelity; worth confirming against
`nova/compute/api.py::_validate_bdm` during implementation.

---

## R10 — Lifecycle failure status mapping

**Decision**: three distinguishable outcomes — target absent, action impossible in the current state,
internal fault — using upstream's statuses for each. A failed self-heal is *action impossible*, not a
fault. No status upstream does not emit for a lifecycle action.

**Rationale**: Context7 confirms the action endpoints answer `202` on success and that the surrounding
error vocabulary for server actions is `400 / 403 / 404 / 409`, with `409` the conflict answer for a
server in an invalid state for the requested action. Today `lifecycleAction` returns a flat `500` for
every libvirt failure (`handlers.go:1741-1756`), which is what makes B12 unreadable from the outside.

**Rejected**: `503` for an unreachable hypervisor, despite Article III's fail-fast posture making it
descriptively apt. Clients branch on status against upstream (the infrastructure-as-code provider,
gophercloud), and Article I outranks descriptive precision. The extra detail lives in the log.

---

## R11 — Floating IPs in the address map

**Decision**: join `floating_ips` on `port_id` in `getInstanceAddresses` and emit floating entries into
the same per-network list, each entry labelled with its own kind.

**Rationale**: the query at `internal/nova/handlers.go:1158` selects `p.network_id, p.fixed_ips, n.name`
and never touches `floating_ips`; the type is then hardcoded `"fixed"` at `:1191`, so only one kind can
ever appear. The columns needed are all present: `floating_ips.port_id`, `floating_ip_address`,
`fixed_ip_address` (`migrations/003_add_routers.up.sql:28-40`), with an index on `port_id` already.
Upstream chains fixed and floating addresses into one per-network list, as the ticket quotes from
`nova/api/openstack/common.py`.

**Care point flagged for implementation**: `getInstanceAddresses` takes the instance's **owning** project
deliberately — its doc comment explains that passing the caller's project would return an empty map for
an administrator viewing another tenant's server. The new join must widen with its parent for the same
reason (FR-039).

**Performance note**: this is one extra join on a query already executed per instance in a listing. It
does not change the number of round trips. Collapsing the whole listing into one query is a
pre-existing N+1 concern, out of scope here and noted in the spec's deferrals.

---

## R12 — Configuration surface

**Decision**: two new `NovaConfig` settings, following the naming and defaulting style already there:

| Setting | YAML | Default | Purpose |
|---|---|---|---|
| `StateReconcileInterval` | `state_reconcile_interval` | 30s | FR-024b/FR-024d — drift correction |
| `InstanceActionRetention` | `instance_action_retention` | 90 days (2160h) | FR-042a/FR-042b — history reclamation |

**Rationale**: `NovaConfig` already carries `ConsoleTokenTTL` and `ConsoleReconcileInterval` as
`time.Duration` with `Default*` constants applied by `LoadConfig` when the file omits them
(`internal/common/config.go:65-81`) — "so a deployment that never heard of the console still expires
tokens". The same reasoning applies exactly. `DefaultConsoleReconcileInterval` is 30s, which is also the
right order of magnitude for state drift.

**Deployment note**: the binary honours only `LIGHTSTACK_DB_URL`, `LIGHTSTACK_JWT_SECRET`, `LIGHTSTACK_ENDPOINT_HOST` and
`LIGHTSTACK_ENDPOINT_SCHEME` as environment overrides, so both settings must arrive through the Secret-backed
`lightstack.yaml` the chart renders — not as environment variables.

---

## Upstream facts confirmed this phase

| Fact | Source | Used by |
|---|---|---|
| Deleted-instance action history is available from **microversion 2.21** | Context7 `/openstack/nova` api-ref | R7, FR-043 |
| Access "restricted by default to administrative users or the owner of the server" | Context7 `/openstack/nova` api-ref | Confirms clarify Q4 independently |
| `os-start` / `os-stop` answer `202` with no body; server-action errors use `400/403/404/409` | Context7 `/openstack/nova` api-ref | R10, FR-026 |
| A failed action that changes status to `ERROR` surfaces an **instance fault** on server details | Context7 `/openstack/nova` api-guide `faults` | R4, FR-016 |
| `block_device_mapping_v2` supports `source_type: image` + `destination_type: volume` with `volume_size` | Context7 `/openstack/nova` api-ref | R9, US9 |
| `volume_size`'s default is **not documented** upstream | Context7 (absence) | R9 — recorded as a local decision |
