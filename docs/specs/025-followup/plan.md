# Implementation Plan: SPEC-025 Follow-ups

**Branch**: `feature/547` (feature dir `025-followup`) | **Date**: 2026-09-02 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/025-followup/spec.md`

## Summary

Seven defects that share one shape — **the product answers successfully and does not do what it was
asked** — fixed across four services with **no new persisted state, no schema change and no new
configuration setting**. (Review later added one data migration, `073`, for volume transfers.)

The technical approach is the same one SPEC-025 found: *the correct shared thing already exists and
one caller bypasses it.* Filter accumulation exists (`common.SQLFilter`, with identity's six
collections as the worked example); a race-free quota check exists (`nova.CheckQuota` over
`database.WithTx` with a row lock); the request-host-over-configured-host rule exists
(`common.BaseURL`); the limit and usage resolution exists and is already shared by three views.
Four of the seven stories are therefore a matter of calling what is there.

The cost is concentrated in three places, and none of them is where the spec originally put it:

1. **Enforcement sites, not the enforcement.** The quota check is one small helper. Planning
   re-derived FR-019's "every path" from the code and found **eight** call sites across four files,
   where the spec named two — three separate `INSERT INTO volumes` statements outside the shared
   provisioning routine, plus snapshots, backups and extend.
2. **Tests that can fail.** Every defect here is silent, so every requirement needs a negative
   assertion. This is the largest single block of work and the one most likely to be delivered
   wrong.
3. **The microversion audit**, bounded by FR-042 to what the product emits — and capped in value by
   a research finding (R4) that nobody had noticed.

Research resolved both upstream lookups the spec deferred and turned up three things that change
requirements as written: `host` is role-split upstream (so FR-004's divergence is two-sided), the
image-patch refusals are **403** upstream rather than the 400 the spec implied, and a request with
**no** version header resolves to upstream's *minimum* where Lightstack uses its *maximum* — a third
Article I deviation, larger than the two the spec took deliberately, and one that bounds what US6
can achieve.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+; Constitution requires 1.21+)
**Primary Dependencies**: `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL),
`google/uuid`. **No new Go modules.** Contract tests use the already-pinned `gophercloud v1.14.1`.
**Storage**: external PostgreSQL — **no schema change**. Every fix is behavioural; review later added one
data migration, `073`, giving the volumes of pending transfers the `awaiting-transfer` status. One
existing column gains a reader (`snapshots.size_gb`, for FR-026's corrected `gigabytes` count).
**Testing**: `go test ./...` for unit tests; `internal/nova/server_filter_test.go`,
`internal/nova/addresses_test.go`, `internal/cinder/quotas_test.go`, `internal/glance/images_test.go`
extended rather than created; `test/nova_gaps_test.sh`, `test/quota_test.sh`,
`test/spec007_catalog_scheme_test.sh` for integration; a real-mode host runbook for the five findings
that only a host surfaces.
**Target Platform**: Linux (real mode); every change is mode-independent and fully testable in stub
mode, since all seven defects are in API/database layers rather than in libvirt, netlink or storage
backends.
**Project Type**: single Go binary, six embedded HTTP services (Constitution Article VI).
**Performance Goals**: FR-045 — address lookups for a detailed server listing must not grow with the
server count. No other performance target; the quota check adds one indexed read inside an existing
transaction boundary.
**Constraints**: Article III fail-fast (no retries anywhere in this feature); Article I compatibility,
with **three** documented deviations (see Complexity Tracking); FR-007/FR-040 both require that a
request which works today keeps working.
**Scale/Scope**: 4 services (`nova`, `glance`, `cinder`, `keystone`) + `common`; 7 user stories, 57
functional requirements, 14 success criteria; ~8 quota call sites, ~11 filter predicates, 1 new shared
error helper, 0 schema migrations (1 data migration, `073`, added in review).

## Constitution Check

*GATE: evaluated before Phase 0, re-evaluated after Phase 1 design.*

| Article | Verdict | Reasoning |
|---|---|---|
| **I. Flamingo API compatibility** (NON-NEGOTIABLE) | ✅ **PASS under the v1.2.0 exception clause** | Three divergences, all four now entered in the constitution's **Known Divergences** register (entries 1, 2 and 4). Each satisfies the clause's four limits: necessary (no column exists / matching would break existing callers), recorded as a requirement (FR-004, FR-040a, FR-041a — not a comment or a plan table), narrow and enumerated (FR-004a bounds the filter set explicitly), and **never silent** (every one answers a client error the caller can see). Everything else in the feature moves *toward* upstream: the filters, the patch operations, the quota refusal, the 406 and the catalog host all become more upstream-correct, not less. |
| **II. Synchronous operations** | ✅ PASS | No new background loop, no queue, no deferred work. Every check completes inside the request that triggered it. The v1.1.0 reconciliation exception is not needed and not used. |
| **III. Fail-fast** | ✅ PASS | No retries, no backoff. The quota check is one indexed read plus a row lock inside a transaction the request already owns; a refusal returns immediately. |
| **IV. Multi-mode** | ✅ PASS | All seven defects live in API and database layers. Quota enforcement reads the same tables in stub and real mode; the address-map rewrite is pure SQL; the catalog, filters and patch are mode-independent. Every requirement is verifiable in stub mode, and the host pass exists to confirm the real-mode surfaces the defects were found on. |
| **V. Project isolation** | ✅ **PASS, strengthened** | FR-006 and FR-006a make explicit that a filter may only narrow — a filter can never substitute for the administrator-only cross-tenant opt-in. FR-031a closes a cross-caller path that would otherwise be *opened* by FR-031. |
| **VI. Single binary** | ✅ PASS | No new process, port or service. |
| **Test-First (workflow)** | ✅ PASS, load-bearing | SC-013 requires every new test be demonstrated failing against current code before the change. FR-009, FR-017, FR-029 and FR-048 each state the negative assertion that makes this possible. |
| **Backwards compatibility** | ⚠️ **PASS with two release-note items** | No schema migration; the one data migration added in review (`073`) is reversible. But two behaviours change for existing deployments: quota begins being enforced where it never was, and reported `gigabytes` usage rises for any project holding snapshots (FR-026). Both are correct and both are user-visible with no user action behind them. |
| **Security** | ✅ PASS | No credential, secret or token handling changes. FR-031a is a security requirement in its own right. |

**Gate result: PASS.** The Article I deviations are justified below rather than waived; two are
explicit product decisions recorded in the spec's Clarifications, and the third is a pre-existing
behaviour this feature documents rather than creates.

**Note on which constitution this feature is gated against.** The repository holds **two**
documents. `.specify/memory/constitution.md` is the Lightstack-specific one — six principles, now v1.2.0 —
and is what every recent feature spec (SPEC-023, SPEC-024, SPEC-025) has been evaluated against, and
what this plan uses. `.claude/memory/constitution.md` is generic spec-kit boilerplate at v1.0 with
nine unrelated articles (Library-First, CLI Interface, …), and it is the one `CLAUDE.md`'s "The Nine
Articles" section describes. The two do not agree on the number of principles, their content, or the
version. This is pre-existing and out of scope here, but it is a live trap: it is why an earlier draft
of this plan cited two articles that do not exist in the governing document.

### Post-design re-evaluation (after Phase 1)

Re-run against the six design decisions in Phase 1. **No new violations, and no verdict changes.**
Three things worth recording:

- **Article I gained its third deviation during Phase 0, not Phase 1.** R4's finding (no header →
  maximum) was invisible from the spec text; it is pre-existing behaviour that FR-040 requires be
  preserved, and it is now in Complexity Tracking rather than left to be rediscovered mid-implementation.
- **Article V got stronger than the spec planned.** FR-031a was not in the original spec at all — it
  came out of clarify, and Phase 1 (R7) confirmed the mechanism: the cache key is `project_id` alone
  with a comment asserting the catalog is immutable, which FR-031 makes false. The design closes a
  cross-caller path that FR-031 would otherwise have opened, so the isolation posture ends up better
  than before the feature.
- **The one new shared helper is not an abstraction.** `internal/common/errors.go` gains a 406
  constructor alongside its twenty existing ones — a missing member of an existing set, not a new
  layer. No new package, no interface, no indirection anywhere in the feature. (An earlier draft of
  this section cited "Article VII (simplicity)" and "Article VIII (anti-abstraction)"; those belong
  to the generic spec-kit document at `.claude/memory/constitution.md`, not to the Lightstack constitution
  at `.specify/memory/constitution.md` that this feature is gated against. See the note below.)

## Project Structure

### Documentation (this feature)

```text
docs/specs/025-followup/
├── spec.md              # Feature specification (11 clarifications, 57 FRs)
├── plan.md              # This file
├── research.md          # Phase 0 output — R1–R9, all upstream lookups resolved
├── data-model.md        # Phase 1 output — no new entities; the two reads that change
├── quickstart.md        # Phase 1 output — how to see each defect, and its fix
├── contracts/           # Phase 1 output — the seven observable contracts
│   ├── server-list-filters.md
│   ├── image-patch-operations.md
│   ├── block-storage-quota.md
│   ├── service-catalog-addressing.md
│   ├── server-actions.md
│   ├── compute-microversions.md
│   └── server-list-addresses.md
├── checklists/
│   └── requirements.md  # Spec-quality checklist (all items pass)
└── tasks.md             # Phase 2 output — NOT created by this command
```

### Source Code (repository root)

```text
internal/
├── nova/
│   ├── server_filter.go          # US1: honoured filters onto the existing SQLFilter base
│   ├── handlers.go               # US1: filter parsing in both listings; US7: one batched address query
│   ├── microversion.go           # US6: the shared negotiation helper; the 406 gate
│   ├── advanced_actions.go       # US5: RestoreInstance answers 409 only; US6: correct the 2.45 comment
│   └── *_test.go                 # server_filter_test.go, addresses_test.go extended
├── glance/
│   ├── images.go                 # US2: add/replace/remove, atomic patch, RowsAffected, 403/400
│   └── images_test.go            # extended: each op pinned by reading the value back
├── cinder/
│   ├── quotas.go                 # US3: the ctx-shaped check helper; FR-026's gigabytes correction
│   ├── volumes.go                # US3: create, boot-volume, extend, snapshot call sites
│   ├── backups.go                # US3: backup create + restore-into-new-volume call sites
│   ├── manage.go                 # US3: managed-import call site
│   ├── transfers.go              # US3: transfer-accept call site (FR-019a semantics)
│   └── quotas_test.go            # extended: per-resource refusal, boot path, concurrency
├── keystone/
│   └── auth.go                   # US4: BuildServiceCatalog + substituteURLTemplates + the cache key
└── common/
    └── errors.go                 # 406 Not Acceptable — the feature's one new shared helper

test/
├── nova_gaps_test.sh             # extended: filters, microversions, addresses
├── quota_test.sh                 # extended: per-resource refusal across the call sites
└── spec007_catalog_scheme_test.sh # extended: catalog under a non-configured host
```

**Structure Decision**: no new packages and no new files beyond tests. The feature edits nine
existing source files across five packages, which is what "the correct shared thing already exists"
implies — the changes are call sites and predicates, not structures. The one exception is
`internal/common/errors.go` gaining a 406 constructor, which belongs in `common` because the
microversion gate is the first caller and will not be the last.

## Phase 0 — Outline & Research

**Status: complete.** See [research.md](./research.md). All Technical Context unknowns resolved; the
spec's two deferred upstream lookups (FR-003, FR-010 – FR-013) answered from the api-ref and
api-guide via Context7.

Three findings that Phase 1 and `/cl.tasks` must carry:

- **R1**: `host` is role-split upstream and `hostname` is a second unsupportable filter; `ip` is
  expressible via the same `ports` query US7 builds, so it should be honoured if US7 lands and
  refused if it does not — never ignored. **Written back into the spec** as FR-003, FR-003a, FR-003b
  and FR-004's enumeration after the analyze pass found the three artifacts disagreeing on the set.
- **R2**: image-patch refusals are **403** upstream, not the 400 the spec implied.
- **R4**: a request with no version header resolves to upstream's *minimum*; Lightstack uses its *maximum*
  and FR-040 requires it to keep doing so. Every gate US6 adds therefore affects only clients that
  negotiate explicitly — which is how FR-044's tests must be written. **Now carried as FR-040a**; the
  analyze pass found this deviation documented in the plan but absent from the spec entirely.

## Phase 1 — Design & Contracts

**Status: complete.** [data-model.md](./data-model.md), [contracts/](./contracts/),
[quickstart.md](./quickstart.md).

Design decisions taken in this phase, each argued in research.md:

1. **One quota helper, `ctx`-shaped** (R5). `(ctx, projectID, resource, amount)` rather than
   `nova.CheckQuota`'s `*gin.Context`, because the compute boot path has no HTTP request. Same
   `WithTx` + `FOR UPDATE` interior.
2. **Check before backing store, not after** (R5). Placed at the top of `provisionVolume`, the two
   paths through it need no rollback at all; the three direct-insert paths check before their
   `INSERT`; transfer accept has nothing to roll back and must leave the transfer pending.
3. **Keep the catalog's prefix substitution, strip the port from the request host** (R7). The
   existing string replacement is what preserves each endpoint's own port; feeding it a host that
   carries a port produces the two-port authority `common.hostWithPort` was written to prevent, so
   `common`'s bracket-aware host split needs extracting or exporting.
4. **Cache key gains the host** (R7) rather than disabling the cache — caching is off by default, and
   silently removing a cache an operator enabled is worse than keying it correctly.
5. **Do not start trusting forwarded headers** (R7, FR-031b). `c.Request.Host` behind a correct
   ingress already carries the public name; a forwarded header is client-supplied on a direct
   connection, which is the hazard FR-031a exists to close. Document the proxy requirement instead.
6. **Batch the address query by instance id, preserving the project pairing** (R8) via
   `JOIN instances i ON i.id = p.device_id AND i.project_id = p.project_id`. Dropping the project
   predicate would let a mismatched port contribute an address.

## Phase 2 — Task planning approach

Not executed by this command. When `/cl.tasks` runs, the ordering that falls out of the above:

- **US1, US2, US3 and US7 are independent** and can be built in any order. US3 is the largest by a
  wide margin (eight call sites, four files, a counter correction with a visible consequence).
- **US7 should precede or accompany US1**, because both edit the same listing path and R1 makes the
  `ip` filter's fate depend on US7's query existing.
- **US6 depends on the new 406 helper** in `common`, which is a one-file prerequisite.
- **US4 and US5 are self-contained**, and US5 is the smallest change in the feature (a handler reduced to one answer).
- Every story's tests come first and must be demonstrated RED (SC-013). For US1 and US3 that means
  writing the *negative* assertion first — the non-matching filter, the over-limit request — since
  the positive case passes against today's code.

## Complexity Tracking

Three Article I divergences. Article I is NON-NEGOTIABLE and, as of constitution **v1.2.0**, carries a
bounded exception clause requiring each divergence to be necessary, recorded as a requirement, narrow
and enumerated, and never silent. All three satisfy it and are entered in the constitution's Known
Divergences register, so the table below is the justification the clause requires — not a waiver of
the principle. The amendment was made because Article I was already contradicted by three shipped
behaviours, only one of which this feature introduces.

| Deviation | Why needed | Simpler / more compatible alternative rejected because |
|---|---|---|
| **FR-004** — refuse a server-list filter naming an attribute the product does not record (`host`, `hostname`, `availability_zone`, `tags`, plus `ip` until FR-003b's condition is met), where upstream honours it for administrators and **silently ignores** it for everyone else (R1) | The product records no host assignment, so honouring is impossible. The two compatible answers are both silent: an empty list reads as a fact about the deployment, and an ignored filter is precisely the defect this feature exists to remove. Explicit product decision, taken in clarify with the divergence understood | Returning an empty collection was rejected as indistinguishable from "nothing matched". Ignoring was rejected as being the defect. Mirroring upstream's role split was rejected because "honour for administrators" cannot be implemented without a column that does not exist |
| **FR-041a** — treat an unparseable version header as no negotiation, where upstream answers 400 | SPEC-025 took this decision deliberately and documented it in the code as A-006: the gate exists to avoid granting *more* than a client negotiated, and that property holds without inventing a rejection on a surface that never had one. FR-040 independently requires existing callers not to change | Adopting upstream's 400 was rejected because it turns requests that succeed today into failures for no compatibility gain — no client sends a malformed header deliberately, and the product's answer is already conservative |
| **R4 / FR-040a** — a request with **no** version header resolves to the *maximum* advertised version, where upstream assumes the *minimum* | **Pre-existing, not introduced by this feature.** Adopting upstream's default would remove behaviour from every current Lightstack caller in one release. FR-040 forbids exactly that | Adopting the minimum was rejected as a breaking change to every existing caller. Making it configurable was rejected as a new setting for a question nobody has asked, and it would double the test matrix of every gate US6 adds. The cost is recorded instead: FR-043's audit note must state that Lightstack's advertised `min_version` is aspirational in this second, distinct way |

**Not a deviation, but a scope note**: the `gigabytes` usage correction (FR-026) and the arrival of
quota enforcement are both visible changes for existing deployments with no user action behind them.
They are backwards-compatibility items for the release notes, listed in the Constitution Check above,
not Article I deviations — the new behaviour is what upstream does.
