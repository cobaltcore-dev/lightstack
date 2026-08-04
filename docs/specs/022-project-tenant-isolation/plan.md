# Implementation Plan: Project/Tenant Lifecycle & Isolation Fixes

**Branch**: `feature/545` (feature id `022-project-tenant-isolation`) | **Date**: 2026-08-04 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/022-project-tenant-isolation/spec.md`
**Ticket**: [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545) (sub-issue 1 of 4 under [#536](https://github.com/cobaltcore-dev/cloud-storage/issues/536))

## Summary

Four pre-existing defects, each with a working reference implementation already in the repository.
The plan is deliberately unambitious: copy what works, do not invent.

1. **B5/P1** — a shared domain-reference resolver in `internal/keystone` replaces two hard-coded
   default-domain literals and one unvalidated pass-through, so `domain_id: "default"` resolves instead
   of reaching a `uuid` column. Also applied to domain *retrieval* and the domain list's `name` filter,
   because standard clients resolve `--domain` before they POST anything.
2. **B8** 🔒 — `CreateSubnet` resolves its target network before inserting, with an
   owner-or-administrator rule (stricter than the `CreatePort` neighbour it copies its shape from).
3. **B2** — four routes added to Cinder's project-scoped group: the ticketed collection plus the three
   sub-resource names a sibling `:id` route currently swallows.
4. **T1 + harness** — the multi-tenancy suite gains setup guards, trap-based teardown, and the two
   checks whose absence let B8 and B2 escape it for this long.

Total production-code surface: **five files**, no new package, no new dependency, no migration.

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL),
`jackc/pgx/v5/pgconn` (SQLSTATE inspection, wrapped behind `common.IsUniqueViolation`),
`google/uuid` (`uuid.Validate`), `rs/zerolog` (structured logging). **No new modules.**
**Storage**: External PostgreSQL. **No schema change, no migration** — every fix is behavioural.
**Testing**: `go test` unit tests per package, gated on `O3K_TEST_DB_URL` via the existing `TestMain`
pattern (`internal/keystone/maindb_test.go`); bash integration suites in `test/`;
`test/contract/keystone/` (gophercloud) must stay green.
**Target Platform**: Linux (stub mode is sufficient for every fix; also runs on macOS)
**Project Type**: Single-binary web service — six HTTP services in one process (`cmd/o3k/main.go`)
**Performance Goals**: Unchanged. The added work is one indexed primary-key lookup on subnet creation
and one on domain resolution; both are on paths that already perform several queries.
**Constraints**: OpenStack Flamingo (2025.2) API compatibility is non-negotiable; synchronous
operations only; fail-fast; must work in stub mode.
**Scale/Scope**: 5 production files, ~4 test files, 1 bash suite, 1 doc line. 29 FRs, 13 SCs.

## Constitution Check

*GATE: evaluated before Phase 0 and re-evaluated after Phase 1.*

| Article | Verdict | Reasoning |
|---|---|---|
| **I. Flamingo API compatibility** (NON-NEGOTIABLE) | ✅ **PASS** — improves it | Every change moves toward upstream: `default` is upstream Keystone's literal Default-domain id; `admin_or_network_owner` is upstream's `create_subnet` policy; the project-in-path form is upstream Cinder's canonical URL; `400` on a malformed body attribute is upstream Neutron's answer. The contract suite (`test/contract/keystone/`) guards the one place where compatibility could regress — responses keep returning the stored UUID (A2, FR-007). |
| **II. Synchronous operations** | ✅ **PASS** | No queue, no worker, no state machine. Two added lookups run inline in the request. |
| **III. Fail-fast** | ✅ **PASS** | No retries, no backoff. Both added lookups are single queries that fail immediately; the subnet check runs *before* any side effect, which makes the failure earlier than today, not later. |
| **IV. Multi-mode** | ✅ **PASS** | All four fixes are mode-independent — they live above the hypervisor/netlink/storage boundary. Stub mode demonstrates every one (A8). `applyDHCPForNetwork` already returns early in stub mode (`reconciler.go:232`), so ordering the entitlement check before it changes nothing in stub and matters only in real mode. |
| **V. Project isolation** | ✅ **PASS** — this is the article being enforced | B8 is a direct violation of *"no cross-project resource access without explicit admin role check"*. FR-010/FR-012 install exactly that check. FR-011b adds the observability the article's spirit implies. |
| **VI. Single binary** | ✅ **PASS** | No new process, port, or service. |
| **Forbidden patterns** | ✅ **PASS** | No queue, worker, state machine, or service discovery introduced. |
| **Test-First workflow** | ✅ **PASS** | Contract/unit tests written first per defect, RED confirmed, then implement (FR-021 makes failing-before/passing-after a requirement, not a courtesy). |
| **Backwards compatibility** | ⚠️ **Intentional behaviour change, justified** | Refusing a cross-tenant subnet write breaks any caller relying on it (A4). Recorded in Complexity Tracking below. No microversion bump: the operation is not gaining or losing a documented capability — it is starting to enforce the isolation the API already claims. |
| **Reversible migrations** | ✅ **N/A** | No migration. |
| **Security requirements** | ✅ **PASS** | No secret, credential, or token handling changes. |

**Post-Phase-1 re-evaluation**: unchanged — all gates still PASS. Phase 1 introduced no new package,
no new dependency, no abstraction layer, and no async anything. The one design decision that could
have added complexity (a generic path-parameter alias framework) was rejected in favour of a single
resolver function; see research.md R1.

### Simplicity notes (Article "Simplicity over performance", governance §2)

- The domain resolver is **one function**, not a type or an interface. It mirrors
  `common.ResolveProjectRef` in shape and lives next to the handlers that use it.
- No abstraction is introduced for the subnet entitlement check: it is an inline query plus two
  branches, matching `CreatePort` line-for-line except for the rule.
- The four Cinder routes are four lines. No route-table generator, no parity-enforcement framework —
  decision Q2 explicitly rejected the full-parity option that would have needed one.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|--------------------------------------|
| **Backwards-incompatible behaviour change**: a cross-tenant subnet write that succeeds today will be refused | It is the only security defect in #536 — a tenant chooses the address range, gateway and DHCP behaviour on another tenant's network, and the victim's own views then present it as theirs | Leaving it, or gating it behind a config flag, keeps a cross-tenant write reachable by default. A flag would also need a documented "insecure" mode, which is more surface than the fix. Article V is unconditional: *no cross-project access without an explicit admin check* |
| **Narrow exception to the #515 rule** that a non-UUID path id gets a verbatim 404 before the handler runs — the domain-retrieval route moves off the validating group | Standard clients resolve `--domain` client-side by retrieving the domain first; without this, the alias works only via a client fallback that a future client version may drop (FR-001a, A11) | Adding an alias allowlist *inside* `uuidParamMiddleware` was rejected: it puts resource-specific knowledge into a generic middleware. Moving the one route to the `mgmt` group and validating in-handler is the pattern #515 itself already uses for its authz-gated routes (`handlers.go:60-66`), so this is the established shape, not a new one |

## Project Structure

### Documentation (this feature)

```text
docs/specs/022-project-tenant-isolation/
├── spec.md                      # Feature specification (29 FR, 13 SC)
├── plan.md                      # This file
├── research.md                  # Phase 0 — 9 decisions, all grounded in existing code
├── data-model.md                # Phase 1 — entities, constraints, no schema change
├── quickstart.md                # Phase 1 — reproduce all four defects, then verify all four fixes
├── contracts/
│   ├── keystone-domain-resolution.md   # B5/P1 — project/user/group create + domain read
│   ├── neutron-subnet-create.md        # B8 — entitlement matrix and refusal codes
│   ├── cinder-project-scoped-routes.md # B2 — the four routes and the shadowing rule
│   └── test-harness.md                 # T1 — setup guards, teardown, the two new checks
├── checklists/
│   └── requirements.md          # Spec-quality checklist (5 validation iterations)
├── tasks.md                     # Phase 2 output — 62 tasks
└── EVIDENCE.md                  # Created in W0 — the RED/GREEN record that FR-021 and
                                 # SC-007 are checked against, one section per defect
```

### Source Code (repository root)

```text
internal/
├── keystone/
│   ├── domain_resolve.go        # NEW  — resolveDomainRef: the single shared resolver (FR-008)
│   ├── handlers.go              # EDIT — CreateProject (alias + 400 + 409), CreateUser (alias),
│   │                            #        route group move for GET /domains/:id
│   ├── groups.go                # EDIT — CreateGroup (alias)
│   ├── domains.go               # EDIT — GetDomain (alias), ListDomains (?name= filter)
│   ├── bootstrap.go             # UNCHANGED — defaultDomainID stays the seed-time constant
│   ├── domain_resolve_test.go   # NEW  — resolver unit tests
│   └── identity_domain_alias_test.go # NEW — handler-level: alias/UUID/omitted/unknown/disabled/dup
├── neutron/
│   ├── network.go               # EDIT — CreateSubnet: entitlement check before INSERT (FR-009..013)
│   └── subnet_ownership_test.go # NEW  — the entitlement matrix incl. the warn record
└── cinder/
    ├── volumes.go               # EDIT — 4 routes into the /v3/:project_id group + comment update
    └── quotas_test.go           # EDIT — extend TestRouteRegistration_NoWildcardConflict
                                 #        into a no-shadow guard over the project-scoped
                                 #        group. NOT a parity guard — full parity is
                                 #        decision Q2's rejected option C

test/
└── multi_tenant_test.sh         # EDIT — setup guards, trap teardown, cross-tenant subnet write
                                 #        check, project-scoped volume list check

docs/
└── blueprint.md                 # EDIT — line 75 claims the two volume URL forms are "the same
                                 #        operation"; true only after this feature
```

**Structure Decision**: No new package and no new directory. The repository is organised by
OpenStack service under `internal/`, and each of the four defects lives inside exactly one of those
service packages, so the change follows the existing boundary rather than crossing it. The one new
production file (`internal/keystone/domain_resolve.go`) exists because FR-008 requires the resolution
to be a single shared behaviour used by four call sites in three files — putting it in any one of
those files would make the other three import from an arbitrary home. It is *not* placed in
`internal/common` (where `ResolveProjectRef` lives) because domains are Keystone's alone: no other
service resolves a domain reference, and `internal/common` is for what several services share.

## Phase 0 — Research

Complete. See [research.md](./research.md). Nine decisions, every one anchored to code already in the
tree; **zero NEEDS CLARIFICATION remain**. Headlines:

- **R1** — resolver as one function in `internal/keystone`, modelled on `common.ResolveProjectRef`;
  a generic alias framework rejected.
- **R2** — resolve the default domain **by name** (`WHERE name = 'Default' AND enabled = true`), not
  by the hard-coded UUID. Migrations `009` and `037` seed the same domain name with *different* UUIDs
  (`…0100` and `…0001`); `009` wins by ordering plus `ON CONFLICT (name) DO NOTHING`, so a
  name-based lookup makes the discrepancy harmless instead of load-bearing.
- **R3** — duplicate name detected via SQLSTATE `23505`, shared as `common.IsUniqueViolation`
  (`internal/nova/flavors.go:62`), not by string-matching the error text as `CreateUser` does.
- **R4** — the middleware exception is a **route-group move**, not a middleware change.
- **R5** — subnet entitlement is one query returning the owner and the shared flag, so a single round
  trip decides 404 / 403 / allowed.
- **R6** — only **three** project-scoped sub-resource names are shadowed; `backups/detail` is absent,
  not shadowed. This corrected the spec.
- **R7** — O3K's existing error envelope is kept for the new `403`; upstream Neutron's `NeutronError`
  wrapper is not adopted here because no O3K Neutron error uses it today.
- **R8** — trap-based teardown, with the guard helper failing on empty *or* literal `null`.
- **R9** — no migration; migration `037`'s dead second seed is left alone.

## Phase 1 — Design & Contracts

Complete. Artifacts:

- **[data-model.md](./data-model.md)** — the five entities the feature touches, their existing columns,
  and the constraints each requirement leans on. Confirms no column is added, altered, or dropped.
- **[contracts/keystone-domain-resolution.md](./contracts/keystone-domain-resolution.md)** — the
  request/response contract for project, user and group creation and for domain retrieval and listing,
  including every status code and message.
- **[contracts/neutron-subnet-create.md](./contracts/neutron-subnet-create.md)** — the full
  entitlement matrix (caller × network ownership × shared × role → outcome), the ordering guarantee
  relative to side effects, and the audit record's fields.
- **[contracts/cinder-project-scoped-routes.md](./contracts/cinder-project-scoped-routes.md)** — the
  four routes to add, the shadowing analysis with evidence, and the routes recorded
  as a deliberate gap.
- **[contracts/test-harness.md](./contracts/test-harness.md)** — the guard and teardown contract for
  `multi_tenant_test.sh` and the two new checks.
- **[quickstart.md](./quickstart.md)** — reproduce each defect (RED), then verify each fix (GREEN),
  including the static one-liners from the ticket.

Agent context refreshed via `.specify/scripts/bash/update-agent-context.sh claude` with
`SPECIFY_FEATURE=022-project-tenant-isolation`.

## Implementation Sequence

The ticket's dependency chain is real and the plan preserves it. Story 3 (Cinder routes) is
independent and may land at any point.

| Wave | Work | Why here | Gate to pass before moving on |
|---|---|---|---|
| **W0** | Stack up, test database, baseline gates, and **capture the failing-before evidence for all four defects** into `EVIDENCE.md` | Blocking and **irreversible**: once W1 lands, B5's `500` can never be reproduced again, and FR-021 requires the pair. Also records that `test/horizon_compat_test.sh` fails two checks on B2 today | Every defect has a recorded RED state; baseline gate results noted |
| **W1** | B5/P1 — resolver, `CreateProject`, `CreateUser`, `CreateGroup`, `GetDomain`, `ListDomains` | Nothing else is demonstrable until a project can be created; `make test-multi-tenant` cannot reach its first assertion | `openstack project create --domain default probe` succeeds; `openstack domain show default` succeeds; contract suite green |
| **W2** | Harness — setup guards + trap teardown (T1's token extraction is already fixed and is only **verified** here, per A7/FR-020) | Fix the harness *before* the isolation fix, so B8 lands with a suite that authenticates and cannot pass vacuously | Suite runs end to end; a deliberately broken setup exits non-zero with zero reported passes; two consecutive runs agree |
| **W3** | B8 🔒 — `CreateSubnet` entitlement check + audit record + the new suite check | Now provable by a working suite | Cross-tenant write refused with no residue; own-network and admin paths unchanged; entitlement matrix test green |
| **W4** | B2 — five Cinder routes + no-shadow guard + the new suite check | Independent of W1–W3 | `GET /v3/{project}/volumes` returns 200; the four shadowed names return their view; `test/horizon_compat_test.sh` goes from **failing to passing** on both of its volume checks |
| **W5** | Docs, host pass, full gates | Verification is a requirement (FR-021), not a follow-up | `make test`, `make lint`, `make test-integration`, `make test-multi-tenant`, the identity contract suite, `test/horizon_compat_test.sh`, host pass on the stub stack |

## Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| The route-group move for `GET /domains/:id` weakens #515's guarantee more broadly than intended | Low | The move is one route and one method; `PATCH`/`DELETE /domains/:id` and `/domains/:id/config` stay on the validating group. `TestRegisterRoutesMiddlewareWiring` (`routes_wiring_test.go:19`) already asserts the val/mgmt split and gains a case for the moved route |
| Retiring the hard-coded default-domain UUID changes which domain new resources land in | Very low | Both seeds carry the name `Default`; `009` wins today, and name-based resolution returns exactly the row that is winning now. The contract suite asserts `…0100` in five places and will catch any drift |
| The `403`-vs-`404` split leaks the existence of shared networks | Accepted by design | A shared network is already visible to the caller through the network list, so `403` discloses nothing new. This was decision Q1, option C rejected for exactly this reason |
| `CreateSubnet` and `CreatePort` decide differently and someone later "unifies" them | Medium | FR-010/A5 require the divergence be stated in the code at the point of decision, and the entitlement matrix test fails if the shared case starts succeeding |
| Adding routes to the project-scoped group collides with an existing wildcard and panics at startup | Low | gin splices a static child ahead of an existing wildcard regardless of registration order (already verified against the vendored gin v1.12.0 and documented at `volumes.go:105-112`). `TestRouteRegistration_NoWildcardConflict` runs `RegisterRoutes` for real, so a panic fails the unit suite |

## Out of Scope (restated from the spec, so the plan cannot drift)

- The Nova owner-or-administrator sweep and domain-scoped tokens — #549.
- An administrator-only gate on project creation — recorded (FR-023/A6), decided in #549.
- The 15 project-scoped Cinder routes that are absent rather than shadowed (FR-016a).
- Domain list filters other than `name`; case-insensitive domain names.
- Returning the alias in responses (A2).
- Any real-mode behaviour, schema change, or migration.
