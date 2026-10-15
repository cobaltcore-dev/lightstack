# Implementation Plan: OpenStack CLI Conformance Test Environment

**Branch**: `feature/105` (feature dir `028-cli-conformance-test-environment`) | **Date**: 2026-10-15
**Spec input**: GitHub issue **#105 — "[Enhancement] Testing"** ("we need proper testing for the existing
Openstack API calls and terraform use cases")
**Goal**: Be sure the **working binary** (the artifact `make build` produces) works against the
**python OpenStack CLI** (`openstack` command) **in all cases** — every CLI-exposed operation for all
five services, happy path and negative path, with a machine-auditable coverage matrix — and that this
is enforced in CI, not run by hand.

---

## 1. Analysis — current state (why #105 is real)

### 1.1 What exists

| Layer | What | Where | Runs in CI? |
|---|---|---|---|
| Unit tests | `go test -race`, DB-backed via `LIGHTSTACK_TEST_DB_URL` | `internal/**` | ✅ `unit-tests` job |
| Contract tests | gophercloud + testify, live server, no mocks | `test/contract/**` (336 `Test*`, 14 files with `t.Skip`) | ⚠️ soft gate: `continue-on-error: true`, ≥85% pass-rate floor (`ci.yml:156,201`) |
| Integration scripts | 75 bash scripts, expect an already-running server | `test/*.sh` | ⚠️ only `quick_test.sh` (curl-only) + `horizon_create_ui_test.sh` (curl-only) |
| CLI-driven scripts | ~24 scripts that do run `openstack …` | `test/validation_suite.sh`, `horizon_full_test.sh`, nova/cinder/glance waves, … | ❌ **none** (except `rgw_integration_test.sh` in the RGW job, Glance-only) |
| Terraform modules | unmodified provider apply/destroy (compat) + real-mode deep scenario (sc007) | `test/terraform/` | ❌ manual only; ~13 provider resources never exercised (`docs/testing/BLUEPRINT_TEST_COVERAGE.md`) |
| Helm | lint/render/helm-unittest | `make chart-test` | ✅ |

### 1.2 The gaps that block the goal

1. **No systematic CLI conformance suite.** The `openstack` CLI (python-openstackclient on
   python-openstacksdk) is a *different client* from gophercloud: different request shapes,
   microversion headers, name→ID resolution, and `-f json` output parsing. Nothing in the repo
   systematically drives it. The best CLI suites (`validation_suite.sh` ~40 ops,
   `horizon_full_test.sh` ~30 ops) are ad-hoc, unrunner'd, and happy-path-heavy — far short of "all
   cases" for 342 endpoints.
2. **CI installs the CLI and then never uses it.** The `integration-tests` job
   (`ci.yml:235-237`) runs `pip install python-openstackclient` and then executes `quick_test.sh`,
   which is curl-only. The install is dead weight; `openstack server list` is never validated in CI.
3. **CI never tests the built binary.** The `build` job uploads `bin/lightstack` as artifact
   `lightstack-binary` — nothing consumes it. The contract/integration jobs bring up
   `deployments/docker-compose.yml`, which **rebuilds the binary inside Docker**. So the thing we
   ship is not the thing we test.
4. **`test/run_contract_tests.sh` is broken off the original author's machine**: hardcoded
   `/Users/I761222/git/lightstack` path and `nova` missing from its service loop.
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
- CI already knows how to install the CLI (`pip install python-openstackclient`) and upload/download
  artifacts.

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
4. **G4 — Terraform in CI.** The `compat` module runs nightly against the artifact binary; provider
   resource coverage grows from 4 categories toward the full set the product claims.
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
│                                #   service → commands → endpoints → tier → status
├── suites/
│   ├── keystone.sh             # Tier A: identity commands
│   ├── compute.sh              # Tier A: server/flavor/keypair/image-action commands
│   ├── network.sh              # Tier A: network/subnet/port/router/secgroup/fip/qos
│   ├── volume.sh               # Tier A: volume/snapshot/backup/quotas
│   ├── image.sh                # Tier A: image commands
│   ├── negative.sh             # Tier B: 404/400/409 semantics via CLI
│   └── workflows.sh            # Tier C: cross-service scenarios (boot with volume+net, snapshot→boot, …)
└── README.md                   # how to run, how to add a case, tier definitions
```

Design rules:

- **The manifest is the definition of "all cases".** `commands.yaml` enumerates every
  `openstack` command lightstack claims to support (intersect `openstack --help` output of the
  pinned CLI version with the 342-endpoint inventory). Each entry carries: command template,
  endpoint(s), tier, and test status. `make cli-coverage` regenerates a report from the manifest and
  fails if an entry is `untested`. This makes "all cases" auditable instead of aspirational, and
  gives the one source of truth G5 asks for.
- **Tiers** (mirroring `test/contract/README.md`'s tier model):
  - **A — happy path + shape**: run command, exit 0, required JSON fields present and typed.
  - **B — negative**: wrong ID → 404-equivalent CLI error; malformed input → 400; duplicate → 409.
  - **C — workflows**: multi-command, cross-service scenarios with state assertions.
- **`osc` wrapper**: every CLI call goes through one function that enforces `--os-*` env, a
  per-command timeout (Constitution fail-fast: 10s), `-f json`, captures stderr, and records the
  result — no naked `openstack` calls in suites.
- **Fixtures, not luck**: `fixtures.sh` creates (and tears down) a dedicated project/user, a tiny
  qcow2 test image (generated or committed, ~100KB dummy — no download in CI), and relies on seeded
  flavors (m1.*) from migration `002`.
- **Idempotent teardown**: every suite cleans what it creates; runner supports `--no-cleanup` for
  debugging.
- **Pinning**: `python-openstackclient` is installed at a pinned version from a constraints file
  (`test/cli/constraints.txt`), so a CLI release can't flip CI red overnight; upgrades are a
  deliberate PR.

### 3.2 Testing the shipped binary (G1)

New compose-lite bootstrap for CI jobs, `deployments/docker-compose.binary-test.yml`:

- `postgres:18-alpine` service (unchanged credentials scheme).
- lightstack container built `FROM alpine` (or reuse the existing runtime stage) that **bind-mounts
  `bin/lightstack` and `bin/lightstack-migrate` from the build artifact** instead of building them.
  One Dockerfile target, no second compile.
- Same env contract as the existing compose (`LIGHTSTACK_ENDPOINT_HOST=localhost`, stub mode,
  `LIGHTSTACK_DB_URL`, JWT secret), same ports.

CI flow: `build` job → artifact → `cli-conformance` job downloads artifact, `docker compose up` the
binary-test stack, readiness probe (reuse the existing 5-port curl loop), run `test/cli/bin/run.sh`.

Local parity: `make test-cli` does `make build` + the same compose file + the same runner — a
developer reproduces CI exactly.

### 3.3 CI wiring

- **`cli-conformance` job (PR + push, hard gate)**: artifact stack, pinned CLI, Tier A+B fast subset
  + Tier C core workflows. Target < 10 min (Tier A suites are parallelizable per service; the CLI's
  ~1-2s python startup is the cost driver, so the PR subset stays curated).
- **`nightly.yml` (new workflow)**: full manifest (every tier, every command), Terraform compat
  module apply/destroy, plus the contract suite via the *fixed* `run_contract_tests.sh` for
  cross-checking. Publishes JUnit + the coverage report as artifacts; opens an issue on regression
  (optional, later).
- **Fix the existing job while we're there**: in `integration-tests`, either drop the unused
  `pip install python-openstackclient` or (better) swap `quick_test.sh` for the Tier A suites once
  green, and delete the stale TODO at `ci.yml:318-319`.

### 3.4 Terraform conformance (G4)

- Nightly job: artifact stack + pinned terraform + pinned `terraform-provider-openstack`;
  `test/terraform/compat` gets a `Makefile`/script wrapper (`make test-terraform`) doing
  `init → validate → apply → destroy`, failing on any non-clean exit or non-empty final state.
- Extend `compat/` with configs for the currently unexercised resources (13 per
  `BLUEPRINT_TEST_COVERAGE.md`): floating IP, router + interface, port, volume snapshot/backup,
  security-group rule variants, server groups, image members, etc. — one TF config file per resource
  family, added incrementally.
- sc007 (real-mode SSH-verified scenario) stays manual/host-based — out of scope per Non-Goals.

---

## 4. Implementation phases

### Phase 0 — Fix the broken foundation (small, unblocks everything)

| # | Task | Files |
|---|---|---|
| 0.1 | Fix `run_contract_tests.sh`: portable path, add `nova` to the service loop, `set -euo pipefail` | `test/run_contract_tests.sh` |
| 0.2 | Seed a test image: commit a tiny qcow2 (or generate at test time via a fixture) + add to `fixtures.sh`; document that CI no longer depends on a hand-preloaded `cirros` | `test/cli/lib/fixtures.sh` (new), `deployments/` |
| 0.3 | Fix stale paths/references in `test/contract/README.md` (dead `contract-tests.yml`, `/Users/…`) | `test/contract/README.md` |
| 0.4 | Consolidate test-status docs: mark the three older reports superseded by the manifest-generated report | `docs/testing/*` |

### Phase 1 — CLI harness core (the feature)

| # | Task |
|---|---|
| 1.1 | `test/cli/lib/` — `common.sh` (`osc` wrapper), `assert.sh` (jq), `report.sh` (JSON+JUnit) |
| 1.2 | `manifest/*.json` — one file per service (keystone, compute, network, volume, image, workflows), jq-native, derived from pinned CLI `--help` ∩ endpoint inventory; every entry starts `untested` |
| 1.3 | `fixtures.sh` — project/user/image/network setup + teardown, idempotent |
| 1.4 | Tier A suites: `keystone.sh`, `compute.sh`, `network.sh`, `volume.sh`, `image.sh` — write cases until no manifest entry in these services is `untested` |
| 1.5 | Tier B `negative.sh` — 404/400/409 semantics via CLI per service |
| 1.6 | Tier C `workflows.sh` — cross-service scenarios (boot with volume+network; snapshot→new server; secgroup lifecycle; floating-IP associate/disassociate) |
| 1.7 | `make test-cli`, `make cli-coverage` Makefile targets; `test/cli/README.md` |

**TDD note (Constitution Art. III)**: for the harness itself the "red" step is running each new case
against a stub-mode server and confirming it exercises the endpoint (manifest entry flips to
`tested`). Product defects the cases uncover are filed and either fixed or recorded as
`known-failing` entries in the manifest — never silently tolerated by the gate (a `known-failing`
count is reported and trended, and blocks release at >0 new).

### Phase 2 — CI integration

| # | Task |
|---|---|
| 2.1 | `deployments/docker-compose.binary-test.yml` (artifact-mounting stack) + runtime-image Dockerfile target |
| 2.2 | `cli-conformance` job in `ci.yml`: artifact download, stack up, pinned CLI install, fast subset, **hard gate**, JUnit artifact upload |
| 2.3 | `nightly.yml`: full manifest + Terraform compat + fixed contract runner; artifacts + generated `CLI_CONFORMANCE_REPORT.md` |
| 2.4 | Clean up `integration-tests` job (drop/replace the dead pip install; resolve the `ci.yml:318` TODO) |
| 2.5 | `status-check` job: add `cli-conformance` to the required-needs list |

### Phase 3 — Terraform conformance

| # | Task |
|---|---|
| 3.1 | `make test-terraform` wrapper: pinned terraform + provider, apply/destroy with clean-state assertion |
| 3.2 | Nightly job runs it against the artifact stack |
| 3.3 | Incrementally add configs for the 13 unexercised provider resources (one PR-sized batch each) |

### Phase 4 — Coverage closure & hardening

| # | Task |
|---|---|
| 4.1 | Close the 25 zero-coverage endpoints from `BLUEPRINT_TEST_COVERAGE.md` with CLI or contract cases (they are itemized there) |
| 4.2 | Migrate the strongest ad-hoc scripts' unique scenarios into Tier C (then retire the duplicates) |
| 4.3 | CLI-version upgrade policy documented; scheduled monthly bump PR of `constraints.txt` |

---

## 5. Success criteria (definition of done)

1. `make build && make test-cli` runs the shipped `bin/lightstack` against the pinned OpenStack CLI
   and passes with **zero `untested` Tier A/B manifest entries**.
2. A PR that breaks any CLI-visible behavior fails `cli-conformance` in CI — hard gate, no
   threshold math.
3. `make cli-coverage` prints a per-service matrix (commands × status) and matches the artifact
   published by nightly CI.
4. Nightly runs the full matrix **and** `test/terraform/compat` apply/destroy green.
5. The binary artifact from `build` is what all live-server jobs consume.
6. Issue #105's two asks are answered: OpenStack API calls (CLI conformance, G2/G3) and Terraform
   use cases (G4).

## 6. Risks & mitigations

| Risk | Mitigation |
|---|---|
| CLI invocations are slow (~1-2s each; full matrix could be 600+ calls) | Tier split: curated PR subset, full matrix nightly; per-service parallel suites |
| New CLI release changes output/behavior | Pinned `constraints.txt`; deliberate upgrade PRs |
| Cases uncover real product bugs (likely — Cinder metadata/transfers already known weak) | Manifest `known-failing` state is explicit, counted, trended, and must shrink — not a silent 85% floor |
| Alpine runtime image differs from developers' environments | Same image already used in prod compose; local `make test-cli` uses the identical stack |
| bash harness complexity grows | `report.sh` + strict `common.sh` from day one; revisit a Go/python runner only if suites exceed bash's comfort (~Tier C growth) |

## 7. Estimate

| Phase | Size |
|---|---|
| 0 — foundation fixes | ~1 day |
| 1 — CLI harness + full manifest | ~4-5 days (largest item; mostly writing cases) |
| 2 — CI wiring | ~1-2 days |
| 3 — Terraform in CI | ~1 day + incremental resource batches |
| 4 — coverage closure | ongoing, batched |

## 8. Open decisions (defaults chosen, flag now to override)

1. **Bash vs Go for the harness** — default bash (Constitution Art. VIII anti-abstraction; builds on
   the existing script vocabulary; strategy doc Option A). A Go runner would give parallelism and
   typed asserts but adds a framework; revisit at Phase 4.
2. **What "the working binary" means in CI** — default: the `build`-job artifact mounted into the
   runtime image (G1). Alternative: keep testing the compose-built image and drop G1 — rejected
   because the issue's wording ("working binary") points at the artifact.
3. **PR-time scope** — default: Tier A+B + 2-3 Tier C workflows (<10 min). Alternative: everything
   nightly-only — rejected; PRs should catch CLI breakage before merge.
4. **Known-failing policy** — default: explicit manifest state with a must-shrink trend, no pass-rate
   floor. Alternative: an 85%-style threshold — rejected, it is how the Cinder debt went invisible.
