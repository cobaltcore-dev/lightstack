# Baseline gates — SPEC-031

The oracle for SC-008 ("the existing suites stay green"): the gates on the commit this branch's code
work starts from, before any SPEC-031 change, and the approval of the test strategy.

## T001 — Baseline (2026-11-06, `dd9e069`)

Go 1.27.3, Helm with helm-unittest 1.2.1, PostgreSQL 18 from `make db-up`. Unit runs exclude
`test/contract` (as `make test` does), use `-count=1` (the test cache ignores `TestMain`'s
environment) and `-p 1` (packages sharing the seeded project race in parallel).

| Gate | Command | Result |
|---|---|---|
| Build | `make build` | ok |
| Lint | `make lint` | 0 issues |
| Unit, PostgreSQL | `LIGHTSTACK_TEST_DB_URL=postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable go test -count=1 -p 1 $(go list ./... \| grep -v /test/contract)` | 19 packages ok, 0 failed |
| Unit, SQLite | the same with `LIGHTSTACK_TEST_DB_URL=sqlite:///tmp/ls-test/t.db` | 19 packages ok, 0 failed |
| Chart | `helm unittest charts/lightstack` | 24 suites, 169 tests passed |
| Contract, PostgreSQL | `make test-contract` against a stub server on a fresh `make db-up` database | 353 passed, 0 failed, 12 skipped |
| Contract, SQLite | `make test-contract` against a stub server on a new SQLite file | 353 passed, 0 failed, 12 skipped |

The contract runs mirror CI's SQLite contract job: `config/lightstack.yaml` with `state_path` set to a
temporary directory, `LIGHTSTACK_DB_URL`, `LIGHTSTACK_JWT_SECRET`, `LIGHTSTACK_ENDPOINT_HOST=localhost`
and `LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER=true`.

No failure predates this branch.

## T002 — Test strategy approved (2026-11-06)

The user approved the test strategy as written in research.md R10 and the test tasks of tasks.md, with
no changes. Host validation (T044) needs root on the openSUSE host, which the agent's sandbox does not
have: the agent generates the runbook (T043), and the user runs it.

## T041 — Gates after the change (2026-11-06)

T001's commands and environment, on the working tree (SPEC-031's changes, uncommitted, on `dd9e069`).
`make fmt` was checked without rewriting files, as `gofmt -l cmd internal pkg test tools`.

| Gate | Baseline (T001) | After |
|---|---|---|
| Format | — | no file to reformat |
| Vet | — | `go vet ./...` clean |
| Build | ok | ok |
| Lint | 0 issues | 0 issues |
| Unit, PostgreSQL | 19 packages ok, 0 failed | 19 packages ok, 0 failed |
| Unit, SQLite | 19 packages ok, 0 failed | 19 packages ok, 0 failed |
| Chart | 24 suites, 169 tests passed | 25 suites, 181 tests passed |
| Contract, PostgreSQL | 353 passed, 0 failed, 12 skipped | 353 passed, 0 failed, 13 skipped |
| Contract, SQLite | 353 passed, 0 failed, 12 skipped | 353 passed, 0 failed, 13 skipped |
| No-hypervisor contract case | RED: the server went `ACTIVE` | PASS: `ERROR` with a fault |

- The chart's 12 new tests: the new `dns_policy_test.yaml` suite (7), three `config_render_test.yaml`
  cases and two `schema_test.yaml` cases.
- The 13th contract skip is the new gated `TestNoHypervisorCreateEndsErrorWithFault`, which skips against
  a stub server by design.
- The no-hypervisor run (quickstart §2): a real-mode server whose `libvirt_uri` names a socket that does
  not exist, on SQLite in a temporary directory. `/readyz` answered `503 hypervisor unavailable`; the gated
  case passed with `LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1`.
- After these runs, T042 changed one test in `pkg/hypervisor` (below). That package was re-run with
  `-race` (ok) and `make lint` repeated (0 issues).

No gate regressed.

## T042 — Mutation check (2026-11-06)

Each fix was reverted in a `git clone --shared` scratch clone carrying the worktree's changes. The
worktree was never edited. First, the unmutated clone passed every test command used below. After each
mutation the file was restored, and at the end the clone matched the worktree.

| # | Fix reverted | Caught by |
|---|---|---|
| M1a | `dnsPolicy` removed from the pod spec (T007) | `dns_policy_test.yaml`: the Deployment and DaemonSet cases |
| M1b | `dnsPolicy` removed from the purge Job (T007) | `dns_policy_test.yaml`: the purge Job case |
| M2 | the socket chosen once per dialer, not per attempt (T004, T013) | `TestSocketDialer/a socket that appears later…`; `TestStartVMManagerConnectsWhenSocketAppears` (see below) |
| M3 | `dialWithin`'s 1 s bound removed (T011, T013, T014) | `TestNewVMManagerBoundsTheAttempt`, `TestStartVMManagerAbandonsSilentHandshake`, `TestCleanupBoundsASilentDaemon` |
| M4 | the single-flight join replaced by a lock, then an attempt of its own (T020) | `TestStartJoinsInFlightAttempt` |
| M5 | `prepared` cleared when a redo fails (T020) | `TestRedoAfterReconnect` |
| M6a–d | the ownership step removed from create, hard reboot, start, resume — one at a time (T021) | `TestGuestStartMakesTheInstanceDirQEMUs`, the matching subtest each time |
| M7a | real mode with no manager turns `ACTIVE` again (T015) | `TestNoHypervisorCreateEndsError/no_manager_wired_in` |
| M7b | a `CreateVM` failure written inline again, with no fault (T015, T016) | `TestNoHypervisorCreateEndsError/unconnected_manager`; the contract case against a binary built with M7b (fails at the fault assertion) |
| M8 | the `loadConfig` check removed (T034, T035) | `TestLoadConfigLibvirtURI`; `TestUnsupportedLibvirtURIRefused` for server, cleanup and console-tokens |
| X1 | `HypervisorReady` always ready (T022; beyond T042's list) | `TestHypervisorReady` |
| X2 | `readinessChecks` without the hypervisor check (T023; beyond T042's list) | `TestReadinessChecks` |

All 16 were caught, none by a compile error. M7a and M7b ran on SQLite. M7a's branch is unreachable
from the real binary — `InitHypervisor` always leaves a manager — so it has no contract counterpart; before
the change, the contract case went RED on exactly that fake `ACTIVE`.

**One finding, fixed.** `TestStartVMManagerConnectsWhenSocketAppears` raced its own background loop. When
the loop's first dial ran after the test had created the socket, it connected at once. The test then
passed in milliseconds without exercising a later attempt, and in the combined M2 run it passed. It now
waits for the logged failed attempt before the socket appears. It takes one backoff step (1 s) every
time, and fails under M2 (10 s timeout).
