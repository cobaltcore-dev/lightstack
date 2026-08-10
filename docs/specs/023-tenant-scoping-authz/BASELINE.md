# Baseline: before SPEC-023

**Captured**: 2026-08-10 | **Branch**: `feature/549` | **Base**: `feature/545` @ `9992ebf`

Recorded before any code change so every later RED→GREEN claim is measured rather than asserted (T001–T003, FR-022).

---

## Static baseline (T002)

| Measure | Command | Value |
|---|---|---|
| Role checks in compute | `grep -h 'HasRole' internal/nova/*.go \| grep -v _test \| wc -l` | **4** — all administrator-*only* gates |
| Project-scoped predicates in compute | `project_id = $N`, non-test files | **102** |
| Handler reads of caller project | `GetString("project_id")` in `internal/nova` | **56** |
| `all_tenants` anywhere in the product | `grep -rc 'all_tenants' --include='*.go' .` | **0 files** |
| Role checks in `aggregates.go` | `grep -c 'HasRole\|Forbidden'` | **0** across **9** handlers |
| Scoping in `migrations.go` | `HasRole` / `project_id = $` | **0** and **0** across 5 handlers |

The five `project_id` matches in `migrations.go` are route doc-comments (`/v2.1/:project_id/...`), not predicates.

## Build & vet

| Gate | Result |
|---|---|
| `go build ./...` | PASS |
| `go vet ./internal/...` | clean |

## Unit tests (T001)

| | Count |
|---|---|
| PASS | **172** |
| SKIP (require a database) | 115 |
| FAIL | **0** |

## Contract tests (T001)

| Package | Result |
|---|---|
| `test/contract/cinder` | ok |
| `test/contract/glance` | ok |
| `test/contract/keystone` | ok |
| `test/contract` | **FAIL** |
| `test/contract/neutron` | **FAIL** |
| `test/contract/nova` | **FAIL — 47 failures, 0 passes** |

### ⚠ Pre-existing environment defect — not a code failure

Every one of the 47 nova contract failures is the same:

```
Expected HTTP response code [201 202] when accessing [POST /v2.1/servers], but got 413
{"overLimit":{"code":413,"message":"Quota exceeded for resource: instances"}}
```

Root cause is **not** leftover instances — `totalInstancesUsed` is `0`. The `quotas` table carries a
zero compute quota for the `default` project:

```
default|cores|0
default|instances|0
default|ram|0
```

so `GET /v2.1/limits` reports `maxTotalInstances: 0` and every server create is refused before it
starts. The same zeroes exist for a `cloud_admin` project, and the database also contains a
`limes-demo` project — this looks like debris from quota-service experimentation on another branch
(`test/limes`), sharing the single `lightstack-postgres` container.

**This blocks any contract test that creates a server**, which is most of the nova suite and several
of this feature's planned tests. It is unrelated to SPEC-023 and predates it.

**Resolution pending user decision** — the database is shared with other worktrees, so it is not
being modified unilaterally. Options recorded in the implementation log.

## Environment

| | |
|---|---|
| `lightstack-postgres` | podman container, `postgres:18`, up 2 days, `0.0.0.0:5432` |
| lightstack binary | running on the host (not containerised); keystone `200`, nova `200` |
| Projects in DB | `default`, `cloud_admin`, `limes-demo` |

## Known-red before this feature

- `make test-all` is expected RED on **B10** (#546) — Neutron 404s use Nova's error envelope, and
  `test/error_handling_test.sh` TEST 6 asserts otherwise. Pre-existing, tracked in the Neutron
  sub-ticket, and must not be attributed to this feature.
- The 47 nova contract failures above (environment, see ⚠).

---

# After SPEC-023

## Consistency audit (T077, SC-002)

The mitigation for the 44-handler consistency risk: every remaining direct read of
`c.GetString("project_id")` in `internal/nova` is accounted for, and none of them is a
point lookup that should have been widened.

```
grep -n 'GetString("project_id")' internal/nova/*.go | grep -v _test
```

| Count | Site | Verdict |
|---|---|---|
| 16 | `advanced_actions.go` — Suspend, Resume, Shelve, Unshelve, Resize ×2, ConfirmResize, RevertResize, Migrate, LiveMigrate, AddSecurityGroup, RemoveSecurityGroup, ChangePassword, Restore, CreateBackup, ResetNetwork | **Correct.** All are dispatch-only through `ServerAction`, which resolves the target and rebinds the context to the OWNING project before dispatching. These reads therefore return the owner, not the caller. |
| 1 | `handlers.go` — `CreateImageAction` | **Correct.** Dispatch-only, same rebinding. |
| 4 | `authz.go` | **Correct.** The helpers that implement the rule. |
| 1 | `handlers.go` — `CreateServer` | **Correct by design.** Creation is never cross-project (FR-004a). |
| 1 | `handlers.go` — `GetLimits` | **Correct by design.** `GET /v2.1/limits` reports the caller's own limits; upstream is the same (research R4). |
| 1 | `quotas.go` — `GetQuotaSetDefaults` | **Correct by design.** Quota routes resolve their `:id` through `common.QuotaTargetProject`; this is the caller fallback. |
| 2 | `handlers.go` — `ServerAction` | **Correct.** One is the debug log line (labelled `caller_project`); one is inside a comment. |
| 4 | `servergroups.go` | **Out of scope.** `os-server-groups` is a distinct resource, not a server operation, and FR-004 does not name it (research R4). |

**Zero** point lookups remain that scope to the caller where the override should apply.

Two dead reads found by this audit and removed: `issueConsoleURL` and
`GetConsoleOutputAction` still read the caller's project to pass to
`lookupConsoleInstance`, whose project parameter became vestigial when the rule moved
into `targetInstance`. The parameter is gone.

What the audit cannot see: it finds reads of the caller's project, so a handler that
reads no project at all is invisible to it. The three server-metadata handlers were such
a case — they checked only that the instance existed, with no project and no role, so any
tenant could read or rewrite another tenant's server metadata by id. They now resolve
through `resolveServerOr404` like every other per-server route
(`TestServerMetadataFollowsTheOverride`).

## Gates after implementation

| Gate | Before | After |
|---|---|---|
| `gofmt -l internal/` | clean | clean |
| `go vet ./...` | clean | clean |
| `go build ./...` | OK | OK |
| Unit tests | **172 pass / 0 fail** | **265 pass / 0 fail** |
| Migrations changed | — | **none** (`git diff --stat migrations/` empty), as data-model.md promised |

The unit count rose by 93: partly the new tests, partly because the DB-backed tests now
run rather than skip (`LIGHTSTACK_TEST_DB_URL` pointed at a throwaway `lightstack_test_549` database).

## Defects found and fixed beyond the specification

Three, all surfaced by tests rather than by reading:

1. **Paging repeated the marker row.** `ROW_NUMBER() - 1` made the offset point *at* the
   marker rather than past it, so walking pages returned some servers 2–4 times. A marker
   is exclusive upstream. Only surfaced by seeding five servers sharing a `created_at`.
2. **`GET /v3/projects` returned 500 on legitimate data.** `projects.domain_id` and
   `description` are nullable in the schema but were scanned into plain strings, so one
   project without a domain broke the entire collection.
3. **A SPEC-021 regression, caught before it landed.** Resolving the instance at the top
   of `ServerAction` turned the unsupported-console-type refusal from `400` into `404`.
   The console authorizes itself, so the resolve belongs after its dispatch.

Found later, in review: **guests never received the server's metadata.**
`GetMetaDataJSON` selected `key, value` from `instance_metadata`, whose columns are
`meta_key` and `meta_value`, and swallowed the error, so every guest's `meta_data.json`
carried an empty `meta`. It now reads the real columns and fails the request on a query
error, like the reads around it (`TestMetaDataJSON_ServesServerMetadata_DB`).

## Contract suites against a live stack (T084)

Run against `bin/lightstack` built from this branch, backed by a freshly migrated database.

| Suite | Result |
|---|---|
| `test/contract/keystone` | **ok** |
| `test/contract/neutron` | **ok** |
| `test/contract/nova` | **ok — 88 pass, 0 fail** (including the two revived RBAC tests) |
| `test/contract/cinder` | FAIL — environmental, see below |
| `test/contract/glance` | FAIL — environmental, see below |
| `test/contract` (root) | FAIL — environmental, see below |

### The cinder/glance failures are environmental, not this feature

Root cause chain, from the service log:

```
mkdir /var/lib/lightstack: permission denied          ← storage root not writable as non-root
qemu-img create /var/lib/lightstack/volumes/….qcow2   ← No such file or directory
POST /v3/volumes → 503                          ← no volume created
POST /v3/volumes//action → 500                  ← empty id in the URL, uuid cast fails
```

`/var/lib/lightstack` requires root, which the SPEC-009 notes already record ("real mode needs REAL
root"). No volume can be created, so every test that needs one fails downstream.

This feature changes `internal/common`, `internal/keystone` and `internal/nova` only — it touches
no storage path. The three suites that exercise this feature's surface (nova, keystone, neutron) are
green.

## Live verification of the ticket's own reproduction

```
POST /v3/auth/tokens  scope={"domain":{"name":"Default"}}
```

| | Before | After |
|---|---|---|
| status | `201` | **`400`** |
| body | token with `has_project=false`, `has_domain_scope=false`, `roles=0` | `{"badRequest":{"code":400,"message":"domain-scoped tokens are not supported; use a project scope or an unscoped token"}}` |

And the regression guard, live: a project scope carrying a nested domain qualifier — the form every
real client sends — still returns **`201`**.

## Multi-tenancy suite (T062–T066)

`./test/multi_tenant_test.sh` grew from 11 checks to 24: the six original isolation checks
(all still passing), plus positive coverage of the override, both directions on the
operator-only surfaces, and the domain-scope pair.

| Run | Passed | Failed |
|---|---|---|
| Before this feature | 10 | 1 (volume creation — `/var/lib/lightstack` permission, environmental) |
| After | **23** | 1 (same environmental failure) |

### The deliberately-red demonstration (T064, FR-020)

The suite's green result has to be evidence, not assumption. Procedure, actually performed:

1. Neutralise the project predicate in `targetInstance` — change
   `WHERE project_id = $2 AND (…)` to `WHERE ($2 = $2) AND (…)`, so the non-administrative
   lookup no longer scopes to the caller.
2. Rebuild, restart, run the suite.
3. Restore, rebuild, run again.

| | Passed | Failed | First failure |
|---|---|---|---|
| Mutated | 20 | **4** | `✗ User2 can delete User1's server (ISOLATION BREACH)` |
| Restored | **23** | 1 | (environmental only) |

The suite detects a removed scoping predicate on its very first cross-tenant check, and the
override checks fail alongside it — so both directions are load-bearing, not decorative.

### What the suite now covers that it did not

- An operator **can** reach another project's server (the override was previously only
  ever tested negatively, so re-narrowing it would have left the suite green).
- The default listing does **not** widen for an administrator, and `?all_tenants` — in the
  bare, valueless form Horizon sends — does.
- A non-administrator's `all_tenants` produces a byte-identical answer to the same request
  without it, so the parameter's existence does not leak.
- Each operator-only surface refuses a tenant **and** admits an operator.
- A tenant cannot create a host aggregate (the privilege escalation, specifically).
- A domain-scoped authentication is refused `400`, while a project scope carrying a nested
  domain qualifier still returns `201`.

## Dangling state after cross-project deletion (T067)

Checked directly against the database after the suite runs, which delete servers as both
the owner and an administrator:

| Check | Result |
|---|---|
| Ports referencing a deleted instance | **0** |
| Console tokens referencing a deleted instance | **0** |
| Action-history rows referencing a deleted instance | 850 (326 `create`, 524 other) |

The first two are the ones a tenant can observe, and both are clean.

The action-history rows are **by design and pre-existing**: `DELETE FROM instances` has never
cascaded to `instance_actions`, and 326 of the orphans are `create` rows that predate this
feature. They are inert — the read path resolves the instance before returning history, so
requesting a deleted instance's actions answers **404** even as an administrator (verified).

This feature does increase the rate at which such rows accumulate, because FR-007a extends the
write path from creation alone to every overridden operation. That is unbounded growth in an
append-only audit table with no retention policy. It is not a correctness or confidentiality
problem, and no requirement covers it — recorded here as a known consequence rather than
fixed, since adding retention would be scope this feature was not given.

## Services this feature does not edit (T087, SC-004)

| Suite | Result |
|---|---|
| `test/contract/neutron` | ok |
| `test/contract/keystone` | ok |
| `internal/cinder` | ok |
| `internal/glance` | ok |
| `internal/neutron` | ok |

## Deferred to the real-mode pass

- **T086 — Horizon Admin panel verification.** The Constitution's "Horizon dashboard tested
  (if UI-affecting changes)" gate. US6 gates `/os-aggregates` and `/os-migrations`, which the
  Admin panel drives, and US1 changes `all_tenants`, which Admin › Instances sends. This needs
  a deployed Horizon, which the runs above did not have; the contract tests and the
  multi-tenancy suite cover the API-level behaviour only. Closed by the real-mode host
  validation below — **R13, PASS**.

---

# Real-mode host validation on k3s — 2026-08-11

Run against `charts/lightstack` + `charts/horizon` on k3s, real libvirt, real iptables networking, real local
storage. Every service mode verified `real`/`iptables`/`local` before testing.

| Step | Covers | Result |
|---|---|---|
| R1 | operator reads another tenant's server | **PASS** — `200`, `tenant_id` = `spec023-b`, `addresses` = real IP on `net-b` |
| R2 | tenant refused, indistinguishably from absent | **PASS** — both `404`, bodies byte-identical |
| R3 | operator acts on another tenant's server | **PASS** for authorization; one unrelated defect (below) |
| R4 | listings widen only on explicit opt-in | **PASS** — default `0`; `=1`/`=true`/`=yes`/bare all `1`; 2 distinct `tenant_id`; non-admin byte-identical |
| R5 | a name never spans projects | **PASS** — `404`, never the other tenant's id |
| R6 | actions attributed to the owning project | **PASS** — 6 admin actions recorded against `spec023-b` with the admin's `user_id` |
| R7 | domain scope refused; nested qualifier unaffected | **PASS** — `400` uniformly for all 4 references; nested form `201`; CLI authenticates |
| R8 | usage confidentiality | **PASS** — tenant `403`/admin `200`; A naming B gets A; malformed `404` |
| R9 | operator-only surfaces, both directions | **PASS** — all three `tenant=403 admin=200` |
| R10 | the privilege escalation | **PASS** — all 6 tenant writes `403` incl. the unknown action; admin `200` |
| R11 | identity collection filtering | **PASS** — `?name=member` `1`, `?name=nope` **`0`**, unfiltered projects `200` |
| R12 | isolation not regressed | **PASS** — 4 cross-tenant attempts `404`, B's server still `200` for its owner |
| R13 | **Horizon Admin panel** (Constitution merge gate, T086) | **PASS** — all four walkthrough steps |
| R14 | services this feature does not edit | keystone **ok**; neutron **FAIL** (known, below); nova passed 88/0 earlier this session |

## Two failures, neither caused by this feature

**`os-start` after `os-stop` returns `500`** — see research R13. The **owner** fails identically on
their own server, nothing here touches `StopVM`/`StartVM`, and the pod had 0 restarts. Afterwards the
database reports `ACTIVE` while libvirt has no such domain — a silent divergence nothing reconciles.

**Neutron port creation returns `500`** — `failed to attach tap-… to bridge br-… in namespace
light-ns-…: Device does not exist`. This is **F10 in sibling ticket #546** verbatim: `CreateNetwork`
makes the bridge in the root netns while `CreatePort` attaches the TAP inside `light-ns-<project>`,
which is never created. `git status internal/neutron/` is empty — this feature changes no Neutron
code.

The nova contract suite passed **88 / 0** earlier in the session against a clean stack; on the
post-R-series stack it hangs, plausibly on tests waiting for a server to reach `ACTIVE` after the
`os-start` defect left domains unstartable.

## Defects in the runbook itself, found by running it

Worth recording because all three were the failure mode this feature exists to fix — a check that
cannot fail:

1. **The catalog check greped the wrong column** (`-c Name` yields `glance`, not `image`), so it
   matched nothing and printed nothing, which read as success. Replaced with localhost-vs-node counts.
2. **The image check printed only the name** while its Pass line demanded `active`; a failed upload
   leaves the image `queued` and `image list` shows the name either way. Now prints `status`.
3. **The R5 check piped a 404 body through `jq -r '.server.id + …'`**, rendering a refusal as
   `  owner=`. Now prints the status code.

And two fixture bugs: `openstack network create --project X` as admin puts the network in the
*admin's* project (research R12), and a stray host-local `lightstack` process shadowed the k3s deployment
because both bind the node's ports under `hostNetwork`.
