# Evidence Log: Project/Tenant Lifecycle & Isolation Fixes

**Feature**: `022-project-tenant-isolation` | **Ticket**: [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545)
**Purpose**: the failing-before / passing-after record FR-021 and SC-007 are checked against.

## Environment

| Item | Value |
|---|---|
| Base commit | `1a9e4bd` (feature/534 tip) |
| Go | 1.26.3 linux/amd64 |
| Unit-test database | `postgres://o3k:secret@localhost:5432/o3k_test` (`O3K_TEST_DB_URL`) |
| RED capture target | a pre-existing O3K already running on the standard ports at base code (`/app/o3k`, started 2026-08-04 06:35, left over from #534's host validation). Read-only probes only |
| Date | 2026-08-04 |

**Baseline gates before any edit** (T003): `make test` → 0 failing packages. `make lint` → **0 issues**.
`go test ./internal/keystone/...` → 48 pass, **0 skips** (T002 confirms DB-backed tests genuinely run
rather than skipping).

---

## B5 / P1 — domain alias on tenant creation

### RED (captured 2026-08-04, base code)

| # | Probe | Result |
|---|---|---|
| 1 | `POST /v3/projects` `{"domain_id":"default"}` | **`500`** `{"computeFault":{"code":500,"message":"failed to create project"}}` |
| 2 | `POST /v3/users` `{"domain_id":"default"}` | **`400`** `{"badRequest":{"code":400,"message":"specified domain not found or disabled"}}` |
| 3 | `GET /v3/domains/default` | **`404`** `{"itemNotFound":{"code":404,"message":"Could not find domain: default."}}` |
| 4 | `GET /v3/domains?name=zzz-nope` | **`1` domain returned** — the filter is ignored entirely |

Probe 4 is the discriminator: the deployment has exactly one domain, so `?name=Default` returning 1 is
ambiguous. Asking for a name that cannot exist and still getting 1 proves no filtering happens.

Probe 1 vs 2 is the whole story of the defect: two handlers in the same file receive the same value;
the one that looks the domain up answers honestly, the one that passes it straight into a `uuid`
column produces a generic fault.

### GREEN — unit level (verified against **this** build)

`go test ./internal/keystone/` — 12 targeted tests, all pass. These drive the **real** `RegisterRoutes`
through `httptest` against a real Postgres, so they exercise the same handler chain a client would:

| Test | Covers |
|---|---|
| `TestResolveDomainRef` (7 subtests) | alias / empty / uuid / unknown / non-identifier / case-sensitivity |
| `TestResolveDomainRefRejectsDisabledDomain` | FR-002's enabled check, both directions |
| `TestResolveDomainRefAnyDomainState` | creation vs retrieval ask different questions |
| `TestResolveDomainRefMalformedNeedsNoDatabase` | the syntax guard precedes the query |
| `TestCreateProjectDomainReference` (7 subtests) | `201` for alias/omitted/empty/uuid; `400` naming the field for unknown/disabled/non-identifier; response carries the **stored UUID** |
| `TestCreateProjectDuplicateNameIsConflict` | `409` naming the duplicate (was `500`) |
| `TestCreateProjectNoServerFaults` (7 subtests) | SC-002's enumerated set: every rejected payload is 4xx, **zero** 5xx |
| `TestCreateUserAcceptsDomainAlias` | alias `201`; admin gate still precedes; existing `400` text preserved |
| `TestCreateGroupAcceptsDomainAlias` | FR-006 |
| `TestGetDomainByAlias` | `GET /v3/domains/default` → `200`; verbatim `404` preserved for other non-identifiers; **disabled domain still `200`** |
| `TestListDomainsNameFilter` | `?name=` filters; case-sensitive; nonexistent name → `0` |
| `TestRegisterRoutesMiddlewareWiring` | the moved route, and that PATCH/DELETE/config stay behind the middleware |

**A regression this caught before it shipped**: routing `GetDomain` through the resolver initially made
`GET /v3/domains/<disabled uuid>` a `404`, because the resolver filters `enabled = true`. That query
never filtered on enabled, and the response reports the flag — so retrieval would have started hiding
disabled domains. Fixed by making the enabled requirement an explicit `domainState` argument, with the
two call-site intentions named (`enabledDomainsOnly` for creation, `anyDomainState` for retrieval).

**Static**: `grep -rn "000000000100" --include='*.go' internal/ | grep -v _test` → only
`bootstrap.go:20`, which legitimately seeds the row (FR-003 / T024).

### GREEN — live (this branch's binary, standard ports, 2026-08-04 10:05)

| # | Probe | RED | GREEN |
|---|---|---|---|
| 1 | `openstack project create --domain default probe` | `500` | **created**, id `e96b7935-…` |
| 2 | `POST /v3/users` `{"domain_id":"default"}` | `400` | **`201`**, user created |
| 3 | `GET /v3/domains/default` | `404` | **`200`** `{"domain":{"id":"…000100","name":"Default",…}}` |
| 4 | `GET /v3/domains?name=zzz-nope` | `1` domain | **`0`** |
| 4b | `GET /v3/domains?name=Default` | `1` (unfiltered) | **`1`** (filtered) |
| 4c | `GET /v3/domains?name=default` | `1` | **`0`** — names stay case-sensitive |

**A separate pre-existing gap this surfaced**: `openstack user create` and `openstack domain show` still
end with a CLI-side `'links'` error even though the API returns `201`/`200` and the resource is created —
python-openstackclient's display code expects a `links` field that O3K's identity `users`/`domains`
responses omit. Proven unrelated to this feature: `openstack domain show <UUID>`, a path this change never
touches, fails identically. Recorded as observation O8. The command the ticket names,
`openstack project create --domain default`, works end to end because `CreateProject`'s response does
carry `links`.

---

## B8 — cross-tenant subnet write 🔒

### RED

**Static** (T006):

```
awk '/func \(svc \*Service\) CreateSubnet/,/^}/' internal/neutron/network.go | grep -c 'FROM networks'
→ 0
```

The handler never consults the `networks` table, so `network_id` from the request body is never
checked against the caller's project.

**Live**: not capturable at base code. Demonstrating it needs two tenants, and creating a tenant is
B5 — the defect that blocks it. This is exactly what task T006 anticipated: static half now, live half
immediately after US1 lands.

### GREEN — unit level (verified against **this** build)

`go test ./internal/neutron/` — 17 assertions across 4 tests, all pass:

| Row | Case | Before | After |
|---|---|---|---|
| 1 | own network | `201` | `201` |
| 2 | foreign **private** network | **`201`** 🔒 | **`404`** |
| 3 | foreign **shared** network | **`201`** 🔒 | **`403`** |
| 4 | admin on a foreign network | `201` | `201` |
| 5 | admin on own network | `201` | `201` |
| 6 | well-formed id naming nothing | **`500`** | **`404`** |
| 7 | malformed id | **`500`** | **`400`** |
| 8 | `network_id` absent | `400` | `400` |

Plus: `TestCreateSubnetRefusalLeavesNoResidue` — zero subnet rows after rows 2 and 3, and the victim's
own `GET /v2.0/subnets` shows no trace. `TestCreateSubnetOwnNetworkResponseShape` — all 14 response
fields preserved, `dns_nameservers` still an empty list rather than `null`, `tenant_id` still the
caller's. `TestCreateSubnetRefusalIsAudited` — exactly one `warn` naming caller/network/owner for rows
2–3, and **none** for rows 1 and 4–7.

**Static, with a caveat.** The ticket's one-liner scans only `CreateSubnet`'s own body:

```
awk '/func \(svc \*Service\) CreateSubnet/,/^}/' internal/neutron/network.go | grep -c 'FROM networks'
→ 0    ← still zero
```

The entitlement lookup lives in a named helper, `callerMaySubnetNetwork`, because inlining ~50 lines
(with the comment explaining the deliberate divergence from `CreatePort`) into an already-long handler
would be worse code. The requirement is met — the handler does consult `networks` before inserting — but
the ticket's grep was written assuming an inline fix. Equivalent checks:

```
awk '/func \(svc \*Service\) callerMaySubnetNetwork/,/^}/' internal/neutron/network.go | grep -c 'FROM networks'
→ 1
grep -c 'callerMaySubnetNetwork' internal/neutron/network.go
→ 2   (definition + the call in CreateSubnet)
```

### GREEN — live (two real tenants, genuine **member** tokens)

| Case | RED | GREEN |
|---|---|---|
| foreign **private** network | `201` 🔒 | **`404`** |
| foreign **shared** network | `201` 🔒 | **`403`** |
| nonexistent network id | `500` | **`404`** |
| malformed network id | `500` | **`400`** |
| tenant B on its **own** network | `201` | `201` |
| **admin** on tenant A's network | `201` | `201` |

Victim's view after both refusals: `["ok-admin"]` — the admin's legitimate subnet only, **zero**
`intruder` subnets. Audit: **2** `refused cross-tenant subnet write` warnings, one per refusal.

**The first live run appeared to fail, and finding out why mattered.** With tokens minted the way
`multi_tenant_test.sh` mints them, the cross-tenant write still returned `201`. Cause: the suite asks
`GET /v3/roles?name=_member_` and takes `.roles[0].id`. That endpoint **ignores the name filter** (the
same class of defect as `ListDomains`), so it returns all three seeded roles — and the first is `admin`.
There is also no `_member_` role at all: O3K seeds `admin`/`member`/`reader`.

So the suite had been granting **admin** to both of its test users, and an admin legitimately bypasses
the check. Every isolation assertion in that suite has been running with two administrators. Re-issued
with a genuine `member` token, the matrix above is exact. Fixed in the suite by selecting the role by
name client-side and asserting neither token carries `admin`; the server-side `?name=` gap on roles is
recorded as observation O9.

### Follow-up found in branch review (2026-08-06) — FR-013a

The victim's view above, `["ok-admin"]`, is itself the artifact. That subnet is the administrator's
legitimate creation on tenant A's network, and it belongs to the **administrator's** project, because
`CreateSubnet` attributes the row to the caller (as upstream does). Tenant A could therefore see it in
`GET /v2.0/subnets` and — until this fix — got `404` fetching it by id and could not delete it. Read
against upstream (`neutron/conf/policies/subnet.py`), `get_subnet` and `delete_subnet` both grant the
network's owner, so O3K was stricter than upstream on those two paths while its own `ListSubnets`
already matched. Closed by FR-013a; see `research.md` R5 for the policy table.

| Case | RED (pre-fix) | GREEN |
|---|---|---|
| network owner fetches a foreign-owned subnet on their network | `404` | **`200`** |
| network owner renames it | `404` | **`200`**, new name stored |
| the rename's `tenant_id` | the caller's project | **the subnet's own project** |
| network owner deletes it | `404` | **`204`**, row gone |
| stranger (owns neither subnet nor network) fetches / renames / deletes | `404` | `404`, row unchanged |
| shared network: non-owner fetches | `200` | `200` |
| shared network: non-owner deletes | `404` | `404` — upstream keeps `rule:shared` out of `delete_subnet` |

Three defects the unit tests caught that reading the code did not:

- With the entitlement query widened but `DELETE … AND project_id = $2` (and `UPDATE … AND
  project_id = $3`) left in place, a network owner's call returned `204`/`200` and changed nothing.
  Both statements now use only the id the entitlement query resolved.
- `UpdateSubnet` reported `tenant_id` from the token. That was right only while the caller was
  necessarily the owner; it now comes from the row.
- A `SELECT` gained a column while its `Scan` did not, so `GetSubnet` answered `500`. Caught
  immediately by the tests below; worth noting only because the edit that caused it looked trivial.

### GREEN — live (stub stack, this branch's binary, 2026-08-07 07:08)

Run as §2.4a of `HOST_TESTING.md`, on the §2.3/§2.4 fixtures: two tenants with genuine `member`
tokens (both decode to `["member"]`, no admin), tenant A owning `net-a` and `net-a-shared`, and the
`ok-admin` subnet an administrator placed on tenant A's private network.

| Case | Result |
|---|---|
| network owner fetches the admin's subnet by id | **`200`** |
| network owner renames it | **`200`** |
| the rename read back from the API | `renamed-by-owner` |
| `tenant_id` on that read | `…0002` — the **admin** project, not tenant A |
| tenant B fetches / renames / deletes it | `404`, `404`, `404` |
| network owner deletes it | **`204`**, gone from the listing |
| shared network: tenant B reads a subnet on it | `200`, `tenant_id` = tenant A |
| shared network: tenant B deletes it | `404` |

§2.4 itself was re-run first and is unchanged: `404, 403, 404, 400, 201, 201`, victim's list
`["ok-admin"]`, `intruder subnets: 0`, and **2** audit records naming caller, network and owner with
`network_shared` correctly `false` then `true`.

#### The host pass found a defect three unit tests had missed

The `tenant_id` row above read **tenant A** on the first run — the caller, not the owner. Cause:
`GetSubnet` and `ListSubnets` never selected `s.project_id`; they filled `tenant_id` from
`c.GetString("project_id")`. That was true-by-construction while both could only return the caller's
own rows, and stopped being true twice over — SPEC-020 gave `ListSubnets` the `n.project_id = $1`
disjunct, and FR-013a gave `GetSubnet` the same. Both were telling a network owner that another
project's subnet was theirs.

`TestNetworkOwnerMayRenameForeignSubnet` did not catch it because it asserts on the `PUT` response,
and `UpdateSubnet` had already been fixed for exactly this. The runbook line that caught it reads the
value back with a `GET`. Both handlers now select the column;
`TestForeignSubnetReportsItsOwnTenantID` asserts it on the by-id read, in the listing, and for a
caller who genuinely does own the subnet.

This is the second time in this feature that a host pass falsified something the unit suite reported
green (the first was the `_member_` role tautology in §2.4). Both were assertion-shape problems, not
logic problems: a test that reads the wrong field cannot see the right answer.

---

## B2 — project-scoped volume routes

### RED (captured 2026-08-04, base code)

**Static** (T007): `GET /volumes` present in the `/v3/:project_id` group → `0`. The three shadowed
sub-resources → `0`. Backups routes in that group → `0`.

**Live**, and it confirms the R6 shadowing analysis empirically:

| Path (`/v3/{project}/…`) | Status | Body | Classification |
|---|---|---|---|
| `volumes` | **`405`** | `badMethod`: "The method GET is not allowed for this resource." | the ticketed defect — `POST` registered, `GET` not |
| `volumes/detail` | **`404`** | `itemNotFound`: "**volume** could not be found." | **shadowed** — served as a lookup for a volume named `detail` |
| `snapshots/detail` | **`500`** | `computeFault`: "failed to get **snapshot**" | **shadowed** |
| `types/default` | **`500`** | `computeFault`: "failed to get **volume type**" | **shadowed** |
| `backups/detail` | `404` | `itemNotFound`: "**Resource** could not be found." | **absent** — no route matched |

The **message text** is the proof, not the status code. The three shadowed paths return
*resource-specific* messages produced by a handler that actually ran; `backups/detail` returns the
generic "Resource could not be found." from the no-route case — so the *method* of proving shadowing
by message text is sound, and `backups/detail` really is absent.

R6's claim of "three shadowed, not four" was **not** confirmed by this, though it reads that way: the
four paths probed here were the four R6 named. Probing only the candidates on a hand-written list
cannot discover one that is missing from it. See the correction below.

Two of the three are **`500`s**, worse than the specification assumed and precisely the #533 F8 failure
mode (a handler running against a resource id that is really a sub-resource name).

Flat form for comparison: `volumes` `200`, `volumes/detail` `200`, `types/default` `200`.

### GREEN — unit level (verified against **this** build)

`go test ./internal/cinder/` — 5 tests, all pass:

- `TestProjectScopedVolumeList` — the collection answers, contains the caller's volume, and returns the
  **same** set as the flat form (SC-005). Naming a foreign project, or a non-identifier, in the path
  changes nothing (FR-015).
- `TestProjectScopedVolumeListEmpty` — `200` with an empty list, never `null`.
- `TestProjectScopedSubResourcesNotShadowed` — all three shadowed views return their own view, and each
  matches its flat twin byte-for-byte.
- `TestProjectScopedBackupsDetailStaysAbsent` — still `404`, guarding the R6 finding against someone
  "fixing" a shadowing bug that isn't one.
- `TestRouteRegistration_NoWildcardConflict` — extended into a no-shadow guard over the project-scoped
  group, and it now **fails** if `backups/detail` is added (untested surface, FR-016a).

**Static**: `GET /volumes` present in the group → `1`; the three shadowed names → `3`.

### Correction — a fourth shadowed route (o3k#82 review, 2026-08-27)

`GET /v3/{project}/os-volume-transfer/detail` is shadowed too, and was filed among the "merely absent"
routes. The project group registers `GET /os-volume-transfer/:id` (`volumes.go:282`), so the literal
`detail` binds to `:id` and `GetVolumeTransfer` runs:

```
GET /v3/{project}/os-volume-transfer/detail
  before → 500 {"computeFault":{"code":500,"message":"failed to get transfer"}}
  after  → 200 {"transfers":[…]}
```

That `500` is the same signature as `snapshots/detail` and `types/default` above, and as #533 F8.
Registered now, with the case added to `TestProjectScopedSubResourcesNotShadowed`; RED proven by
deleting the route and re-running (the failure above *is* that run).

**How it was missed, and how it was found.** R6 correctly replaced the "flat-vs-project difference"
test with "does a `:id` sibling exist at the same level" — but applied it to a five-name candidate list
that never included `os-volume-transfer`. It was found by probing *every* absent route against the real
route tree instead of reasoning about the list. Census: **four shadowed, 14 merely absent**.

The `os-volume-transfer` **collection** (`GET`, POST-registered but not GET → `405`) is the same shape
as the ticketed B2 defect and stays in the absent set for the follow-up ticket, deliberately not
widened into this change.

### GREEN — live

| Path (`/v3/{project}/…`) | RED | GREEN |
|---|---|---|
| `volumes` | `405` | **`200`** |
| `volumes/detail` | `404` (by-id lookup) | **`200`** |
| `snapshots/detail` | `500` | **`200`** |
| `types/default` | `500` | **`200`** |
| `backups/detail` | `404` | `404` — **unchanged by design** (FR-016a) |

`diff` of `.volumes` between the flat and project-scoped forms: **IDENTICAL** (SC-005). A caller naming
a foreign project in the path sees the same collection as when naming their own (FR-015).

---

## Verification harness

### RED (captured 2026-08-04, base code)

`bash test/multi_tenant_test.sh` → exit 1, and the ticket's exact symptom:

```
Created PROJECT1=null, USER1=null
Created PROJECT2=null, USER2=null
```

It then ran **six** test sections against tenants that do not exist, reported `Passed: 0 / Failed: 5`,
and its cleanup emitted `jq: error … Cannot iterate over null`, six `401 authentication required`
responses and four `Could not find user/project: null.` responses.

The suite failing here is incidental, not a defence: it fails only because *everything* fails. FR-017's
point is that no check should have run at all.

**FR-020 verified** (T009) — all three suites repaired in the #534 review read the token from the
header, with zero references to the non-existent body field:

| Suite | `X-Subject-Token` refs | `.token.token` refs |
|---|---|---|
| `test/multi_tenant_test.sh` | 3 | 0 |
| `test/performance_test.sh` | 1 | 0 |
| `test/vxlan_multinode_test.sh` | 2 | 0 |

### GREEN — static (code written, not yet exercised)

`test/multi_tenant_test.sh` now has: a `require_value` guard rejecting empty **and** the literal `null`;
a `require_status` guard for the two role assignments (requiring `204`, the status `AssignRole` returns);
guards on all nine setup values; and teardown moved into an `EXIT` trap registered before the first
resource is created, preserving the suite's own exit code. Checks rose from 26 to 32: the cross-tenant
subnet write (with the victim's view and the own-network control), and the project-scoped volume list
(with the foreign-project scoping control). `bash -n` clean.

### GREEN — live

`make test-multi-tenant`: **13 passed, 0 failed**, twice in a row with no manual cleanup between runs
(SC-006a), and again immediately after a deliberately aborted run. Was: `PROJECT1=null`, six sections run
against nothing, `Passed: 0 / Failed: 5`.

The five checks this feature added all pass against real tenants:

```
✓ User2 cannot add a subnet to User1's network (HTTP 404)
✓ User1's subnet list shows no trace of the refused write
✓ User2 can still add a subnet to their own network
✓ User1 can list volumes on the project-scoped URL form
✓ The project segment in the URL does not widen what User2 can see
```

**Guards fire (T031)**: with a deliberately invalid admin token the suite exits **1** with
`SETUP FAILED: could not create project test-project-1 (got 'null')` and reports **0** passes.

**Two further harness defects found and fixed while verifying** — both of the same family as T1, an
assertion that cannot read what it judges:

1. **The role bug** (see B8 above): `admin` granted instead of `member`, making every isolation
   assertion a tautology.
2. **A false FAIL** in the cross-project volume-attach check. It read
   `jq -r '.error // .badRequest // "success"'`, but O3K refuses that attach with `{"itemNotFound": …}` —
   neither key — so jq fell through to the literal `"success"` and the suite reported an ISOLATION BREACH
   that does not exist. Verified directly: the attach returns `404`, correctly refused. Both checks now
   assert on the HTTP status, which cannot be misread.

---

## `test/horizon_compat_test.sh` (T007a)

### RED (captured 2026-08-04, base code)

Exit 1, failing at **`[FAIL] Cinder - List volumes`** — line 131, `GET /v3/${PROJECT_ID}/volumes`, the
`405`. Preceding checks pass (authentication, 6-service catalog, Nova servers, Nova flavors, Neutron
networks).

**Finding: the suite cannot report more than one failure.** It aborts at the first one, so the second
B2 check at line 210 (`GET /v3/${PROJECT_ID}/volumes/detail`) is **never reached**. Cause:

```
set -e                      # line 5
print_fail() { echo …; ((FAILED++)); }
```

`((FAILED++))` evaluates to the pre-increment value. When `FAILED` is `0` the arithmetic expression
yields 0, which bash reports as exit status **1**, and `set -e` then terminates the script. So the
first failure is always fatal and the summary can never show more than the checks up to it.

This is the same family of defect as T1 — a suite that cannot report honestly — and it blocks SC-008's
"goes from failing to passing" from being verifiable beyond the first check. Fixed as part of this
feature; see the note in the GREEN section.

### GREEN — the counter bug is fixed

`print_pass` / `print_fail` now use `VAR=$((VAR + 1))`. The suite can report every failure instead of
dying on the first, which is what makes SC-008's "goes from failing to passing" observable at all.

The same `((VAR++))` + `set -e` pattern exists in **8 other suites** — recorded as observation O7 for a
follow-up rather than fixed here.

### GREEN — live

**exit 0 · Total Passed: 18 · Total Failed: 0.** Was: exit 1 after 5 passes, dead at the first failure.

Both previously failing volume checks pass — `Cinder - List volumes` (line 131, the `405`) and the
Volumes-tab check (line 210). SC-008 is met in the strong sense: the suite went from **failing to
passing**, not merely from unchanged to unchanged.

---

## Superseded: the port constraint that blocked the live half

_Resolved. The user stopped the leftover #534 deployment, freeing the standard ports; this branch's
binary was then run on them for every result above. Recorded for the record, and because observation O6
stands on its own:_

- A pre-existing O3K from #534's host validation had held `35357/8774/9696/8776/8778` since 06:35. Being
  base code, it was a good source of RED evidence and useless for GREEN.
- A second instance could not run alongside it: `cmd/o3k/main.go:715` hardcodes placement's `:8778` — the
  only service whose port is not read from config — and `main.go:300` calls `log.Fatalf` on any bind
  failure (observation O6).

**Correction to an earlier claim in this log**: the contract suites target `localhost:35357`, so the
`go test ./test/contract/keystone/...` run recorded mid-implementation validated the *base-code server*,
not this change. That claim was withdrawn and the suite has now been re-run properly against this
branch's binary — see the gate table below.

## Outstanding

Everything above marked "unit level" is verified against **this** build. The live half is not, and cannot
be from here:

- A pre-existing O3K from #534's host validation has held `35357/8774/9696/8776/8778` since 06:35 today.
  It is **base code**, which made it a good source of RED evidence but useless for GREEN.
- A second instance cannot be started alongside it: `cmd/o3k/main.go:715` hardcodes placement's `:8778`
  (the only service whose port is not configurable) and `main.go:300` does `log.Fatalf` on any bind
  failure. An isolated stack on alternate ports was prepared — `/tmp/o3k-545.yaml`, database `o3k_545`,
  migrated to 65 — and gets as far as placement before dying (observation O6).
- The bash suites hardcode `localhost:35357` (14 occurrences in `multi_tenant_test.sh`, 1 in
  `horizon_compat_test.sh`) with no environment override, so they need those exact ports.

**Correction to an earlier claim in this log**: the contract suites also target `localhost:35357`, so the
`go test ./test/contract/keystone/...` run recorded during implementation validated the *base-code
server*, not this change. It is not evidence for this branch and T026 has been un-ticked.

A nova contract test (`TestMigrateServer_Contract`, expected `202` got `409`) also fails against that
server. It is not attributable to this change: the server predates these edits by three hours and nothing
here touches Nova.

## Summary

| Defect | RED | GREEN (unit / static) | GREEN (live) |
|---|---|---|---|
| B5 / P1 | ✅ live, 4 probes | ✅ 12 tests | ✅ 6 probes |
| B8 🔒 | ✅ static + unit (rows 2/3 returned `201`) | ✅ 17 assertions | ✅ 6-case matrix + no residue + 2 audit records |
| B8 aftermath (FR-013a) | ✅ unit (owner got `404` on fetch, rename and delete) | ✅ 4 tests, 12 cases | ✅ §2.4a, 9 probes + the shared-network split |
| B2 | ✅ static + live, 5 paths | ✅ 5 tests | ✅ 5 paths + identical-collection diff |
| Harness | ✅ live (`PROJECT1=null`) | ✅ `bash -n` clean | ✅ 13/13 twice; guards fire with 0 passes |
| `horizon_compat_test.sh` | ✅ live (exit 1 after 5) | ✅ counter bug fixed | ✅ exit 0, 18/18 |

**Four for four, plus the horizon suite** (SC-007), and FR-013a is now live too.

### Branch-review addendum — harness teardown re-verified live (2026-08-07)

The review found that the suite's new "own network" check creates a real subnet on `project2-network`
but never recorded its id, so teardown deleted the *network* while a subnet still sat on it —
`DeleteNetwork` refuses that with a `409` (`network has subnets; delete subnets first`), swallowed by
`-o /dev/null … || true`. The database survived only because deleting the project cascades; in real
mode the subnet's bridge and dnsmasq would leak on every run.

Fixed, and proved from the server's own log across two consecutive runs — the teardown now deletes the
subnet first and the network delete succeeds:

```
DELETE /v2.0/networks/bbc8f19d… -> 204     (project1-network, no subnet)
DELETE /v2.0/subnets/984c7619…  -> 204     (project2-subnet — this call did not exist before)
DELETE /v2.0/networks/d906648a… -> 204     (project2-network, formerly a 409)
```

`make test-multi-tenant`: **13 passed / 0 failed, twice back to back, exit 0**, with no manual cleanup
between runs (FR-017a). Residue afterwards: `0` leftover subnets, `0` networks, `0` `test-project-%`
projects. All five checks this feature added to the suite report `✓`, including the two that would
have caught B8 and B2.

### Gates on this build

| Gate | Result |
|---|---|
| `make test` | 0 failing packages |
| `make lint` | **0 issues** |
| `go build ./...` | clean |
| `make test-integration` | **28 passed, 0 failed** |
| `make test-multi-tenant` | **13 passed, 0 failed** (×2, plus once after an abort) |
| `test/horizon_compat_test.sh` | **18 passed, 0 failed** (was failing) |
| `go test ./test/contract/keystone/...` | **ok** — the A2/FR-007 guard, against this binary |
| `go test ./test/contract/{cinder,neutron,nova,.}/...` | **ok** |
| `go test ./test/contract/glance/...` | 1 failure — `TestGlanceDownloadImageData_Contract` returns 33 zero bytes because this run configures `glance.storage_mode: stub` per A8. It needs `local`, hence root-owned `/var/lib/o3k/images`. Not a code defect and unrelated to this change |
| `git status migrations/` | empty — **no migration** (A9) |
| `go.mod` / `go.sum` | untouched — **no new dependency** |
| Production files changed | 7 |

**Verification configuration**: `cinder.storage_mode` and `glance.storage_mode` set to `stub` for this
run (A8 sanctions stub mode). `local` requires `/var/lib/o3k/{volumes,images}`, which needs root — the
directory does not exist on this host and `paths.Base` is a compile-time constant with no override.

**Gates on this build**: `make test` 0 failures · `make lint` 0 issues · `go build ./...` clean ·
`git status migrations/` empty (no migration) · `go.mod`/`go.sum` untouched (no new dependency) ·
7 production files changed.
