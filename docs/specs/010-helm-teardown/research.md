# Research: Teardown via Helm Pre-Delete Hooks (Phase 0)

**Feature**: SPEC-010 | **Date**: 2026-06-15 | **Input**: [spec.md](./spec.md), [plan.md](./plan.md)

The spec's 9 clarifications (3 rounds) already resolved the policy questions (DB reset, multi-node deferral, PVC/hostPath, backend ownership, fail-closed, quiesce). This document resolves the remaining **design/technical** unknowns the plan must commit to. All codebase facts below were verified against the tree on 2026-06-15 (round-3 audit).

> **Superseded in part by the 2026-06-24 scope change** (see `spec.md` scope note): R5's DB-schema-reset decision (`database.MigrateReset`) is **obsolete** — the database is operator-owned and is never reset; teardown reads it read-only to enumerate. The purge scope was also widened to all `/var/lib/o3k` trees + full host network state. This file is point-in-time design; `spec.md` is authoritative.

---

## R1 — Where do the Kubernetes-API steps (scale, PVC delete) run?

**Decision**: Keep the **o3k binary Kubernetes-API-free**. The K8s-API steps run in the chart's **kubectl-image pre-delete Job** (operator-supplied image, no implicit pull); the host/storage/DB purge runs in a **privileged `o3k cleanup` Job**. Two ordered hooks, two images, one binary.

**Rationale**:
- SPEC-008 FR-009c is explicit that the product **must not be required to integrate with the Kubernetes API** (decided there for floating IPs). Embedding `client-go` for teardown cuts against that intent and bloats the ~35 MB single binary (Constitution VI).
- A privileged host-purge pod is unavoidable anyway (libvirt socket + host netns + `hostPath` mounts), and a kubectl image is a trivial, standard, widely-trusted artifact. Requiring it (configurable, fail-fast when missing) matches the chart's existing "operator supplies all images" posture (FR-018).
- `o3k cleanup` stays runnable standalone (Compose/bare-metal): it does host+DB purge with no k8s involved; only the chart adds the k8s orchestration.

**Alternatives considered**:
- *Embed `client-go` in the binary; `o3k cleanup` scales the Deployment and deletes PVCs itself (one image, one Job).* Rejected: couples the product to the K8s API (against FR-009c), enlarges the binary for a teardown-only path, and makes the standalone binary carry k8s machinery it never uses elsewhere.
- *Bundle `kubectl` into the o3k image.* Rejected: bloats every deployment for a teardown-only need; the configurable-image split is cleaner.

> **Flag for plan review**: R1 introduces a required `teardown.kubectlImage` value (operator-facing). If the team prefers one image over K8s-agnosticism, switch to the `client-go` alternative — it collapses to a single purge Job and drops `kubectlImage`. Recommended as written.

---

## R2 — How is the cleanup invoked from the binary?

**Decision**: A new **`o3k cleanup` subcommand**. `cmd/o3k/main.go` checks `os.Args[1]` **before** the server startup path: if it is `cleanup`, dispatch to `internal/teardown` and exit; otherwise run the servers as today. The cleanup logic lives in a new `internal/teardown` package (library-first).

**Rationale**:
- FR-010 requires the cleanup to be an operation of the **existing** binary, not a new artifact. There is precedent for a sibling helper (`cmd/o3k-migrate`), but FR-010 explicitly wants it in `o3k`, and the hook invocation reads naturally as `o3k cleanup`.
- `main.go` today only `flag.Parse()`s `--config`/`--migrations`; a leading-subcommand check (the pattern `cmd/o3k-migrate` already uses via `os.Args`) is the minimal, idiomatic addition.
- A package keeps the orchestration testable in isolation (stub mode) per Constitution III/IV.

**Alternatives considered**: a `--cleanup` boolean flag (rejected — subcommand reads better and matches `o3k-migrate`); a third binary (rejected — FR-010 wants it in `o3k`).

---

## R3 — Enumeration source and purge order

**Decision**: Enumerate **from the database** (the source of truth), purge in dependency-safe order, reset the DB **last**:

1. **Quiesce** (in the kubectl Job, before this Job runs): Deployment → 0, wait for pods gone.
2. **Enumerate** from DB: `instances.libvirt_domain_id`; `networks`/`routers` → `light-ns-<project_id>` / `qrouter-<router_id[:11]>`; `ports` → bridges/TAP; `volumes`/`images` → local file paths.
3. **Compute**: `VMManager.DeleteVM` per domain (destroy + undefine).
4. **Network**: `DeleteNamespace` / `DeleteRouterNamespace` (+ bridges, TAP, iptables, VXLAN) per enumerated id.
5. **Storage**: delete node-local volume/image files under `<hostStoragePath>/{volumes,images}`; within that tree, sweep orphans (files with no DB record). The sweep is **bounded to `hostStoragePath`** — never a global filesystem scan — and that tree is single-release by FR-003b (two real releases can't co-reside on a node; stub has no `hostPath`), so it holds only this release's files.
6. **DB reset**: `database.MigrateReset()` — last, so enumeration data survives until host purge completes.

**DB-unreachable fallback (C9)**: if the DB cannot be reached, **ignore it** — do not fail. Enumerate host state by convention instead: `ip netns` filtered to the `light-ns-`/`qrouter-` prefixes, and the libvirt `ListAllDomains` set; for storage, wholesale-purge `<hostStoragePath>/{volumes,images}` (safe — single-release per FR-003b); skip the DB reset.

**Rationale**: DB-first gives exact, release-scoped ids (FR-007) and avoids blunt prefix wildcards that could touch a co-resident release. Reset-last preserves the enumeration source (FR-019). The prefix/daemon fallback keeps host purge working when the DB is already gone, honoring C9 without hanging (Constitution III).

**Alternatives considered**: prefix-wildcard as the *primary* enumeration (rejected — imprecise, cross-release risk, and unnecessary since the DB is present in the normal quiesced flow); reset-first (rejected — destroys the enumeration source).

---

## R4 — Hook topology, ordering, blocking & escape

**Decision**: All teardown resources are `helm.sh/hook: pre-delete`, `hook-delete-policy: before-hook-creation,hook-succeeded`, ordered by `hook-weight`:

| Weight | Resource | Role |
|-------:|----------|------|
| -20 | ServiceAccount + Role + RoleBinding | least-privilege RBAC for the kubectl Job |
| -10 | kubectl Job | quiesce (scale→0, wait) **then** delete release PVCs (FR-005) |
| 0 | privileged `o3k cleanup` Job | host + storage purge + DB reset; **blocking, fail-closed** |

- **Blocking** (FR-001): Helm runs all pre-delete hooks to success before deleting the release; a non-zero purge Job fails the hook → `helm uninstall` aborts with the Job's error.
- **Bounded** (FR-016): the purge Job sets `activeDeadlineSeconds: {{ .Values.teardown.timeoutSeconds }}` (default 600) and `backoffLimit: 0` (no silent retries).
- **Escape hatch** (C5): `helm uninstall --no-hooks` skips the purge and removes the release — documented, since `helm uninstall` accepts no value overrides.

**Rationale**: PVC deletion folds into the quiesce Job (weight -10) because pods must be gone before a PVC can be deleted (finalizers), and the host purge reads `hostPath`, not PVCs — so PVC-delete is order-independent w.r.t. the purge and need not be a third Job. Hook-scoped RBAC (weight -20) means the permissions exist only during teardown.

**Alternatives considered**: a separate weight-10 PVC-delete Job (rejected — unnecessary; folds cleanly into quiesce); RBAC as normal (non-hook) templates (works — Helm deletes them after pre-delete hooks — but hook-scoping keeps them teardown-only and tidier).

---

## R5 — Database reset mechanism

**Decision**: Call `database.MigrateReset(dbURL, migrationsPath)` **directly** from `internal/teardown` (drop all tables, then re-`Up` to a clean schema). Gated on `delete`. Skipped if the DB is unreachable (C9).

**Rationale**: `o3k-migrate reset` is interactive-only — it blocks on a typed `RESET` and has no `--force`/env (verified `cmd/o3k-migrate/main.go`); a hook cannot drive it. The underlying `MigrateReset` is a plain library function (`internal/database/migrate.go`) the cleanup can call without a prompt. Re-`Up` leaves a current empty schema, so the redeploy's startup migration is a no-op (pristine — SC-002). The o3k image already ships the migrations directory (the server runs `MigrateUp` on boot), so the Job has it.

**Alternatives considered**: add a `--force` flag to `o3k-migrate reset` and shell to it (rejected — extra surface and a second process; the library call is simpler); drop-only without re-`Up` (rejected — re-`Up` makes the redeploy a clean no-op and is what `MigrateReset` already does).

---

## R6 — FR-020: Cinder local-volume path fix

**Decision**: Change `pkg/storage/ceph.go` (`NewCephClient`) so local volumes live under the **same base as Glance** — `/var/lib/o3k/volumes` (the path the reference Dockerfile already pre-creates), with `~/.o3k/volumes` retained as a non-container fallback, and the base path configurable. Mirror the existing `pkg/storage/image_store.go` pattern exactly.

**Rationale**: Verified — Glance uses `/var/lib/o3k/images` (on the chart's `hostPath` mount) while Cinder uses `~/.o3k/volumes` ("more portable than /var/lib"), which in a pod is ephemeral and off-mount. That breaks persistence (latent SPEC-008 FR-012a) and makes the teardown's storage purge (FR-005) unable to find volumes. Aligning the two paths fixes both with a localized change and a matching test.

**Alternatives considered**: have the purge scan *both* `/var/lib/o3k/volumes` and `~/.o3k/volumes` and leave the code as-is (rejected — leaves the persistence bug unfixed and bakes the inconsistency into the teardown); convert to PVCs (rejected in spec C3).

---

## R7 — RBAC scope (least privilege)

**Decision**: A namespaced **Role** (release-scoped, RoleBinding to the hook SA) granting exactly:

- `apps` / `deployments`: `get`, `list`, `patch` (scale to 0)
- core / `pods`: `get`, `list` (wait for termination)
- core / `persistentvolumeclaims`: `get`, `list`, `delete` (FR-005)

No cluster-scoped rights; no `secrets`/`nodes`/cross-namespace access.

**Rationale**: FR-011 — exactly what the kubectl Job needs and no more. Namespaced Role (not ClusterRole) keeps it release/namespace-bound (FR-007). The privileged o3k purge Job needs **no** K8s RBAC (it talks to the host and DB, not the API) — only the kubectl Job binds to the SA.

**Alternatives considered**: a ClusterRole (rejected — teardown is namespace-local); broader verbs (rejected — least privilege).

---

## R8 — Test strategy (TDD; Constitution III)

**Decision** — write tests RED first, in three layers:

1. **Go unit** (`internal/teardown/teardown_test.go`, stub mode, no privileges): enumeration maps DB records → the right delete calls; correct order; idempotency (not-found = success); DB-unreachable fallback path. Plus a `pkg/storage` test asserting Cinder + Glance local paths share the configured base (FR-020).
2. **helm-unittest** (`charts/o3k/tests/teardown_test.yaml`): the RBAC + both Jobs render **iff** `dataRetentionPolicy: delete`; **absent** under `retain` (SC-004 no-regression); RBAC verbs are the least-privilege set (R7); the purge Job carries `activeDeadlineSeconds` + `backoffLimit: 0`; `kubectlImage` empty under `delete` → template `fail`.
3. **Scenario** (`test/helm_teardown_test.sh`, single-node stub on kind/k3s): install → create resources → `helm uninstall` with `delete` → assert the uninstall **blocks** on the hook, then zero residue (no workloads/PVCs/domains/netns/local-storage; DB schema empty) → **immediate reinstall** → identity check passes, no stale resources (SC-001/002/003). A `retain` run asserts SPEC-008's existing behavior is intact (SC-004). A **second `delete` teardown against the already-clean release** must exit success (SC-005 idempotency; US3 test (c)).

**Deferred (issue #52, tagged `[DEFERRED #52]`)**: multi-node DaemonSet host-purge across N nodes (SC-007) — needs functioning cross-node real mode.

**Rationale**: stub-mode unit + render tests give fast, host-free CI coverage of the logic and the gating; the scenario test exercises the real blocking/redeploy contract on a single node. Matches SPEC-008's test layering.

---

## R9 — Idempotency, fail-closed granularity, observability

**Decision**:
- **Idempotent** (FR-008): every delete treats not-found / already-absent as success; re-running the teardown succeeds.
- **Fail-closed required classes** (FR-016): the purge Job exits non-zero (→ uninstall aborts) if it could not complete **compute**, **network**, **node-local storage**, or (when the DB is reachable) the **DB reset**. Per-artifact failures within a class are best-effort-logged but still roll up to a class failure. An **unreachable DB is not a failure** (C9). PVC deletion failures (kubectl Job) also fail the teardown.
- **Observable** (FR-012): structured JSON logs list each artifact purged / skipped-absent / failed, with a final summary line; the Job's logs are the audit trail.

**Rationale**: makes "no state leakage" verifiable and the blocking contract precise, while idempotency keeps retries and partial-state safe.

---

## Resolved unknowns summary

| Unknown | Resolution |
|---------|------------|
| K8s-API access for scale/PVC | Kubectl hook Job; binary stays K8s-free (R1) |
| Invocation | `o3k cleanup` subcommand + `internal/teardown` (R2) |
| What/which order to purge | DB-first enumeration; reset last; host-prefix fallback if DB down (R3) |
| Hook wiring/blocking/escape | pre-delete, weighted, fail-closed + `activeDeadlineSeconds`; `--no-hooks` escape (R4) |
| DB reset | `database.MigrateReset()` direct call (R5) |
| Cinder path | Move under host storage base, mirror Glance (R6) |
| RBAC | Namespaced least-privilege Role (R7) |
| Tests | Unit + helm-unittest + single-node scenario; multi-node deferred (R8) |
| Idempotency/fail-closed/logs | Defined (R9) |

No open `NEEDS CLARIFICATION` remain. One item flagged for plan review (R1's `kubectlImage` vs. embedding `client-go`).
