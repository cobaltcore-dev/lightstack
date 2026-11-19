# Implementation Plan: OpenStack CLI Conformance Test Environment

**Branch**: `feature/105` (feature dir `031-cli-conformance-test-environment`) | **Date**: 2026-10-15
**Spec input**: GitHub issue **#105 — "[Enhancement] Testing"** ("we need proper testing for the existing
Openstack API calls and terraform use cases")
**Goal**: Be sure the **working binary** (the artifact `make build` produces) works against the
**python OpenStack CLI** (`openstack` command) **in all cases** — every CLI-exposed operation for all
five services, happy path and negative path, with a machine-auditable coverage matrix — and that this
is enforced in CI, not run by hand.

---

## 1. Analysis — current state (why #105 is real)

### 1.1 Pre-implementation baseline (2026-10-15)

| Layer | What | Where | Runs in CI? |
|---|---|---|---|
| Unit tests | `go test -race`, DB-backed via `LIGHTSTACK_TEST_DB_URL` | `internal/**` | ✅ `unit-tests` job |
| Contract tests | gophercloud + testify, live server, no mocks | `test/contract/**` | ⚠️ soft gate: `continue-on-error: true`, 85% pass-rate floor in `contract-tests` |
| Integration scripts | 75 bash scripts, expect an already-running server | `test/*.sh` | ⚠️ only `quick_test.sh` (curl-only) + `horizon_create_ui_test.sh` (curl-only) |
| CLI-driven scripts | ~24 scripts that do run `openstack …` | `test/validation_suite.sh`, `horizon_full_test.sh`, nova/cinder/glance waves, … | ❌ **none** (except `rgw_integration_test.sh` in the RGW job, Glance-only) |
| Terraform modules | unmodified provider apply/destroy (compat) + real-mode deep scenario (sc007) | `test/terraform/` | ❌ manual only; ~13 provider resources never exercised (`docs/testing/BLUEPRINT_TEST_COVERAGE.md`) |
| Helm | lint/render/helm-unittest | `make chart-test` | ✅ |

### 1.2 Gaps identified before implementation

1. **No systematic CLI conformance suite.** The `openstack` CLI (python-openstackclient on
   python-openstacksdk) is a *different client* from gophercloud: different request shapes,
   microversion headers, name→ID resolution, and `-f json` output parsing. Nothing in the repo
   systematically drives it. The best CLI suites (`validation_suite.sh` ~40 ops,
   `horizon_full_test.sh` ~30 ops) are ad-hoc, unrunner'd, and happy-path-heavy — far short of "all
   cases" for 342 endpoints.
2. **CI had no systematic CLI conformance suite.** The `integration-tests` job runs curl-based
   checks; CLI behavior needed a separate, pinned suite.
3. **CI never tests the built binary.** The `build` job uploads `bin/lightstack` as artifact
   `lightstack-binary` — nothing consumes it. The contract/integration jobs bring up
   `deployments/docker-compose.yml`, which **rebuilds the binary inside Docker**. So the thing we
   ship is not the thing we test.
4. **`test/run_contract_tests.sh` did not cover every package**: it omitted Nova and the root
   `test/contract` package.
5. **~65 of 75 `test/*.sh` scripts have no runner at all.** `make test-all` covers only 5 live-server
   scripts. Results are bash counters, exit codes only — no structured (JUnit/JSON) reporting.
6. **No test image is seeded.** Nothing in migrations/compose seeds a `cirros` image; CLI and
   Terraform tests that boot servers need one and currently rely on it being preloaded by hand.
7. **Coverage bookkeeping is inconsistent.** Four status docs disagree on totals (157/191, 223/233,
   258/286, 375-endpoint basis); `BLUEPRINT_TEST_COVERAGE.md` counts **25 endpoints with zero
   coverage**; 19 Cinder contract tests are known-failing with no machine-readable list.
8. **Terraform conformance is manual** (issue #105 explicitly names Terraform use cases).

### 1.3 Assets we can build on (the change is shallow where it counts)

- The stack bootstrap is already solved: `deployments/docker-compose.yml` (postgres + lightstack,
  healthcheck, `LIGHTSTACK_ENDPOINT_HOST=localhost`), migrations + admin bootstrap run on startup.
- A CLI-shaped test vocabulary already exists in ~24 scripts — commands, cleanup patterns, wait
  loops. The harness extracts these into a shared library instead of re-inventing them.
- `test/terraform/compat` already applies the **unmodified** upstream provider — moving it into CI
  is wiring, not new design.
- CI already supports artifact upload/download and isolated service jobs.

---

## 2. Goals / Non-Goals

### Goals

1. **G1 — Test the shipped binary.** CI gates run against the `lightstack-binary` artifact produced
   by the `build` job (plus a Postgres service), not a second Docker-rebuilt binary.
2. **G2 — CLI conformance suite ("all cases").** A maintained, machine-readable matrix of
   `openstack` CLI commands × expected outcomes covering every CLI-exposed operation of the five
   services; happy path, output-shape (`-f json` field presence/type), and negative paths (404/400/
   409 semantics). Suite runs against the artifact binary in stub mode.
3. **G3 — CI enforcement.** New `cli-conformance` job, **hard gate** (no `continue-on-error`), fast
   enough for PRs (<10 min); full matrix runs nightly.
4. **G4 — Terraform in CI.** The existing `compat` module runs nightly against the artifact binary,
   with an apply/destroy wrapper and evidence that tracked resources were cleaned up. Adding
   further provider resources is follow-up work, with each resource family handled in its own batch.
5. **G5 — Auditable results.** JUnit/JSON output, a generated coverage report (command → endpoint →
   test status), one source of truth replacing the four disagreeing status docs.
6. **G6 — Fix the broken runner** (`run_contract_tests.sh`) and seed a test image so every suite can
   boot servers.

### Non-Goals

- Real-mode/libvirt/KVM testing (existing wave scripts and k3s verification remain the mechanism).
- Horizon browser automation (covered by the Horizon compatibility program, SPEC-018/019/020).
- Making the 19 known-failing Cinder contract tests pass (tracked separately; the new suite must not
  inherit the soft-gate habit, but fixing Cinder gaps is product work, not this feature).
- SDK conformance beyond the CLI and Terraform (gophercloud contract tests already exist).
- Rewriting the 75 existing bash scripts (they stay; the harness supersedes the ad-hoc CLI ones).

---

## 3. Technical approach

### 3.1 Architecture of the CLI conformance harness

```
test/cli/
├── bin/run.sh                  # entrypoint: preflight, env, fixture setup, suite exec, report
├── lib/
│   ├── common.sh               # OS_* env handling, `osc` wrapper (timeout + -f json), PASS/FAIL counters
│   ├── fixtures.sh             # idempotent setup: test project+user, image, flavor, network; teardown
│   ├── assert.sh               # jq-based field/type asserts on CLI JSON output
│   └── report.sh               # JSON + JUnit emission, per-command result records
├── manifest/
│   └── <service>.json           # THE matrix (one file per service, jq-native):
│                                #   service → commands → endpoints → tiers
├── suites/
│   ├── keystone.sh             # Tier A: identity commands
│   ├── compute.sh              # Tier A: server/flavor/keypair/image-action commands
│   ├── network.sh              # Tier A: network/subnet/port/router/secgroup/fip/qos
│   ├── volume.sh               # Tier A: volume/snapshot/backup/quotas
│   ├── image.sh                # Tier A: image commands
│   └── workflows.sh            # Tier C: cross-service scenarios (boot with volume+net, snapshot→boot, …)
└── README.md                   # how to run, how to add a case, tier definitions
```

Design rules:

- **The manifest is the definition of "all cases".** `manifest/*.json` enumerates every
  `openstack` command lightstack claims to support (intersect `openstack --help` output of the
  pinned CLI version with the 342-endpoint inventory). Each entry carries: command template,
  endpoint inventory, and one or more tiers; status is recorded in the results files.
  coverage.sh fails on missing required cases, failures, skips, and unbaselined known failures.
  This makes the coverage matrix auditable and keeps results separate from command inventory.
- **Tiers** (mirroring `test/contract/README.md`'s tier model):
  - **A — happy path + shape**: run command, exit 0, required JSON fields present and typed.
  - **B — negative**: assert the expected CLI refusal and error class. Some cases stop at server-backed resource lookup; document the stage exercised.
  - **C — workflows**: multi-command, cross-service scenarios with state assertions.
- **`osc` wrapper**: every CLI call goes through one function that enforces `--os-*` env, a
  per-command timeout (default 30s), `-f json`, captures stderr, and records the
  result — no naked `openstack` calls in suites.
- **Fixtures, not luck**: fixtures.sh creates a project, user, flavor, network, and a 64 KiB raw image at runtime;
  It also uses seeded flavors (m1.*) from migration 002.
- **Idempotent teardown**: every suite cleans what it creates; runner supports `--no-cleanup` for
  debugging.
- **Pinning**: `python-openstackclient` is installed at a pinned version from a constraints file
  (`test/cli/constraints.txt`), so a CLI release can't flip CI red overnight; upgrades are a
  deliberate PR.

### 3.2 Testing the shipped binary (G1)

Local compose bootstrap, deployments/docker-compose.binary-test.yml:

- `postgres:18-alpine` service (unchanged credentials scheme).
- lightstack container reuses the Alpine runtime stage and **bind-mounts `bin/lightstack`, this
  checkout's migrations, and its config**. `make build` uses `CGO_ENABLED=0` so the binary runs on
  Alpine/musl.
- Same env contract as the existing compose (`LIGHTSTACK_ENDPOINT_HOST=localhost`, stub mode,
  `LIGHTSTACK_DB_URL`, JWT secret), same ports.

CI flow: the build job uploads bin/lightstack; cli-conformance downloads that artifact and runs it
natively against PostgreSQL before running the CLI suite. Compose is the local fallback.

Local parity: make test-cli uses the binary-test Compose stack and runner. It builds the binary
only when absent; run make build first to refresh an existing binary. The runner rebuilds and recreates the stack.

### 3.3 CI wiring

- **cli-conformance job (PR + push, hard gate)**: the build artifact runs natively against PostgreSQL.
  The pinned CLI runs the fast Tier A+B subset, and the required coverage gate checks results.

- **nightly.yml**: a suite matrix runs the full CLI manifest or Terraform wrapper against a native Lightstack server.
  It uploads each suite’s JSON/JUnit results; contract tests remain in CI.
- **Completed:** removed the unused CLI installation from the SQLite contract/integration job and the obsolete e2e TODO.

### 3.4 Terraform conformance (G4)

- Nightly job: native Lightstack server, pinned Terraform CLI, and terraform-provider-openstack;
  `test/terraform/compat` runs through `make test-terraform`. The wrapper checks its declared
  Terraform version, runs `init → validate → apply → re-plan → destroy`, and verifies that
  Terraform state and named Lightstack resources are clean.
- This PR exercises the compatibility module's existing resource set. Adding the currently
  unexercised resources listed in `BLUEPRINT_TEST_COVERAGE.md`—such as floating IPs, routers and
  interfaces, ports, snapshots and backups, server groups, and image members—is follow-up work.
  Add those resource families in separate batches with their own setup, cleanup, and evidence.
- sc007 (real-mode SSH-verified scenario) stays manual/host-based — out of scope per Non-Goals.

---

## 4. Implementation phases

### Phase 0 — Fix the broken foundation (small, unblocks everything)

| # | Task | Files |
|---|---|---|
| 0.1 | Fix `run_contract_tests.sh`: use the script-relative module, include the root package, count JSON test events | `test/run_contract_tests.sh` |
| 0.2 | Seed a test image: commit a tiny qcow2 (or generate at test time via a fixture) + add to `fixtures.sh`; document that CI no longer depends on a hand-preloaded `cirros` | `test/cli/lib/fixtures.sh` (new), `deployments/` |
| 0.3 | Fix stale paths/references in `test/contract/README.md` (dead `contract-tests.yml`, `/Users/…`) | `test/contract/README.md` |
| 0.4 | Consolidate test-status docs: mark the three older reports superseded by the manifest-generated report | `docs/testing/*` |

### Phase 1 — CLI harness core (the feature)

| # | Task |
|---|---|
| 1.1 | `test/cli/lib/` — `common.sh` (`osc` wrapper), `assert.sh` (jq), `report.sh` (JSON+JUnit) |
| 1.2 | Per-service JSON manifests with unique command IDs, endpoints, tiers, and coverage gate validation |
| 1.3 | `fixtures.sh` — project/user/image/network setup + teardown, idempotent |
| 1.4 | Tier A and B cases in the five service suites; required manifest entries have matching results |
| 1.5 | Tier B cases live in each service suite and assert expected 404/400/409 refusal classes |
| 1.6 | Tier C `workflows.sh` — cross-service scenarios (boot with volume+network; snapshot→new server; secgroup lifecycle; floating-IP associate/disassociate) |
| 1.7 | `make test-cli`, `make cli-coverage` Makefile targets; `test/cli/README.md` |

**TDD note (Constitution Art. III):** cases record the requests and outcomes they actually exercise.
Product defects are fixed or recorded as known-failing only when the pinned environment makes a
case structurally unpassable. Each known-failing case must appear in the checked-in baseline with
a reason. coverage.sh and run.sh --strict reject new known failures; baseline entries that pass
produce a stale-entry warning. When a product divergence is fixed, remove its baseline entry and
turn a self-healing probe into a plain assertion.

### Phase 2 — CI integration

| # | Task |
|---|---|
| 2.1 | `deployments/docker-compose.binary-test.yml` (artifact-mounting stack) + runtime-image Dockerfile target |
| 2.2 | cli-conformance downloads the build artifact, runs the native server and pinned CLI fast subset, enforces coverage, and uploads results |
| 2.3 | `nightly.yml`: full manifest and Terraform compat; upload JSON/JUnit results |
| 2.4 | Completed: remove the unused CLI install from the SQLite contract/integration job and the obsolete e2e TODO |
| 2.5 | `status-check` job: add `cli-conformance` to the required-needs list |

### Phase 3 — Terraform conformance

| # | Task |
|---|---|
| 3.1 | `make test-terraform` wrapper: enforce the Terraform pin and run the existing compatibility module through init, validate, apply, re-plan, and destroy |
| 3.2 | Nightly conformance matrix runs the Terraform wrapper against the native server and archives results |
| 3.3 | Check empty Terraform state and named OpenStack resource cleanup after destroy |

### Phase 4 — Follow-up coverage closure & hardening (outside PR 107)

| # | Task |
|---|---|
| 4.1 | Close the 25 zero-coverage endpoints from `BLUEPRINT_TEST_COVERAGE.md` with CLI or contract cases (they are itemized there) |
| 4.2 | Extend `test/terraform/compat` to additional provider resource families in separate batches, with cleanup assertions |
| 4.3 | Migrate the strongest ad-hoc scripts' unique scenarios into Tier C (then retire the duplicates) |
| 4.4 | CLI-version upgrade policy documented; scheduled monthly bump PR of `constraints.txt` |

---

## 5. Success criteria (definition of done)

1. `make build && make test-cli` runs the shipped `bin/lightstack` against the pinned OpenStack CLI
   and passes with **zero `untested` Tier A/B manifest entries**.
2. A PR that breaks any CLI-visible behavior fails `cli-conformance` in CI — hard gate, no
   threshold math.
3. `make cli-coverage` prints a per-service matrix (commands × status) and matches the artifact
   published by nightly CI.
4. Nightly runs the full CLI matrix and the existing `test/terraform/compat` apply/destroy workflow green.
5. The CLI conformance job consumes the binary artifact produced by the build job.
6. Issue #105's two asks are answered: OpenStack API calls (CLI conformance, G2/G3) and Terraform
   use cases (G4).

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| CLI invocations are slow (~1-2s each; full matrix could be 600+ calls) | Curated PR subset; full nightly matrix |
| New CLI release changes output/behavior | Pinned `constraints.txt`; deliberate upgrade PRs |
| Cases uncover real product bugs (likely — Cinder metadata/transfers already known weak) | Every `known-failing` case is listed in the checked-in baseline with a reason; both gates fail on a new one, a stale entry only warns, and the count is visible — not a silent 85% floor |
| Alpine runtime image differs from developers' environments | Same image already used in prod compose; local `make test-cli` uses the identical stack |
| bash harness complexity grows | `report.sh` + strict `common.sh` from day one; revisit a Go/python runner only if suites exceed bash's comfort (~Tier C growth) |

## 7. Estimate

| Phase | Size |
|---|---|
| 0 — foundation fixes | ~1 day |
| 1 — CLI harness + full manifest | ~4-5 days (largest item; mostly writing cases) |
| 2 — CI wiring | ~1-2 days |
| 3 — Terraform in CI | ~1 day |
| 4 — coverage closure | ongoing, batched |

## 8. Open decisions (defaults chosen, flag now to override)

1. **Bash vs Go for the harness** — default bash (Constitution Art. VIII anti-abstraction; builds on
   the existing script vocabulary; strategy doc Option A). A Go runner would give parallelism and
   typed asserts but adds a framework; revisit at Phase 4.
2. **The working binary in CI** — the build-job artifact downloaded by cli-conformance and run natively (G1).
   The local Compose fallback separately bind-mounts that checkout binary.

3. **PR-time scope** — run the five service suites and gate every required Tier A/B manifest case
   with `run.sh --fast`. Tier C workflows and Terraform run nightly; provider-resource expansion is
   tracked as follow-up work.
4. **Known-failing policy** — default: `known-failing` cases are explicit in the suite and must be
   listed in `test/cli/baseline/known_failures.txt` with a reason; new ones fail the gate, stale ones
   warn, no pass-rate floor. Alternative: an 85%-style threshold — rejected, it is how the Cinder
   debt went invisible.

## 9. Migration sequence

- Main uses migrations 074–075. This feature adds migrations 076–082 in order; each version has
  backend-specific files where the PostgreSQL and SQLite schemas differ.
