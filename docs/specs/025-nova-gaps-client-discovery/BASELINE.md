# Baseline: Nova Gaps & Client Discovery (SPEC-025)

**Captured**: 2026-08-31, before any code change on `feature/547`
**Purpose**: T001–T003. Every later failure must be attributable — either it is here, or this feature
caused it.

## T001 — shared PostgreSQL state

One `lightstack-postgres` container is shared across ~12 worktrees, and a poisoned quota row has previously
made an entire suite report failures that had nothing to do with the code under test. Checked first:

| Check | Value | Verdict |
|---|---|---|
| Projects | `default` (only) | clean |
| `quotas` for `default` | `instances=10`, `cores=20`, `ram=51200`, `volumes=10`, `networks=10`, `ports=50`, `floatingip=10`, `gigabytes=1000`, `routers=10`, `security_groups=10`, `security_group_rules=100`, `snapshots=10`, `subnets=10` | clean — **not** the `instances=0` poisoned state |

Connection: `postgres://lightstack:secret@localhost/lightstack` (the `postgres` role does not exist in this
container — use `-U lightstack`).

## T002 — suite baseline

### Build and lint

| Gate | Result |
|---|---|
| `go build ./...` | **OK** |
| `make lint` (`golangci-lint run ./...`) | **0 issues** |

`golangci-lint` emits one warning about a cache entry referencing a path in another worktree
(`bugfix/515/...`). It is a stale-cache artefact, not a finding, and does not affect the exit code.

### Unit tests — `go test ./internal/... ./pkg/...`

**PASS.** 15 packages ok, 0 failed, 5 with no test files.

### Contract tests — `go test ./test/contract/...` with the server running

Run against `./bin/lightstack --config config/lightstack.yaml` in stub mode (`libvirt_mode: stub`,
`networking_mode: stub`, cinder/glance `storage_mode: local`) with
`LIGHTSTACK_JWT_SECRET=dev-secret-change-me`.

> **Correction (same day).** The first capture ran without `-count=1`, so `test/contract` came back
> `(cached)` and its tests never actually ran. Re-run with `-count=1` the root package **fails**:
> `TestWS5_VolumeAttachLifecycle` gets a `503` on volume create. Proven pre-existing by stashing the
> only code change on the branch and re-running — it fails identically. Root cause is environmental:
> `/var/lib/lightstack` does not exist in a local non-root run, and `paths.Base` is a hardcoded const with no
> override, so cinder/glance `storage_mode: local` has nowhere to write. The container image creates
> that tree (`RUN mkdir -p /var/lib/lightstack/...`); a bare `./bin/lightstack` does not. **Always pass `-count=1`
> when capturing a contract baseline.**

| Package | Result | Time |
|---|---|---|
| `test/contract` | **FAIL (1)** — `TestWS5_VolumeAttachLifecycle`, environmental | 2.5s |
| `test/contract/keystone` | **ok** | 5.1s |
| **`test/contract/nova`** | **ok** | **17.5s** |
| `test/contract/cinder` | FAIL (7) | 2.5s |
| `test/contract/glance` | FAIL (2) | 2.3s |
| `test/contract/neutron` | FAIL (2) | 5.1s |

**12 pre-existing failures, none in compute or identity** — the two services this feature changes most:

```
TestWS5_VolumeAttachLifecycle              (test/contract)
TestCinderUpdateReadonlyFlag_Contract      TestCinderSetImageMetadata_Contract
TestCinderForceDetach_Contract             TestCinderResetStatus_Contract
TestCinderUnsetImageMetadata_Contract      TestCinderReimageVolume_Contract
TestCinderCreateBackup_Contract
TestGlanceUploadImageData_Contract         TestGlanceDownloadImageData_Contract
TestTopologyNetworkList_Contract           TestTrunkBasicCRUD
```

**10 of the 12 are storage-environmental** (the missing `/var/lib/lightstack` above) — every cinder volume-data
and glance image-data case. The two neutron ones are genuine pre-existing defects outside this feature's
scope.

None is in scope for SPEC-025. They are the bar: this feature must not add to this list, and must not
remove anything from the `ok` rows above.

### Update after Phase 8 (2026-09-01)

Two of the twelve baseline failures — `TestTopologyNetworkList_Contract` and `TestTrunkBasicCRUD` —
**no longer reproduce**, in isolation or in a full run of the neutron package (0 failures). Nothing in
this feature touches the network topology or trunk surfaces, so this is not a fix: they are
state-dependent, which is the shared-`lightstack-postgres` hazard T001 exists to check for. The baseline
captured them failing against whatever leftover state existed at that moment.

Consequence for later phases: the comparison bar is **"no failure absent from the baseline"**, not
"exactly the baseline set". Ten storage-environmental failures remain, as does the reason for them.

**Note on the compute suite.** `test/contract/nova` passes in 17.5s and does **not** hang. The source
ticket reports it hanging against a stack where the stop/start divergence (B12) has been triggered, and
that a clean stack passes 88/0 in the same session. This baseline is consistent with that account, and
it is the reference for T054's check that the hang is gone.

## T003 — stacks

| Mode | Status | Stories it can exercise |
|---|---|---|
| **Stub** (local binary + shared PostgreSQL) | **Confirmed working.** `GET /v3` → 200, `GET /v2.1` → 200, all six services bound | US1, US2, US5, US7, US8 in full; US3's pre-flight half; US4's state-persistence half; US9's volume-row half |
| **Real** (k3s, libvirt/KVM, router + external network + floating IP) | **Not exercised in this pass** — needs a privileged Linux host with a hypervisor | US3 (full boot path), US4 (self-heal, reconciler), US6 (floating IPs), US9 (real volume) |

Real-mode work is therefore deferred to the host-validation tasks (T052, T065, T078, T107) and the
runbook (T122). Nothing else in the plan is blocked by its absence.

## Environment

| Component | Version |
|---|---|
| Go | 1.26.3 linux/amd64 |
| Binary | `v0.6.1-45-g90731cd-dirty` |
| PostgreSQL | `lightstack-postgres` container, up 3 days |
| Container runtime | podman (via the `docker` CLI shim) |

## Project setup verification

- `.gitignore` — present, already carries the Go and universal patterns.
- `charts/lightstack/.helmignore` — present.
- **`.dockerignore` — created in this pass.** `build/package/Dockerfile` does `COPY . .` with no
  exclusions, so the whole worktree entered the build context. The new file excludes VCS, build output,
  docs, chart and deployment manifests, editor noise and anything secret-shaped. It deliberately does
  **not** exclude `migrations/` or `config/lightstack.yaml`, both of which the runtime stage copies.

## R2 diagnostic (B12 root cause) — CLOSED, 2026-09-01

**Answer: hypothesis 2 of research.md R2 — the guest never disappeared.** The ACPI shutdown had not
completed, the domain was still running, and the `500` had a different cause that the error text hid.
Nothing removed the guest, in this deployment or in the original report.

### What the host showed

R4, run on the k3s single node:

| Check | Result |
| --- | --- |
| `libvirt_uri` in the rendered Secret | `qemu:///system` |
| socket in the pod | `srw-rw---- root 111 0 Aug 28 10:30 /var/run/libvirt/libvirt-sock` |
| socket on the host | `srw-rw---- root libvirt 0 Aug 28 10:30 /var/run/libvirt/libvirt-sock` |
| fresh guest present in host libvirtd before the stop | yes |
| domain after the stop | **SURVIVES**, `shut off` |
| API after the stop | `SHUTOFF` |

**Reproduced on a second, freshly deployed stack on 2026-09-02** — same URI, same socket, domain present
before the stop and `shut off` after it, API `SHUTOFF`. The first run was against a stack that had already
executed the buggy code, so this rules out the conclusion being an artifact of that state.

The two socket listings agree on mode, size, mtime and gid — `111` is the `libvirt` group's numeric id,
rendered numerically inside the container because it has no matching `/etc/group` entry. So it is one
bind-mounted socket, not two libvirtd instances. Strictly this is not inode-proof (`ls -la` does not print
inodes; `ls -lai` would), but the point is moot: **the domain survived the stop inside the host's
libvirtd**, which is the same libvirtd Lightstack is configured against. There is no second one to hide in.

### Verdict on each ranked hypothesis

1. **Two libvirtd instances — FALSIFIED.** One bind-mounted socket, and the domain is visible to the host
   `virsh` both before and after the stop.
2. **ACPI shutdown never completed; the 500 has a different cause — CONFIRMED.** The first R1 run recorded
   `api=SHUTOFF power=4 virsh=running` followed by eight identical `500 failed to perform os-start`. The
   "different cause" was `DomainCreate` against an already-active domain, an error with nowhere to go but
   `NewInternalServerError`.
3. **Name collision — not tested, and not needed.** Hypothesis 2 accounts for the whole report.
4. **Something reaped it — FALSIFIED** by the surviving domain.

### Why the ticket said the guest had vanished

Because from outside it looked that way. The API reported the server `SHUTOFF`, starting it failed with an
unexplained server error, and the guest's actual state matched neither. "The libvirt domain disappears" was
an *inference* from that divergence, not an observation of an absent domain — which is exactly why reading
the tree exonerated every mechanism that could remove one.

### What that changes, and what it does not

**The fix did not depend on the answer**, which is why this stayed open by design: persist-on-action
(FR-024a), self-heal (FR-025) and the periodic sweep (FR-024b) each produce correct behaviour whatever
removed the guest. All three stay — a guest CAN vanish (R2 proves the repair works when it does), and the
sweep is what notices drift nothing in the request path hears about.

What the answer did add is the three defects the original framing had hidden, all found once R1 ran against
real timing: the optimistic stop (FR-022a), the fault-instead-of-conflict that latched (FR-026c), and the
one-directional sweep (FR-024e). See tasks T053–T057, and T059 for the write-deadline defect the first fix
introduced.

## Real-mode host pass — complete, 2026-09-02

Every step R1–R13 run against a freshly deployed k3s stack. The numbers worth keeping:

| Evidence | Value | What it establishes |
| --- | --- | --- |
| R1, ten stop/start cycles | 20/20 lines agree with `virsh` | no divergence, no 500s, no lost responses |
| R13a, stop of a settled guest | **3.22s** | the stop waits for the guest rather than guessing (FR-022a) |
| R13e, stop 3s after boot | **30.87s**, `exit 0` | the guest could not answer ACPI, was forced off at the grace expiry, and the response still arrived inside the 60s write deadline (FR-022b) |
| R10, contract suite | **307s**, completed | no hang (SC-014, #548 F15). The ~18s quoted elsewhere is the stub baseline |
| R12, retention sweep | orphan 1→0, live-server row 1→1, `reclaimed: 1` | over-age records go, a live server's history stays (FR-042c) |
| R11, `openstack 6.6.0` | three identity reads, exit 0 | B16 against its actual reproduction case, not a client that would pass anyway |

**Defects found by this pass, none of them in the original ten:**

- Three that were B12's real root cause (optimistic stop, fault-instead-of-conflict that latched,
  one-directional sweep) plus a fourth the first fix introduced (grace period equal to the write
  deadline, so a forced stop lost its answer). Tasks T053–T057, T059.
- `libvirt_domain_id` scanned into a non-nullable `string` in three handlers — T060, found by R10.
- The `no rows in result set` leak in a tenant-visible fault message.
- **FOLLOWUPS F7** — the compute server listing ignores every query filter.
- **FOLLOWUPS F8** — the image PATCH silently ignores every op except `replace`.

**Six runbook steps could not fail as written** (R5, R7, R8, R9d, R11, R12's live-server count) and two
`NODE_IP` derivations were wrong in a way that failed much later. Each is corrected in place with a note
on why the original form passed vacuously. That count is the pass's most transferable result: a check that
cannot fail is indistinguishable from a check that passed.

## Constitution deviations actually taken

| Tracked in plan.md | Outcome |
|---|---|
| Two background loops (Article II) | **Not a deviation.** The constitution was amended to **v1.1.0** during the analyze pass, stating a permitted exception for stateless reconciliation loops. The amendment was a clarification: Article II's Forbidden Patterns list was already contradicted by two reconcilers merged to `main` (SPEC-009's network reconciler, SPEC-021's console reaper). |
| First microversion gate (Article I nuance) | **Taken as planned.** One local gate in `internal/nova/microversion.go`, for a deleted server's action history at 2.21, matching upstream. Documented there as the product's only gate; generalisation recorded in FOLLOWUPS.md F3. |
| New fault columns (`instances.fault_*`) | **Taken as planned.** Migration `067`, three nullable columns, reversible. Reusing `instance_actions.message` was rejected: a user diagnosing a failed boot runs `server show`. |

No further deviation was needed. The post-implementation constitution position is **PASS**.

## Verification summary (T120, T121, T125)

| Gate | Result |
|---|---|
| `gofmt` | 0 files need formatting |
| `go build ./...` | OK |
| `make lint` | **0 issues** |
| `go test ./internal/... ./pkg/...` | **15/15 packages ok** |
| `go test ./test/contract/...` | 10 failures, **all pre-existing**; 0 absent from the baseline |
| `test/contract/nova` | **ok**, ~18s, no hang |
| `test/contract/keystone` | **ok** |
| `helm lint` / `helm unittest` | pass / **18 suites, 84 tests** |
| `test/nova_gaps_test.sh` | **19 passed, 0 failed, 1 skipped** |
| Migrations `067` + `068` | down and up verified, columns and index confirmed present after |

The contract count is 10 rather than the baseline's 12 because `TestTopologyNetworkList_Contract` and
`TestTrunkBasicCRUD` stopped reproducing — state-dependent, not fixed by this feature. See the update
note above.

### T125 — mutation sweep

Every fix was reverted in turn and the corresponding check confirmed to go red. **15 of 15
falsifiable.** Two needed a second attempt:

- *nova version links* was first aimed at the source-level self-link sweep, which cannot detect a
  MISSING links array. Re-aimed at the contract test: goes red.
- *the dead `!= 'DELETED'` filter* could not be falsified behaviourally **and never could be** — the
  clause excluded nothing, so restoring it leaves the usage contract test green. That is the whole
  point of the finding: a latent under-count, not a current one. A source-level guard
  (`TestNoFiltersOnStatusesNothingWrites`) was added to make FR-041 verifiable, and it goes red with
  the filter restored.

Four tests were found to be **unfalsifiable as first written** during the phases themselves and fixed
at the time: two lifecycle tests asserted the state they seeded, and two more silently skipped or
exercised a code path that never ran. The pattern to watch is a test whose expected value equals its
setup, and a "confirm" task that asserts nothing.
