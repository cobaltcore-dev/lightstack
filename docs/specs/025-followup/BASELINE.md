# Baseline: SPEC-025 Follow-ups

Recorded before any change, per tasks T001/T002, so that a failure predating this feature is not
attributed to it. RED evidence for each story is appended below as it is gathered.

## T001 — Gate state at the pre-change tip

| Gate | Result |
|---|---|
| `go build ./...` | ✅ clean |
| `gofmt -l ./cmd ./internal ./pkg` | ✅ no drift |
| `make lint` (golangci-lint) | ✅ **0 issues** |
| `go test ./...` | ✅ all packages pass |

One warning appears in lint output and is **not** ours: golangci-lint's `generated_file_filter`
cannot read `../../bugfix/515/test/contract/glance/cache_test.go` — a path in a *sibling worktree*
that no longer exists. It is a cache artefact of the shared module path, not a finding, and it was
present before this feature.

**Conclusion**: nothing was broken before this feature started. Any gate failure from here is ours.

## T002 — Shared `lightstack-postgres` fixture state

The container is shared across ~12 worktrees. A sibling's fixtures once left `quotas.instances = 0`
for `default`, and a contract suite reported failures that had nothing to do with the code under
test. This feature is *about* quotas, so the check is done first.

**`quotas` (Nova's table, migration 007) for `default`** — healthy, not poisoned:

```
cores 20 | floatingip 10 | gigabytes 1000 | instances 10 | networks 10 | ports 50
ram 51200 | routers 10 | security_group_rules 100 | security_groups 10
snapshots 10 | subnets 10 | volumes 10
```

`instances = 10`, not `0`. The poisoning case is absent.

**`cinder_quotas` for `default`** — **EMPTY**. No rows at all.

This is worth more than a passing note, because it is the state most of US3's work runs against:

- Block-storage limits therefore resolve entirely from `cinderQuotaDefaults` in code
  (`volumes 100`, `gigabytes 10000`, `snapshots 100`, `backups 100`, `backup_gigabytes 10000`,
  `per_volume_gigabytes 1000`) — the FR-025 path, not the configured-row path.
- It is also the **no-row concurrency case** of FR-023 / T063: there is no `cinder_quotas` row to
  lock, so a naive `SELECT ... FOR UPDATE` implementation would give the concurrency guarantee only
  to projects an administrator has already touched — and this project, the one every test uses by
  default, is not one of them.
- A test that sets a limit will *create* the row and so exercise the configured path instead.
  T047 and T056 must therefore be explicit about which of the two they are testing.

**Note on the `quotas` table**: it carries `volumes`, `gigabytes` and `snapshots` rows of its own.
`resolveCinderQuotaLimits` reads `cinder_quotas` **only**, so those Nova-table rows are inert for
block storage. Not a defect, but a trap when reading fixtures by hand: setting
`quotas.volumes` will not change what Cinder enforces.

**Other projects present**: `spec020-scoping-other`, `spec021-admin/authz/other`,
`spec022-cinder-other`, `spec023-a/b`, `spec024-other`, `spec025-fip-other` — left by sibling
features. Harmless, but a listing test that assumes only `default` exists will be surprised.

## RED evidence

Appended per story as each "confirm RED" task runs.

### US1 — server-list filters (T016)

Run: `LIGHTSTACK_TEST_DB_URL=… go test ./internal/nova/ -run TestServerList`

| Test | Result | Role |
|---|---|---|
| `TestServerListFiltersMatchAndReject` | **FAIL** | every non-matching case returns the whole project |
| `TestServerListFilterNarrowsWithinProject` | **FAIL** | `?status=ERROR` returns the ACTIVE server too |
| `TestServerListNameIsRegex` | **FAIL** | no pattern discriminates |
| `TestServerListFilterCannotWidenScope` | **FAIL** | admin's project filter neither narrows nor scopes |
| `TestServerListRefusedFilters` | **FAIL** | `?host=` answers 200 with a list |
| `TestServerListUnfilteredUnchanged` | PASS | **control** — must stay passing |
| `TestServerListBriefAndDetailAgree` | PASS | **control** — both listings are equally wrong today |
| `TestServerListIgnoresUnknownParameters` | PASS | **control** — FR-004a's status quo |
| `TestServerListInvalidFilterValue` | PASS | **control** — no filter parsing exists to fault |
| `TestServerListFilterWithPaging` | PASS | **control** — paging works; the filter is what is missing |

The failure text is F7's signature verbatim:

```
?name=filter-nonexistent returned [filter-beta filter-alpha]; want an EMPTY collection
?status=SUSPENDED        returned [filter-beta filter-alpha]
?host=compute-01         code=200 with both servers; want 400
```

**Note on the harness.** DB-backed nova tests skip unless `LIGHTSTACK_TEST_DB_URL` is set, and the
`lightstack_test` database did not exist locally — CI creates it, a developer machine does not. Left
unset, every test above would have **skipped**, and "confirm RED" would have recorded ten skips as
though they were evidence. The database was created (`CREATE DATABASE lightstack_test OWNER lightstack`, separate
from the shared `lightstack`, migrations applied to version 68) specifically so the RED step could mean
something. **A skipped test is not a red test** — that distinction is the whole reason this file
exists.

**US1 GREEN (T024)**: all `TestServerList*` pass; the full `internal/nova` suite passes, so
nothing regressed. FR-007 is additionally pinned at SQL level in
`TestApplyQueryFiltersUnfilteredAddsNothing`, which needs no database and so keeps guarding the
unfiltered path in an environment where the DB-backed tests skip.

**One asymmetry found while wiring, not present in the spec.** The two listings page differently
inside: the brief listing computes a `ROW_NUMBER` offset *within* the filtered set, so its marker is
inherently filter-relative, while the detailed one resolves a `(created_at, id)` keyset that was
built from scope alone. Filters made the difference reachable — a marker the filter excludes would
have behaved differently depending on which representation was asked for, which FR-002 forbids. The
detail marker lookup now applies the same query filters, and `TestServerListMarkerParityAcrossListings`
pins the two together with a marker present, which T014's filter-only parity test could not see.

### US2 — image patch operations (T033)

| Test | Result | Role |
|---|---|---|
| `TestImagePatchAdd` | **FAIL** | `min_disk_gb in the database = "0"; want 7 — the operation was accepted and discarded` |
| `TestImagePatchRemoveRefused` | **FAIL** | 200 with the unchanged image |
| `TestImagePatchUnsupportedOperation` | **FAIL** | `move`/`copy`/`test`/`frobnicate` all 200 |
| `TestImagePatchUnwritablePath` | **FAIL** | `/status`, `/id`, `/hw_disk_bus` all 200 |
| `TestImagePatchIsAtomic` | **FAIL** | a document with an inapplicable member half-applies |
| `TestImagePatchValueType` | **FAIL** | a wrong-typed value reported as applied |
| `TestImagePatchMalformedDocument` | **FAIL** | a non-array document accepted |
| `TestImagePatchReplace` | PASS | **control** — the one operation that worked |
| `TestImagePatchZeroRows` | PASS | **control**, and see below |

**`TestImagePatchZeroRows` passing before the change is worth recording.** FR-015 wanted 404 for a
write matching no row, and 404 was already the answer — but for the wrong reason: the UPDATE matched
zero rows silently, then the trailing `GetImage` 404'd on its own scoping. Nothing checked
`RowsAffected`. The right answer by accident is indistinguishable from the right answer by design
until the accident stops holding, so the check is now explicit.

**A fixture bug of mine, caught by the control failing.** `TestImagePatchReplace` failed its first
run with `500 failed to get image`. The cause was my fixture, not the product: `GetImage` scans
`disk_format` and `container_format` into plain strings, and I had inserted the row without them.
Checked before patching the test — `CreateImage` always defaults both, so a NULL is unreachable
through the API and there is no product defect here. **The control failing is what surfaced it**; had
I marked that test as expected-to-fail like the others, the fixture bug would have been invisible
until the GREEN run.

**US2 GREEN (T042)**: all `TestImagePatch*` pass; the full `internal/glance` suite passes. The
`openstack image set --min-disk 10` half of T042 needs a running deployment and is carried into the
host runbook (T130).

### US3 — block-storage quota (T060)

**Process note, recorded rather than hidden.** For this story I wrote the implementation before the
tests, inverting the order the tasks specify. That left no RED evidence, which SC-013 requires be
*demonstrated*. Rather than assert the tests would have failed, the pre-change `internal/cinder`
files were restored from `HEAD` into a scratch copy, the route-level tests run against them, and the
implementation put back. All 14 fail without it:

```
TestQuotaVolumesCount TestQuotaGigabytes TestQuotaPerVolumeCeiling TestQuotaDefaultsPath
TestQuotaSnapshots TestQuotaSnapshotSizeCountsTowardGigabytes TestQuotaSnapshotFillsAllowance
TestQuotaBackups TestQuotaBackupGigabytes TestQuotaExtendChargesTheDelta
TestQuotaTransferAcceptEnforced TestQuotaImmediateEffect TestQuotaExistingOverageIsKept
TestQuotaConcurrentCreates                                                    — all FAIL
```

with the defect's signature: `create at limit: code=202 … want 413`, and
`after a refused create the project has 3 volumes; want 2`.

`TestQuotaDenialIsRecorded` was moved to its own file, `quota_denial_test.go`, because it names
package-internal symbols and so cannot compile against a tree without the implementation. Splitting
it is what let the other fourteen be run against the unmodified code at all.

**A `git stash` misstep, and what it cost.** The first attempt at this used `git stash push` on the
implementation files. The push failed — one file was untracked, which `stash push` by pathspec will
not take — and the following `git stash pop` therefore popped a **pre-existing stash belonging to
another branch** (`feature/420`) into the working tree, conflicting in `docs/blueprint.md`. Nothing
was lost: the failed pop retains its entry, all four pre-existing stashes were intact, and
`docs/blueprint.md` was restored from `HEAD`. The scratch-copy approach above replaced it — this
repository has four stashes from three other worktrees, so `git stash` is not a safe scratchpad here.

### US3 — three defects in the first implementation, found by its own tests

The tests did not merely confirm the work; each of these was wrong and the tests said so:

1. **The absolute-limits view had its own copy of the usage counting.** `plan.md` and `data-model.md`
   both stated that all three reporting surfaces read the shared resolution, so correcting
   `gigabytes` would land everywhere at once. False: the limits view resolved *limits* through
   `resolveCinderQuotaLimits` but counted *usage* itself, in three inline queries. It kept reporting
   5 GB where the quota views reported 10. FR-026a is now true rather than assumed — the view reads
   `cinderQuotaUsage`, and ~20 lines of duplicated counting are gone.
2. **My quota check released its lock before the write it authorised.** The check ran in its own
   transaction, committed, and the row was inserted afterwards — so two concurrent creates against a
   single remaining allowance both passed and both succeeded. `TestQuotaConcurrentCreates` caught it
   on the first run (`2 accepted (codes [202 202]); want exactly 1`). The lock is now held across
   check *and* insert: `provisionVolume` runs both in one transaction, and the insert goes through
   `tx` rather than `database.DB`. **Worth noting for a future ticket**: `nova.CheckQuota` — the
   pattern this was modelled on — has the same shape, checking in a transaction of its own and
   leaving the caller to insert later. Compute's quota checks are racy in exactly the way this one
   was.
3. **A ninth quota-consuming site.** T074 exists because FR-019 says to re-derive the site list from
   the code rather than trust an enumeration. Doing so found `ManageSnapshot`
   (`internal/cinder/manage.go`) inserting a snapshot with no check. The spec named two routes,
   planning found four, the finished code had nine. Every enumeration so far has been short by at
   least one.

**US3 GREEN (T078)**: the full `internal/cinder` and `internal/nova` suites pass.

### US5 — restore withdrawn (T094)

| Test | Result |
|---|---|
| `TestRestoreActionIsWithdrawn` | **FAIL** — `409 {"conflictingRequest":{"message":"instance is not soft-deleted"}}` |
| `TestNoActionGatesOnAnUnwrittenStatus` | **FAIL** — `restore` still in the dispatch table |
| `TestDeletionSemanticsUnchanged` | PASS — **control**, deletion must not change |

The 409 is the whole defect in one line: a *state-dependent* refusal, which tells a client the
action exists and the server is merely in the wrong state — for a state the product never writes.

**GREEN (T097)**: handler and dispatch entry removed (41 lines), full `internal/nova` suite passes.
`SOFT_DELETED` now appears nowhere in `internal/`, `pkg/` or `cmd/` except in two test comments
explaining why it does not.

#### Revised 2026-09-30 — `restore` answers as upstream (spec.md, Session 2026-09-30)

The withdrawal failed the gophercloud contract test `TestNovaRestoreInstance_Contract`
(`[]int{202, 404} does not contain 400`), and upstream agrees with the test: `deferred_delete.py`
routes `restore` at every configuration (expected errors 403/404/409), so a deleted server is 404
and a live one 409. `restore_withdrawn_test.go` became `restore_action_test.go`; T092 is superseded.

| Test | RED — the withdrawal | GREEN — route kept, handler answers 409 |
|---|---|---|
| `TestRestoreAnswersAsUpstream` (ACTIVE) | **FAIL** — `400 {"badRequest":{"message":"unknown action"}}` | PASS — 409 |
| `TestRestoreAnswersAsUpstream` (SHUTOFF) | **FAIL** — same 400 | PASS — 409 |
| `TestRestoreAnswersAsUpstream` (deleted) | **FAIL** — same 400 | PASS — 404 |
| `TestDeletionSemanticsUnchanged` | PASS — control | PASS |

`SOFT_DELETED` now appears nowhere in `internal/`, `pkg/` or `cmd/`. The host result for R7 in the
table below predates this revision; R7 now expects 409 and 404.

### US4 — catalog addressing (T084)

The new tests call the new signature (`BuildServiceCatalog` gained a `requestHost`
parameter), so they cannot compile against the pre-change tree — the defect is precisely
that **there was nowhere to pass the addressed host**. RED was therefore demonstrated by
extracting the pre-change `substituteURLTemplates` and running it directly:

```
pre-change result: http://lightstack.internal:8774/v2.1
CONFIRMED RED: the addressed host lightstack.example.com is absent;
               the configured lightstack.internal was served instead.
```

A signature with no parameter for the value is a stronger statement of the defect than a
failing assertion would be: the information never reached the function.

**GREEN**: `TestCatalogEchoesRequestHost`, `TestCatalogNeverDoublesThePort`,
`TestCatalogFallsBackWhenNoHost`, `TestCatalogLeavesObjectStorageAlone`,
`TestCatalogHostPrecedence`, `TestCatalogCacheKeyIncludesHost` all pass, and the full
`internal/keystone` suite passes.

**The pre-existing catalog tests all still pass unchanged in meaning.** Each one exercised the
configured-host path, and each now passes `""` as the request host — which `catalogHost` resolves to
`LIGHTSTACK_ENDPOINT_HOST`, exactly the value they set. So the fallback behaviour is pinned by the tests
that predate this story rather than by new ones.

### US7 — address batching (T117)

RED for this story is structural rather than assertive: `instanceAddressMap` did not exist, so
`TestBatchedAddressesRoundTripCount` could not compile against the pre-change tree. The equivalence
controls (`TestAddressesIncludeFloatingIPs`, `TestAdminSeesAnotherProjectsFloatingAddresses`) predate
this story, exercise the detail listing, and pass before and after — which is what makes them useful
here: they are the guard against the rewrite changing output, and FR-046 is exactly that requirement.

**A real bug in my own filter, caught by a POSITIVE assertion.** The `ip` filter's correlated
subquery read `p.device_id = id`, and in the brief listing — where the table has no alias — `id`
binds to **`ports.id`**, not `instances.id`: `ports` has an `id` column of its own and the inner
scope shadows the outer. The subquery compared a port to itself, matched nothing, and the filter
silently returned an empty list. `?ip=<matching address>` returning `{"servers":[]}` is what exposed
it.

Worth noting because this feature's whole thesis is that negative assertions catch what positive ones
miss — here the reverse held. A silently-empty filter passes every "non-matching value returns
nothing" test ever written. **Both directions are needed, and for opposite reasons.**

**GREEN (T123)**: `gofmt` clean, `go build ./...` clean, and every package under `internal/` and
`pkg/` passes.

### US6 — microversion negotiation (T103)

| Test | Result | Role |
|---|---|---|
| `TestMicroversionOutOfRangeRefused` | **FAIL** | 2.99 answered normally at the maximum |
| `TestMicroversionAppliedVersionReported` | **FAIL** | no version header on any API response |
| `TestMicroversionNoHeaderUnchanged` | **FAIL** | no header emitted at all |
| `TestRequestedMicroversionParsing` | PASS | **control** — parsing predates the story |
| `TestMicroversionMalformedTolerated` | PASS | **control** — pins divergence A-006 |
| `TestCreateImageGatedAt245` | PASS once the helper existed | the audit's one new gate |

**A defect found by the applied-version test, and it is not only cosmetic.** Reporting the parsed
float back as a string gave `compute 2.9` for a client that negotiated `2.90` — because `2.90` as a
float *is* `2.9`, and **2.9 is itself a real upstream microversion** (the one that introduced
`locked`). The reporting half was fixed by echoing the request's string rather than re-rendering the
float, and the comparison half was left float-based, recorded as finding A6.

**A6 has since been fixed, and it was larger than this entry judged it.** Branch review found the
float ordering did not merely conflate 2.9 with 2.90 — it *inverted* the order of both gates, since
`2.9 > 2.45 > 2.21` numerically while every one of those versions is smaller than the next. Clients
negotiating 2.3–2.9 were receiving gated behaviour, and `"2.100"` was passing the 406 range check as
`2.1`. A6 deferred the repair on the reasoning that "every gate in this product sits below 2.9 and is
unaffected", which was false: 2.21 and 2.45 are below 2.9 as floats and above it as versions.
Comparison is now `struct{ major, minor int }` through `atLeast`. See `microversion-audit.md` A6.

### T133 — the test audit this feature's thesis demands

Every new and extended test file, checked for the shape this feature exists to remove — a check that
cannot fail:

| File | Tests | Failure paths |
|---|---|---|
| `nova/server_list_filters_test.go` | 11 | 32 |
| `nova/address_batching_test.go` | 5 | 22 |
| `nova/microversion_test.go` | 6 | 13 |
| `nova/restore_withdrawn_test.go` | 3 | 9 |
| `glance/image_patch_test.go` | 9 | 30 |
| `cinder/quota_enforcement_test.go` | 14 | 50 |
| `cinder/quota_denial_test.go` | 1 | 4 |
| `keystone/catalog_host_test.go` | 6 | 18 |
| `common/errors_test.go` (extended) | 13 | 42 |
| `common/baseurl_test.go` (extended) | 4 | 6 |

**72 tests, 226 failure paths, and no test body without one** — verified mechanically, not by
reading. Every story has a recorded RED run above, and the controls are labelled as controls rather
than counted as evidence.

**What the RED runs actually proved, story by story:**

| Story | RED method | Result |
|---|---|---|
| US1 | run as written | 5 of 10 fail; 5 controls pass |
| US2 | run as written | 7 of 9 fail; 2 controls pass |
| US3 | pre-change files restored from `HEAD` | 14 of 14 fail |
| US4 | pre-change function extracted and run | defect reproduced directly |
| US5 | run as written | 2 of 3 fail; 1 control passes |
| US6 | run before registering the middleware | 3 of 6 fail; 3 controls pass |
| US7 | structural — the function did not exist | pre-existing equivalence tests are the guard |

**And the finding that cuts against this feature's own thesis.** US7's `ip` filter bug — a
correlated subquery binding `id` to `ports.id` instead of `instances.id` — was caught by a
**positive** assertion, not a negative one. A silently-empty filter passes every "non-matching value
returns nothing" test ever written. Both directions are needed, for opposite reasons: the negative
case catches a filter that does nothing, the positive case catches a filter that matches nothing.

---

## Real-mode host run, 2026-09-02

### Passed as written

| Step | Result |
|---|---|
| R1a — filter narrows; non-match returns nothing | ✅ `alpha` / empty / `alpha beta gamma` / `alpha -> 10.244.20.21` |
| R1b — `name` is a regex | ✅ `alpha` / `alpha beta` / `[]` / `400` |
| R1c — both listings agree | ✅ four × `SAME` |
| R2a — the four refusals | ✅ four × `status=400`, each naming its filter |
| R2b — benign extras ignored | ✅ four × `count=3` |
| R2c — a filter cannot widen scope | ✅ `0` / `3` / `0` / `3` |

### R3 FAILED, and it was a real product defect — not a runbook error

`openstack image set --min-disk 10` answered **`400 Invalid field value`** where it should have written
10. Three symptoms, one root cause:

| Symptom | Reading |
|---|---|
| R3a: `image set --min-disk 10` → 400 | the integer was refused, not applied |
| R3c line 1: `[replace 42, remove /name]` → **400** | expected 403 for the `remove`; the 400 came from the *first* member, so the atomicity path was never reached |
| R3c line 3: patch a nonexistent image → **400** | expected 404; validation refused before the row lookup ran |

**Root cause.** `internal/middleware` sets `binding.EnableDecoderUseNumber = true` in a package
`init()`, so every JSON number bound into an `any` reaches a handler as **`json.Number`**, a string
type. `coerceImagePatchValue` accepted only `float64`, so it refused every integer patch. The product
traded F8's silent failure for a loud wrong one.

**Why the unit tests did not catch it, which is the more important half.** That flag is set by an
`init()` in a package the glance test harness **never imported**. The production binary links it
through `main.go`; the tests did not, so they bound numbers as `float64` and asserted a shape
production never produces. Nine tests passed against a decoder configuration that does not exist in
the deployed binary.

The codebase already knew: `internal/nova/handlers.go` documents this at lines 382, 408 and 488, and
cinder's `os-extend` action branches on `json.Number` explicitly. **I did not read those before
writing the coercion.**

**Fixed.**

- `patchInt` accepts `json.Number`, `float64`, `int64` and `int`, and rejects strings, bools and
  non-integral values. Both decoder shapes work, so link order cannot break it.
- `image_patch_test.go` now **blank-imports `internal/middleware`**, so the tests inherit production's
  actual decoder configuration rather than a copy that can drift.
- `TestImagePatchBindsNumbersAsJSONNumber` asserts the flag is set, so the blank import cannot be
  tidied away as an unused dependency — which would silently return every value-type test in the file
  to testing the wrong shape.
- `TestPatchIntAcceptsBothShapes` pins all six input shapes directly.

**This is the fourth instance of this feature's own shape**, and the first inside its test harness: a
test that could not fail, because it exercised a configuration the product does not run under. The
others were F7/F8 themselves, SPEC-025's nine empty verification steps, and R2a's suppressed `jq`
error.

---

## Real-mode host validation — COMPLETE (2026-09-02 / 2026-09-03)

k3s, libvirt/KVM, node-local storage, `libvirt_mode: "real"` / `networking_mode: "iptables"` /
`security_group_mode: "iptables"` / `storage_mode: "local"` confirmed by the deploy gate.

**All nine steps pass. All seven user stories validated against a real host.**

| Step | Story | Assertions | Result |
|---|---|---|---|
| R1 | US1 filters | 11 | ✅ |
| R2 | US1 refusals + scope | 12 | ✅ |
| R3 | US2 image patch | 12 | ✅ |
| R4 | US3 quota, 7 parts | 20 | ✅ |
| R5 | US3 counting | 5 | ✅ |
| R6 | US4 catalog host | 4 | ✅ |
| R7 | US5 restore withdrawn | 2 | ✅ |
| R8 | US6 microversions | 12 | ✅ |
| R9 | US7 addresses + `ip` | 9 | ✅ |

Selected evidence:

- **R1a** `--name doesnotexist` → nothing; the trap line names `alpha` with `alpha`'s own address.
- **R2a** four × `status=400`, each naming its filter; **R2b** four × `count=3`.
- **R3a** `openstack image set --min-disk 10` → `10`; **R3c** `403` · `10` (not 42) · `404`.
- **R4b** boot-from-volume refused `413` naming `gigabytes`, no server and no orphan volume left.
- **R4d** 32-char auth key, `413`, transfer still pending, `202` after raising the limit.
- **R4f** one `202`, one `413`, final count exactly `USED+1` — the TOCTOU race is closed.
- **R5** `22 → 25` on both surfaces, equal at every point.
- **R6** `http://lightstack-alias:8774/v2.1`, 6 endpoints naming the alias, **0** two-port authorities.
- **R8b** both `createBackup` and `createImage`: `image_id` at 2.45, 0 bytes at 2.44.
- **R9a** per-server and listing maps byte-identical, carrying `fixed 10.244.20.164` **and**
  `floating 192.0.2.101`; portless server → `{}`.
- **R9b** `?ip=<alpha's address>` → `alpha` alone; non-matching → `[]`; subnet regex → `alpha beta`.

### One product defect found on the host, fixed

**`coerceImagePatchValue` accepted only `float64`.** `internal/middleware` sets
`binding.EnableDecoderUseNumber` in a package `init()`, so every JSON number reaches a handler as
`json.Number`. R3 failed three ways on the first attempt — `image set` answered `400 Invalid field
value`, the atomicity case answered 400 from its *first* member instead of 403 from the `remove`, and
a missing image answered 400 instead of 404. All one cause.

**The unit tests could not have caught it**: the flag is set by an `init()` in a package the glance
test harness never imported, so the tests bound numbers as `float64` and asserted a shape production
never produces. Fixed by `patchInt` (accepts both shapes), a blank import of `internal/middleware` in
the test file, and `TestImagePatchBindsNumbersAsJSONNumber` so the import cannot be tidied away.

### One scope decision made on the host

**`CreateImageAction` was not gated at 2.45.** The audit found the misleading comment in
`CreateBackupAction` and gated that handler; R8b exercises `createImage`, which dispatches elsewhere
and returned **no body at any version** — upstream's pre-2.45 shape, served unconditionally. The
mirror image of the defect the audit found, in a handler upstream changed at the same time. Fixed,
recorded as audit finding A7, and both actions now read one gate with
`TestCreateImageActionsBothGated` pinning the pair.

### Two pre-existing gaps found, neither in scope — file separately

1. **`/v3/{project}/backups` is unrouted.** Backup routes exist only on the flat `/v3/backups` path,
   so `openstack volume backup create` answers **404** against a project-scoped catalog URL. Same
   family as SPEC-020's B1. The quota enforcement behind it is correct and reachable by `curl`.
2. **`networks: "auto"` / `"none"` cannot be bound.** From microversion 2.37 upstream accepts
   `networks` as a string; Lightstack's request struct binds an array, so `openstack server create` answers
   **400 invalid request body** unless `--network` is given explicitly. Wide: it is the most common
   form of the most common command.

### Ten runbook defects found and fixed by running it

Recorded because the pattern matters more than any single instance: **eight of the ten were checks
that could not fail** — the exact shape this feature exists to remove, in the document written to
verify it.

| # | Defect | How it presented |
|---|---|---|
| 1 | `curl -w` piped into `jq` | four correct-looking messages, status code never asserted |
| 2 | R2c had only negative assertions | a broken predicate would have passed |
| 3 | R4b boot volume 5 GiB vs `min_disk` 10 | in-quota case failed for a size reason |
| 4 | R4b cleanup missed the boot volume | R4c's fixture inherited 10 GiB |
| 5 | `auth_key` read from `show`, not `create` | empty key, accept refused 400, quota never reached |
| 6 | `openstack quota show` reports limits, not usage | R5 never read the number it existed to check |
| 7 | R5 fixture omitted `--volumes` | volume create refused by the previous step's limit |
| 8 | address maps compared CLI output to raw API | two different shapes, FR-046 never tested |
| 9 | equality check passed on two empty strings | `IDENTICAL` printed while the service was down |
| 10 | setup 1.1's quota read-back used `-c volumes` | same as #6; the Pass line asked for output that could never print |

Defect 9 is the sharpest: it was introduced *in the assertion written to fix defect 8*. Defect 10 is
the most persistent: the identical `-c <resource>` mistake was made twice, in two sections, and the
second instance was only found by auditing the fix for the first. An equality
assertion between two reads must establish that either read produced something before comparing them.

R4c now derives its own limit from live usage rather than hardcoding one, and R9a builds the
external-network + router topology a floating IP requires instead of assuming a tenant network will
serve as a pool.

### A cross-cutting annoyance worth knowing

**Running the host runbook drops the unit-test database.** Setup 1.1 calls `make db-up`, which
recreates the `lightstack-postgres` container — and `lightstack_test`, which the DB-backed Go tests need, lives in
that container and is created by CI rather than by any repo target. It vanished three times during
this validation, and each time `go test ./internal/...` reports a *connection* failure that reads
like broken code:

```
LIGHTSTACK_TEST_DB_URL is set but connecting to it failed: ... database "lightstack_test" does not exist
```

Recreate it with:

```
docker exec lightstack-postgres psql -U lightstack -d postgres -c "CREATE DATABASE lightstack_test OWNER lightstack;"
```

Worth a `make test-db` target so this is not rediscovered. **And note the failure mode it hides**:
with `LIGHTSTACK_TEST_DB_URL` *unset* the DB-backed tests SKIP rather than fail, which is how a suite can
report success while exercising none of its database coverage.
