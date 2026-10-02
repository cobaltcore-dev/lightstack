# Implementation Plan: Inject the database pool through service constructors (ADR-001)

**Branch**: `feature/533` (feature id: `027-database-pool-injection`) | **Date**: 2026-10-01 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/027-database-pool-injection/spec.md`

## Summary

Remove the process-wide `database.DB` handle and pass the one `*pgxpool.Pool` explicitly instead. Every database-using constructor takes it as its first parameter. The 20 package-level helpers take it as a parameter. Shared helpers, the readiness check, the admin bootstrap and the `cleanup` job receive it from their caller.

The conversion is mechanical, and it changes nothing observable from outside the process. Today's gate results, recorded first, are the oracle. One new test, the Keystone demonstrator, is written first and fails to compile until Keystone takes a pool. Once the handle is deleted, the compiler guarantees that nothing else reaches for a pool. The work lands as one commit (research R10).

## Technical Context

**Language/Version**: Go 1.26 (module targets 1.21+)
**Primary Dependencies**: jackc/pgx/v5 + pgxpool (already a dependency), gin-gonic/gin. **No new modules.**
**Storage**: PostgreSQL. **No schema change, no migration.** The pool configuration from `lightstack.yaml` is unchanged.
**Testing**:
- `go test`. DB-backed unit tests are gated on `LIGHTSTACK_TEST_DB_URL`.
- The gophercloud contract suite (`test/contract`), `test/quick_test.sh` (OpenStack CLI), the Horizon UI probe (`make test-horizon-ui`), and `golangci-lint` via `go tool`.
**Target Platform**: Linux server, in stub and real mode. macOS runs stub mode only.
**Project Type**: A single-binary Go web service (six HTTP listeners in `cmd/lightstack/main.go`).
**Performance Goals**: Unchanged: same pool, same queries, same 20-connection default.
**Constraints**: No externally observable change (FR-005). No shim for the removed handle (FR-006). Concrete pool only (FR-007). One commit (FR-013). The spec and this plan cover this branch's code only.
**Scale/Scope**:
- 690 production references in 60 files, plus 184 test references in 29 files.
- 9 constructors, 20 package-level helpers (18 direct, 2 through `WithTx`), 7 `WithTx` call sites, 11 `QuotaTargetProject` call sites.
- 6 test `TestMain`s and 1 benchmark file.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle (`.specify/memory/constitution.md`) | Verdict | Why |
|---|---|---|
| I. OpenStack Flamingo API compatibility | PASS | No endpoint, status, header or body changes. The contract suite is the gate (SC-003). |
| II. Synchronous operations | PASS | Untouched |
| III. Fail-fast | PASS | Pool limits and the startup ping are unchanged, and so is the fatal exit on an unreachable database (US1 scenario 2) |
| IV. Multi-mode | PASS, with a gate | Stub mode is exercised by every suite. Real-mode-only consumers change too — the VXLAN coordinator, the node registry, the network reconciler and the cleanup purge — so a real-mode host smoke is added (research R11). |
| V. Project isolation | PASS | No query text changes; only the receiver they run on |
| VI. Single binary, shared pool | PASS | Still exactly one shared pool (FR-004). DI makes the sharing explicit rather than ambient. |
| Technology constraints and forbidden patterns | PASS | No new dependency. No queue, worker, state machine or service discovery |
| Test-first | PASS | Test strategy approved in clarify (2026-10-01). RED is the demonstrator's compile failure. The existing contract and integration suites are the behavioral guard. |
| Backwards compatibility | PASS | No API change and no migration. `internal/` packages have no external Go consumers, so `UPGRADING.md` gets no entry. |
| Security | PASS | No credential, token or authorization path changes |
| Simplicity and anti-abstraction (CLAUDE.md's Articles VII and VIII) | PASS | The concrete pool, no interface, no repository. Dead helpers (`ConnectSimple`, `Stats`) are deleted, not adapted. |
| Quality gates before merge | PASS by plan | CLI (`quick_test.sh`), Horizon (probe), contract tests, both modes (stub suites plus the real-mode smoke), migrations (n/a), secrets (none added) |

There are no violations, so Complexity Tracking is empty.

## Project Structure

### Documentation (this feature)

```text
docs/specs/027-database-pool-injection/
├── spec.md
├── plan.md              # this file
├── research.md          # Phase 0: R1–R13 decisions
├── data-model.md        # Phase 1: pool owners, holders, lifecycle
├── quickstart.md        # Phase 1: how to verify SC-001…SC-005
├── contracts/
│   └── go-signatures.md # Phase 1: every changed Go signature, before → after
├── checklists/
│   └── requirements.md
└── tasks.md             # Phase 2 (/cl.tasks — not created here)
```

### Source Code (repository root)

```text
cmd/lightstack/main.go            # owner: Connect → pool → constructors; defer pool.Close()
internal/database/                # db.go: Connect returns the pool; var DB, Close, Stats, ConnectSimple deleted
                                  # tx.go: WithTx(ctx, pool, fn)
internal/health/health.go         # DatabaseCheck(pool)
internal/compute/node_registry.go # NewNodeRegistry(pool, …)
internal/metadata/                # NewService(pool, bindAddr)
internal/glance/                  # NewService(pool, …)
internal/keystone/                # NewAuthService(pool, …), NewService(pool, …), BootstrapAdmin(ctx, pool, cfg),
                                  # BuildServiceCatalog(pool, …); new pool_injection_test.go (demonstrator)
internal/cinder/                  # NewService(pool, …); 2 helpers; 2 WithTx sites
internal/neutron/                 # NewService(pool, …), NewVXLANCoordinator(pool, …); reconciler via r.svc.pool;
                                  # 4 helpers; 2 WithTx sites
internal/nova/                    # NewService(pool, …); 5 helpers; 3 WithTx sites
internal/teardown/teardown.go     # Run owns its pool; enumerateDomainIDs(ctx, pool); neutron.NewService(pool, …)
internal/common/project.go        # ResolveProjectRef(ctx, pool, ref), QuotaTargetProject(c, pool)
test/benchmark/database_bench_test.go
CLAUDE.md, docs/ARCHITECTURE.md, docs/TROUBLESHOOTING.md, docs/STUB_PLACEHOLDERS.md,
docs/WHATS_LEFT.md, docs/adr/ADR-001-database-dependency-injection.md
```

**Structure Decision**: The existing single-module layout stays as it is. No package is added, moved or split. Each service package changes in place, and `internal/common` swaps its `internal/database` import for `pgxpool`.

## Implementation Steps

These come from research R10. Each step ends with `go build ./... && go vet ./...` and the touched packages' unit tests.

| Step | Scope | Exit check |
|---|---|---|
| 0 | Record the baseline gates (R11) on the unmodified tree | Results saved for comparison |
| 1 | `internal/database` API, plus every owner (`main`, `teardown` connect, six `TestMain`s, the benchmark) and `health` | Builds. Transition only: `Connect` still sets the handle, and the `WithTx` call sites pass `database.DB` |
| 2 | Keystone demonstrator test | **RED**: the keystone test package does not compile, and the only reason is that neither Keystone constructor takes a pool yet |
| 3–5 | `compute`, `metadata`, `glance` | Their tests are green |
| 6 | `keystone` (both holders, bootstrap, catalog) | Demonstrator **GREEN** |
| 7–9 | `cinder`, `neutron` (with the teardown wiring that constructs it), `nova` | Their tests are green |
| 10 | `common`: `QuotaTargetProject(c, pool)` and its 11 call sites | All three services' tests are green |
| 11 | Delete `var DB` and the transition assignment in `Connect` | The compiler reports nothing, and the acceptance search is empty |
| 12 | Docs (R13), then the full gate set | Identical to the baseline (SC-003) |

A conversion script, if one is used, stays outside the repository (spec clarification 2026-10-01).

## Verification

See [quickstart.md](quickstart.md):

- the acceptance search;
- unit tests with the race detector, run both with and without a test database, plus lint;
- the demonstrator run on its own;
- the contract counts against the baseline, plus `quick_test.sh` and the Horizon probe;
- the fail-fast, readiness and shutdown checks;
- the real-mode host smoke ([`HOST_TESTING_REAL_MODE.md`](HOST_TESTING_REAL_MODE.md); the stub pass is [`HOST_TESTING.md`](HOST_TESTING.md));
- the docs search.

## Constitution Re-check (post-design)

The re-check passes, with no change from the pre-design verdicts. The design adds no abstraction, dependency or behavior. Every new signature is in [contracts/go-signatures.md](contracts/go-signatures.md). Principle IV's gate is satisfied by the real-mode smoke in quickstart step 4.

## Complexity Tracking

> No constitution violations — section intentionally empty.
