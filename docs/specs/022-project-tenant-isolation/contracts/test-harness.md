# Contract: Multi-tenancy suite harness (T1 and its siblings)

**Feature**: `022-project-tenant-isolation` | **Requirements**: FR-017, FR-017a, FR-018 – FR-022
**File**: `test/multi_tenant_test.sh` | **Target**: `make test-multi-tenant` (`Makefile:69`)

The suite that should have caught B8 and B2 reported passes while creating nothing. T1 — the token
extraction — is already fixed; what remains is that the suite never checks whether its *tenants* were
created, and never exercises the two paths the defects live on.

## What T1 already fixed (verify, do not redo — FR-020)

All three suites now read the token from the `X-Subject-Token` **header**, not from a body field that
never existed:

| Suite | Extraction site |
|---|---|
| `test/multi_tenant_test.sh` | `:41`, `:96`, `:101` |
| `test/performance_test.sh` | `:28` |
| `test/vxlan_multinode_test.sh` | `:243`, `:315` |

FR-020 requires this be re-confirmed, so the feature builds on a harness that authenticates. Recording
the check is the whole task.

## Gap 1 — setup guards (FR-017)

**Today** only `ADMIN_TOKEN` is guarded (`:43`). Everything the setup then creates is used unguarded:

| Variable | Line | Guarded today |
|---|---|---|
| `ADMIN_TOKEN` | 38–43 | ✅ |
| `PROJECT1_ID`, `PROJECT2_ID` | 48, 55 | ❌ |
| `USER1_ID`, `USER2_ID` | 63, 70 | ❌ |
| `MEMBER_ROLE_ID` | 76 | ❌ |
| `USER1_TOKEN`, `USER2_TOKEN` | 92, 99 | ❌ |

With B5 unfixed, `POST /v3/projects` returns `500`, `jq -r '.project.id'` prints `null`, and the suite
runs every check with `PROJECT1_ID=null` — reporting passes for isolation it never tested. That is the
`PROJECT1=null` symptom in the ticket.

**Contract**: a single guard helper, applied to all seven values.

```
require_value <name> <value>
  → fails when the value is empty OR the literal string "null"
  → prints what could not be created, and exits non-zero
```

**The `null` half is essential, not defensive padding.** `jq -r` prints `null` for a missing field, and
`[ -z "null" ]` is **false** — so the existing guard *shape* could never have caught this. That is
precisely the T1 defect, one level up from the token.

**Guarantee**: when setup cannot complete, **zero** checks run and **zero** passes are reported
(FR-017, SC-006).

## Gap 2 — teardown on exit (FR-017a)

**Today** cleanup is a block at the end of the script (`:265-305`), and the script runs `set -e`. Any
failure before that point skips it entirely.

**Why this becomes urgent**: the suite creates `test-project-1` and `test-project-2` under **fixed**
names. Today a failed run leaves nothing behind, because creation returns `null` — the defect hides its
own residue. Once FR-004 makes a duplicate name a `409` and FR-017 exits setup early, a run that dies
part-way leaves a project behind and **every later run aborts at setup with a conflict**. Two fixes in
this feature combine to create the failure mode, so this is not optional polish.

**Contract**:

| Requirement | Detail |
|---|---|
| Registration | a teardown trap on `EXIT`, registered **before** the first resource is created |
| Coverage | pass, failed check, aborted setup — teardown runs in all three |
| Idempotency | tolerates resources that were never created (unset or `null` ids), silently |
| Content | the existing cleanup block, moved — not duplicated |
| Ordering | dependants before dependencies (servers → volumes/networks/images → users → projects) |
| Exit code | preserved; teardown must not mask a failure with its own success |

**Guarantee**: two consecutive runs give the same result, and a run immediately after an aborted one
starts clean — with **zero** manual cleanup (SC-006a, US4 scenario 5).

Fixed names are kept deliberately: they appear in the suite's own output (`Created PROJECT1=…`) and make
failures readable. A timestamp suffix would remove the collision but leave residue accumulating in a
shared database with nothing to reclaim it.

## Gap 3 — the cross-tenant subnet write check (FR-018)

**Today** the network isolation check (`:146-166`) is **read-only**: user1 creates a network, and the
suite asserts user2 cannot *see* it. It never attempts a write — which is exactly how B8 survived a
suite named "Multi-Tenancy & Isolation".

**Contract** — extend that check:

| Step | Assertion |
|---|---|
| 1 | user1 creates a network → id captured and guarded |
| 2 | user2 `POST /v2.0/subnets` with `network_id` = user1's network | refused: `403` if user1 marked it shared, otherwise `404`. **Never `201`** |
| 3 | user1 `GET /v2.0/networks/{id}` | no subnet attributed to it |
| 4 | user1 `GET /v2.0/subnets` | empty — no trace of user2's attempt |
| 5 | user2 creates a subnet on user2's **own** network | `201` — the fix costs no legitimate capability |

Step 5 matters as much as step 2: a check that only proves refusal would pass equally well against a
handler that refuses everything.

## Gap 4 — the project-scoped volume list check (FR-019)

**Today** the suite creates and deletes volumes on the project-scoped URL form (`:171`, `:285`) but
**never lists** on it — which is how a `405` on that collection went unnoticed.

**Contract**:

| Step | Assertion |
|---|---|
| 1 | user1 lists volumes on `GET /v3/$PROJECT1_ID/volumes` | `200`, includes the volume created at `:171`. Not `405` |
| 2 | user2 lists on `GET /v3/$PROJECT1_ID/volumes` — user1's project in the path | `200`, and **user1's volume is absent** — the path segment never widens visibility (FR-015) |

Step 2 is the isolation half, and it belongs in this suite rather than a unit test: it is the assertion
that would catch someone "fixing" the path segment by actually honouring it.

## What the suite must not become

- **No new dependency**: `curl` + `jq` + `bash`, as today.
- **No sleeps or retries**: the constitution forbids retry loops, and every operation here is
  synchronous by design (Article II).
- **No real mode**: stub is sufficient for all four defects (A8).
- **Not a replacement for unit tests**: FR-022 puts the routing shape and the entitlement matrix in Go
  tests, where the eight-row matrix is cheap. This suite proves the two-tenant story end to end, which
  is what a Go test cannot.

## Test obligations

| Case | How |
|---|---|
| Setup guards fire | run against a stack with B5 unfixed → non-zero exit, zero passes reported |
| Teardown runs on abort | interrupt or force-fail a run, then re-run → starts clean |
| Two consecutive runs agree | run `make test-multi-tenant` twice |
| The four new assertions fail before the fixes and pass after | FR-021 — record both states per defect |
| The three T1 suites still extract a real token | `grep` the extraction sites; run each suite |
