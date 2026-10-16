# OpenStack CLI Conformance Suite (SPEC-028)

Systematic verification that the **built lightstack binary** works against the
**python OpenStack CLI** (`openstack`) across all cases — every CLI-exposed
operation of the five services, happy path and negative path — enforced in CI.

Implements [SPEC-028](../../docs/specs/028-cli-conformance-test-environment/plan.md)
for [issue #105](https://github.com/cobaltcore-dev/lightstack/issues/105).

## Quick start

```bash
make build                      # the suite tests THIS binary
make test-cli                   # runs against a running server, or starts one via docker
# or, with a server already running (stub mode, default creds):
./test/cli/bin/run.sh
./test/cli/bin/coverage.sh      # the coverage gate — see below
```

Requirements: `bash`, `jq`, `curl`, `python3`, and the pinned CLI
(`pip install -c test/cli/constraints.txt python-openstackclient`, or point
`OPENSTACK_CLI` at an existing binary).

## How it works

```
bin/run.sh ── sources ──> lib/common.sh ──> lib/assert.sh, lib/report.sh
    │                                                │
    │  fixtures_ensure/teardown (lib/fixtures.sh)    │ results.json + results.junit.xml
    ▼                                                ▼
suites/<service>.sh   ── case records ──> results/results.jsonl ──> bin/coverage.sh
manifest/<service>.json ──────────────────────────────────┘ (the gate)
```

- **Suites** (`suites/*.sh`) drive the real `openstack` CLI through the `osc`
  wrapper and record one result per case.
- **Manifests** (`manifest/*.json`) enumerate the command surface per service —
  the machine-checkable definition of "all cases". See
  [manifest/README.md](manifest/README.md) for the schema.
- **The gate** (`bin/coverage.sh`) cross-references manifests with results and
  fails on any tier A/B command that is untested, failing, or skipped.
  `known-failing` cases are allowed but counted and listed — the debt must
  shrink, never hide (no pass-rate thresholds).

## Tiers

| Tier | Meaning | Case id | Gated |
|------|---------|---------|-------|
| A | happy path + output shape (`-f json` fields present and typed) | `<manifest id>` | yes |
| B | negative paths (404/400/409 semantics via the CLI) | `<manifest id>.neg` | yes |
| C | cross-service workflows | scenario id, verbatim | nightly (`--strict`) |
| S | smoke — harness end-to-end proof | `smoke.*` | no |

## The lib API (locked)

Suites source `lib/common.sh` and use exactly these:

```bash
osc <args...>        # run the CLI (always -f json unless -f passed; retries
                     # once without -f for formatter-less mutation commands).
                     # Sets OSC_RC, OSC_OUT, OSC_ERR, LAST_CMD, LAST_CMD_MS.

t_begin keystone.project.list      # open a case (id = manifest id)
osc project list
assert_rc 0                        # see lib/assert.sh for all assertions
assert_jq_type '.' array
assert_jq '[.[] | select((.Name // .name) == "default")] | length' 1
t_end                              # close + record the case

t_fail "reason"                    # mark the current case failed (repeatable)
t_known_failure "reason"           # tracked, does not fail the run
t_skip "reason"                    # recorded as skipped (fails the gate)
```

## Conventions (mandatory)

1. **Naming**: every resource a case creates is named `cli-conf-<slug>-<unique>`.
   The fixture sweep reclaims anything with the `cli-conf` prefix — floating
   IPs excepted (nameless; suites that allocate them must release them).
2. **Rerun-safety**: a case must pass when run twice against the same server —
   delete a possible leftover before creating, or derive the name from the
   case id.
3. **One manifest file per service**, owned by that service's suite; ids in the
   suite must match ids in the manifest exactly.
4. **Known failures** are recorded in the suite with `t_known_failure`, never
   by deleting the case — and every one needs a reason string.
5. **Output shape matters**: assert the fields the CLI surfaces, not just the
   exit code. Note OSC 7 emits display-cased keys for `list` commands
   (`ID`, `Name`) but raw lowercase API fields for `create`/`show` —
   `(.Name // .name)` covers both.

## Running

```bash
./test/cli/bin/run.sh                     # everything (fixtures + all suites + teardown)
./test/cli/bin/run.sh --suite keystone    # one suite, no teardown (dev iteration)
./test/cli/bin/run.sh --fast              # PR subset (everything except workflows)
./test/cli/bin/run.sh --strict            # known-failing/skip also fail the run (nightly)
./test/cli/bin/run.sh --no-cleanup        # keep fixtures and created resources
bash suites/keystone.sh                   # standalone; results under results/standalone/
./test/cli/bin/coverage.sh                # gate against results/results.json
./test/cli/bin/coverage.sh --strict       # also require tier C (nightly)
```

Environment: the standard `OS_*` variables (defaults match a local stub-mode
deployment: `http://localhost:35357/v3`, `admin`/`secret`), plus `OPENSTACK_CLI`,
`OSC_TIMEOUT` (default 30s), `NO_COLOR`.

If no server is at `OS_AUTH_URL` and docker is available, `run.sh` starts
`deployments/docker-compose.binary-test.yml` — the runtime image with this
checkout's `bin/lightstack` bind-mounted over the in-image binary, so the suite
always exercises the artifact `make build` produced, not a container rebuild.
CI runs the binary natively instead (artifact download + PostgreSQL service).

## Results

- `results/results.json` — aggregate + per-case records (latest full run)
- `results/results.junit.xml` — JUnit XML for CI tooling
- `results/standalone/` — per-run records for standalone suite invocations
- Exit code: 0 iff no case failed (known-failing/skipped do not fail the run;
  the coverage gate handles those)

## Adding a case

1. Add the command to your service's `manifest/<service>.json` (tier A, and
   tier B if a negative case makes sense).
2. Write the case in `suites/<service>.sh` using the id from the manifest
   (`<id>` for tier A, `<id>.neg` for tier B).
3. `./test/cli/bin/run.sh --suite <service> && ./test/cli/bin/coverage.sh`
   until green.

## Pinning policy

`constraints.txt` pins `python-openstackclient`. Upgrades are deliberate PRs
(batch the expected diffs), never ambient. Current pin: 7.0.0.
