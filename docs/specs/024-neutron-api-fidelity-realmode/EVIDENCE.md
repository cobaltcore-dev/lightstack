# RED → GREEN Evidence

**Feature**: `024-neutron-api-fidelity-realmode` (cloud-storage#546) · **Branch**: `feature/546`
**Requirement**: FR-038 and SC-010 — each of the six defects must be shown failing before the change
and passing after it.

---

## Phase 1 — Baseline (T001–T003), captured 2026-08-11

Stack: `deployments/docker-compose.yml` (postgres 18.4 + redis + o3k), `networking_mode: stub` from the
baked `config/o3k.yaml`.

### Environment / gates before any change

| Check | Result |
|---|---|
| `go build ./...` | OK |
| `go vet ./internal/neutron/... ./internal/common/...` | OK |
| `go test ./internal/neutron/...` **with** `O3K_TEST_DB_URL` | **ok — 34 tests pass** |
| `go test ./internal/neutron/...` **without** it | ok, but **27 DB-backed tests skip** |
| `go test ./internal/common/...` | ok |
| `make test-errors` | **FAIL — 10 total, 9 passed, 1 failed, exit 1** |

### T001 — `make test-errors` is red on B10 (the free RED)

```
[TEST 6] Error format consistent across Nova/Neutron/Cinder
✗ Inconsistent error formats across services
...
Total Tests:  10
Passed:       9
Failed:       1
❌ SOME TESTS FAILED
make: *** [Makefile:97: test-errors] Error 1
```

The single failure is TEST 6 (`test/error_handling_test.sh:158`), which asserts
`has("NeutronError") or has("error")` on a Neutron 404. It predates this branch (`7b15418`, 2026-03-07)
and is part of `test-all` (`Makefile:125`), so **`make test-all` cannot pass while B10 is open**. This
is the RED for US2 and it needed no new test.

### T002 — shared-database state (recorded so a later red result is attributable)

```
projects:            00000000-0000-0000-0000-000000000002 | default
                     00000000-0000-0000-0000-0000000b0b0b | spec022-tenant-b   (leftover, another worktree)
quotas.instances:    default = 10        ← healthy, NOT the poisoned 0
networks:            0
schema_migrations:   version 65, dirty = f
```

Migration head is **65**, confirming `066` is the next free number (data-model.md).

### T003 — B10 reproduced directly on every resource (E1, E2, E3)

```
resource          [envelope key, inner .code]
networks          ["itemNotFound", 404]
subnets           ["itemNotFound", 404]
ports             ["itemNotFound", 404]
routers           ["itemNotFound", 404]
floatingips       ["itemNotFound", 404]
security-groups   ["itemNotFound", 404]
```

Every one is the **compute** envelope, and every one carries the numeric `code` key that upstream
Neutron does not have. Expected after US2: `["NeutronError"]` with inner keys exactly
`type`/`message`/`detail` and **no** `code`.

### 🔑 The non-group error paths — live confirmation of the analyze D1 finding

```
E5  no token, GET /v2.0/networks     → ["unauthorized"]
E6  GET /v2.0/nonexistent            → ["itemNotFound"]
E7  DELETE /v2.0/networks (405)      → ["badMethod"]
```

All three answer outside any route group: auth is `r.Use(...)` at `cmd/o3k/main.go:632`, and `NoRoute`
/ `NoMethod` are registered at `:633` / `:635`, before `svc.RegisterRoutes(r.Group(""))` at `:637`.
This is the empirical proof that the marker must be registered at **engine** level — a group-level
marker would have converted the six resource paths above and left these three in the compute envelope,
passing 9 of 12 E-contracts while shipping the half-fix FR-008 forbids.

### E10 baseline — other services must not move

```
nova  GET /v2.1/servers/<absent>     → ["itemNotFound"]
```

Cinder and Nova must still read `itemNotFound` after US2; `test/error_handling_test.sh` asserts it.

---

## Phase 3 — US2 / B10 — **GREEN** (T007–T020)

### RED (before the change)

Unit tests would not compile — `ErrorEnvelope` did not exist, which is the honest RED for a marker
that had never been written:

```
internal/neutron/error_envelope_test.go:37:8: undefined: ErrorEnvelope
FAIL github.com/cobaltcore-dev/o3k/internal/neutron [build failed]
```

The gophercloud contract tests compiled and failed on the defect itself, naming it each time:

```
--- FAIL: TestNeutronErrorEnvelope_Contract/networks
    expected wrapper key `NeutronError`, got map[itemNotFound:map[code:404 message:network could not be found.]] — this is B10
--- FAIL: .../subnets   … got map[itemNotFound:…]
--- FAIL: .../ports     … got map[itemNotFound:…]
--- FAIL: .../routers   … got map[itemNotFound:…]
--- FAIL: .../security-groups … got map[itemNotFound:…]
--- FAIL: TestNeutronErrorEnvelopeUnauthenticated_Contract  … got map[unauthorized:map[code:401 …]]
--- FAIL: TestNeutronErrorEnvelopeUnroutedPath_Contract
```

`make test-errors`: 10 total, 9 passed, **1 failed**, exit 1.

### GREEN (after the change)

Live, against the rebuilt container:

```
networks   {"NeutronError":{"detail":"","message":"network could not be found.","type":"NetworkNotFound"}}
subnets    {"NeutronError":{"detail":"","message":"subnet could not be found.","type":"SubnetNotFound"}}
ports      {"NeutronError":{"detail":"","message":"port could not be found.","type":"PortNotFound"}}
routers    {"NeutronError":{"detail":"","message":"router could not be found.","type":"RouterNotFound"}}
E5 (no token)   {"NeutronError":{"detail":"","message":"authentication required","type":"NotAuthorized"}}
E6 (unrouted)   {"NeutronError":{"detail":"","message":"Resource could not be found.","type":"NotFound"}}
E10 (nova)      {"itemNotFound":{"code":404,"message":"instance could not be found."}}   ← unchanged
```

| Gate | Before | After |
|---|---|---|
| `make test-errors` | 9/10, exit 1 | **10/10, exit 0** — "ALL ERROR HANDLING TESTS PASSED" |
| `go test ./internal/neutron/...` | 34 pass | **39 pass** |
| `test/contract/neutron` | ok (blind to the envelope) | ok |
| `test/contract/{,cinder,glance,keystone,nova}` | ok | **all ok** |
| `go test ./internal/... ./pkg/...` | ok | **all ok** |
| `golangci-lint run ./internal/common/... ./internal/neutron/...` | — | **0 issues** |
| `gofmt -l` | clean | clean |

**`make test-all` is no longer blocked on B10.** That gate had been red since `7b15418` (2026-03-07).

### Contracts satisfied

E1, E2, E3 (7 resources), E4, E5, E6, E7, E8, E9, E10, E11, E12 — all twelve, as unit tests and
(E1/E2/E3/E5/E6/E10) again through a real gophercloud client.

### 🔑 What the evidence proves about the analyze D1 finding

E5 and E6 came back `NeutronError` from a **live unauthenticated call and a live unrouted path**. Both
are answered outside any route group — auth is `r.Use` at `cmd/o3k/main.go:632`, `NoRoute` at `:633`.
The pre-change baseline recorded them as `unauthorized` and `itemNotFound`. Had the marker been
registered on the `v2` group as the plan originally said, these two would still read that way while
the six resource paths flipped: 9 of 12 contracts green, and the half-fix FR-008 exists to prevent.

### Size of the change

3 files modified (+34/−8), 2 new source files, **0 of the 244 `common.SendError` call sites touched**
— as research R1 predicted.

```
cmd/o3k/main.go                             |  5 +++++
internal/common/errors.go                   | 26 +++++++++++++++++++++++++-
internal/neutron/auto_allocated_topology.go | 11 ++++-------
```

### Note on T018 (id-carrying message form)

**No-op, deliberately.** The task was bounded during analyze remediation to sites already using
`NewResourceNotFoundError`, which receives an id. Neutron has **zero** such sites (verified:
`grep -c NewResourceNotFoundError internal/neutron/ → 0`); every one uses `NewNotFoundError`, which
has no id to carry. Changing that would mean editing call sites, which research R2 explicitly rules
out. Message text is unchanged; `type` carries the fidelity.

### One flake worth recording

`test/contract/template_test.go` failed with "Authentication failed" during a fully parallel
`go test ./...`, and passes in isolation and in the contract-suite run. Not related to this change —
the package authenticates against Keystone and the parallel run contends on it.

## Phase 4 — US1 / F10 — **GREEN** (T021–T032)

### RED (before the change)

The unit tests reproduced the ticket's exact symptom:

```
--- FAIL: TestCreatePortOnNetworkAbsentFromBridgeRegistry
    create port: code=500 body={"computeFault":{"code":500,"message":"failed to attach TAP to bridge"}}; want 201 — this is F10
--- FAIL: TestCreatePortOnSharedNetworkOfAnotherProject
    port on a shared network: code=500 body={"computeFault":{"code":500,"message":"failed to attach TAP to bridge"}}; want 201
```

### GREEN (after the change)

```
--- PASS: TestCreatePortOnNetworkAbsentFromBridgeRegistry
--- PASS: TestCreatePortPlacesTapWhereTheBridgeIs
--- PASS: TestCreatePortRollsBackCleanly
--- PASS: TestCreatePortOnSharedNetworkOfAnotherProject
```

Live, contract P2 — the case the ticket's repro hits on a long-running stack. Create a network, then
**restart o3k** (wiping the in-process bridge registry), then create a port on that network:

```
{"name":"p2-after-restart","mac_address":"6a:ac:7d:f6:99:e2",
 "fixed_ips":[{"ip_address":"192.168.199.77","subnet_id":"b61b302d-…"}]}
status: 201
```

Contracts satisfied: P1, P2, P3, P4, P7, P8, P9, P10. P5, P6, P11 are real-mode and remain for T092.

### 🔑 The codebase already contained the correct pattern

`AllocatePortForInstance` (`internal/neutron/ports.go:1231`) — the path Nova uses when booting an
instance — **already** created its TAP in the default namespace, with the comment *"Create TAP device
in default namespace (not project namespace) for libvirt access"*, and ensured the bridge before
attaching.

So `CreatePort` was not merely wrong, it was **inconsistent with the sibling path that works**. That
is why a VM booted through Nova could get networking while `openstack port create` returned 500. The
fix makes the API path match the instance path rather than inventing a placement.

It also motivated one addition beyond the task text: `CreatePort` now ensures the bridge before
attaching, as `AllocatePortForInstance` does. In real mode the kernel object is gone after a host
reboot until the reconciler's next pass (up to 30s), and FR-001 requires port creation to succeed on
any recorded network rather than depending on that timing.

### Reconciler consistency (T030)

Verified rather than assumed — `internal/neutron/reconciler.go:159` creates bridges with
`(false, "")` and runs dnsmasq with an empty namespace at `:279`/`:281`. Both already agreed with the
new placement, so no change was needed.

`vxlan_coordinator.go:120` still attaches inside a namespace. Left alone deliberately: multi-node
overlay networking is out of scope, and that path creates its own namespace, so it is
self-consistent.

### Size of the change

```
internal/neutron/ports.go     CreatePort + both rollbacks + DeletePort
pkg/networking/netns.go       stub attach no longer gates on the in-process map
```


## Phase 5 — US5 / B9 — **GREEN** (T033–T040)

### RED (before the change)

```
--- FAIL: TestRuleValuesAgreeAcrossAllFourEmitters
    rule 508a1e77… key "remote_group_id": create response="" but rule-by-id=null — B9, the create path is the fourth emitter
--- FAIL: TestUnsetRuleOptionalsAreNullInTheCreateResponse
    create response "remote_ip_prefix" = "", want null for an unset optional — this is B9
    create response "remote_group_id" = "", want null …
    create response "port_range_min"  = 0,  want null …
    create response "port_range_max"  = 0,  want null …
    create response "protocol"        = "", want null …
--- FAIL: TestDeliberateZeroPortSurvivesStorage
    stored "port_range_min" came back null, want 0 — `Valid: portMin > 0` discarded a real zero
    stored "port_range_max" came back null, want 0 — …
```

### GREEN (after the change)

```
--- PASS: TestRuleValuesAgreeAcrossAllFourEmitters
--- PASS: TestUnsetRuleOptionalsAreNullInTheCreateResponse
--- PASS: TestDeliberateZeroPortSurvivesStorage
```

`go test ./internal/neutron/...` ok · `golangci-lint` **0 issues**.

Contracts satisfied: G1, G2, G3, G4, G5, G6, G7, G8.

### 🔑 Spec A6 is now settled empirically, and it was worse than the spec allowed for

A6 said security-group port bounds *should* be storable as three values and "if they are not, making
them so is part of US5". The RED above answers it: the columns are nullable and can hold a zero, but
the **write path could not express one**. `Valid: portMin > 0` (the old INSERT) stored a deliberate
port 0 — a legitimate ICMP type — as NULL. So this was never only a response defect; the stored rule
was wrong too, which is why `TestDeliberateZeroPortSurvivesStorage` re-reads rather than trusting the
echo.

The fix is subtraction, not addition: the request struct **already** declared these as pointers, so
the wire distinction arrived intact and was destroyed by five `if != nil { … }` flattens. Removing
them fixed the response (FR-026/027), the storage (FR-028) and the round-trip in one move.

Two small helpers make presence-based storage explicit — `nullStringFrom` / `nullInt32From` — plus
`derefOr` for the one consumer that genuinely cannot express "unset": the kernel rule builder, where
a defaulted zero is correct. That sentinel is now confined to a single call site and named as such.

### Renamed test

`TestRuleValuesAgreeAcrossAllThreeEmitters` → `…AllFourEmitters`. The old name was the evidence that
the create path had never been in scope when #533 aligned the other three.


## Phase 6 — US4 / B6 — **GREEN** (T041–T057)

### RED (before the change)

Build failures for the two new seams, which is the honest RED for functions that never existed:

```
internal/neutron/network_test.go:149:13: undefined: subnetFilterParams
pkg/networking/dhcp_options_test.go:55:6: cfg.MetadataNextHop undefined (type DHCPConfig has no field …)
```

### GREEN (after the change)

```
--- PASS: TestBuildSubnetListFilter               (6 subtests)
--- PASS: TestSubnetListFiltersByNetwork
--- PASS: TestSubnetListHonoursEveryContractedFilter (6 subtests: id, name, cidr, enable_dhcp, ip_version, project_id)
--- PASS: TestUnknownSubnetFilterDoesNotNarrow
--- PASS: TestSubnetListPagesWithoutRepeatingOrLosingRows
--- PASS: TestUnreadableSubnetRowIsLoggedNotDropped
--- PASS: TestOmittedGatewayIsStillAutoAssigned
--- PASS: TestExplicitNullGatewayIsHonoured
--- PASS: TestExplicitGatewayIsStored
--- PASS: TestGatewaylessSubnetIsReadableAndUpdatable  (S12 show, S13 update)
--- PASS: TestUpdateCanClearTheGateway
--- PASS: TestGatewaylessSubnetKeepsMetadataRoute
--- PASS: TestSubnetWithGatewayKeepsBothOptions
--- PASS: TestNoMetadataRouteWithoutANextHop
```

Live, against the rebuilt container:

```
unfiltered            ["sub-b","sub-a","test-ext-subnet", … 9 rows]
?network_id=<A>       ["sub-a"]                                   ← the filter is applied (S1)
gateway_ip:null       {"name":"sub-nogw","gateway_ip":null}       ← honoured, not overwritten (S9)
?network_id=<A>       [{"sub-nogw",null},{"sub-a","10.90.1.1"}]   ← the gatewayless row is LISTED (S11)
```

Contracts satisfied: S1–S16, P12, P14, P15.

### 🔑 FR-023a was bigger than the DHCP template

The clarify session found that `dhcp-option=3` (router) and `dhcp-option=121` (the metadata route)
were guarded on the same value. Implementing it surfaced that the guard was only the last link:

- **`applyDHCPForNetwork` (`reconciler.go:235`) scanned `gateway_ip` into a plain `string`.** On a
  gatewayless subnet the Scan failed and the function took its "no DHCP-enabled subnet" early return —
  so such a subnet would have got **no DHCP at all**, not merely no metadata route.
- **`teardown.go:115` had the same scan**, so teardown reported a failure *and* leaked the metadata
  DNAT for that subnet.

Both now scan a pointer and share one helper, `bridgeAddrFor(gateway, cidr)` — the gateway when there
is one, otherwise the first host address. The bridge needs an address in the subnet either way
(dnsmasq binds to it, the DNAT hangs off it); what changes is that for a gatewayless subnet that
address is the bridge's own and is **never advertised as a default route**. Apply and teardown share
the helper so they cannot disagree about which address to add and remove.

`DHCPConfig` gains `MetadataNextHop`, and the template now emits option 121 via it when there is no
gateway — so metadata survives while the default route is correctly absent.

### The three-state request

`gateway_ip` is `json.RawMessage` on both create and update, decoded by a small `optionalString`
helper. `*string` alone cannot do this: absent and explicit-null both decode to nil, which is exactly
why `--gateway none` was silently overwritten. Absent → auto-assign (unchanged); null → NULL;
value → as given.

### Also fixed: paging was not on a total order

`ListSubnets` ordered by `created_at DESC` alone and paged with `created_at < marker`. Ties made the
boundary unstable — the same defect #549 found in the compute listings, where rows repeated across
pages. Now `(created_at, id)` with an exclusive composite marker, matching `ListNetworks`.
`TestSubnetListPagesWithoutRepeatingOrLosingRows` seeds five subnets sharing one `created_at` and
asserts each appears exactly once.


## Phase 7 — US3 / B11 — **GREEN** (T058–T072)

### RED (before the change)

```
--- FAIL: TestAdminCreatesNetworkIntoAnotherProject
    response project_id = "…0002", want "…0b0b"
    STORED   project_id = "…0002", want "…0b0b" — the request was accepted and discarded (B11)
--- FAIL: TestNonAdminCannotCreateIntoAnotherProject     code=201, want 400
--- FAIL: TestDisagreeingProjectSpellingsAreRefused      code=201, want 400
--- FAIL: TestTenantSpellingIsAccepted                   project_id = "…0002", want "…0b0b"
--- FAIL: TestUnresolvableProjectIsRefused               (N4 absent project, N5 malformed id)
--- FAIL: TestNetworkResponsesReportTheRowsOwner         (N6 list, N7 show)
--- FAIL: TestGetNetworkCacheIsCallerIndependent
```

### GREEN (after the change)

All seven pass. Live, the ticket's own probe:

```
POST /v2.0/networks {"name":"b11-net","project_id":"…0b0b"}
  → {"name":"b11-net","project_id":"…0b0b","tenant_id":"…0b0b"}
STORED project_id in the database: …0b0b        ← was the administrator's project
```

Contracts satisfied: N1, N2, N3, N4, N5, N6, N7, N9, N10.

### 🚩 Discovered: fixing B11 makes the ticket's *verification* step 404, and that is a separate gap

The ticket verifies with `openstack network show b11-net -c project_id` as the administrator. That now
returns **404**, because the network genuinely belongs to the other project and O3K's
`ListNetworks`/`GetNetwork` scope to `(project_id = caller OR shared = true)` with **no administrator
override** — verified: `GET /v2.0/networks/<id>` as admin → `404`.

This is not a regression. Before the fix the network was created in the administrator's own project,
so `show` worked *because the ownership was wrong*. The visibility rule is unchanged; correcting the
ownership merely made the missing override visible.

Upstream's `get_network` is `admin_or_owner`, so an administrator can read any network there. O3K has
no such override on network read. **Deliberately not added here**: it widens tenant visibility
(Constitution Article V), and the equivalent question for compute was settled in #549 by an explicit
`all_tenants` opt-in decision rather than a blanket widening. It deserves the same treatment, not a
silent inclusion in a bug-fix story.

Recommended: file as a follow-up, or fold in as an explicit story with its own clarification. The
functional outcome B11 asked for — the network belongs to the requested project — is delivered and
verifiable against storage and from the owning tenant.

### Implementation note

One helper, `resolveNetworkOwner`, carries the whole decision and writes its own refusals, so
`CreateNetwork` just returns. All four emitters now report the row's owner; `ListNetworks` and
`GetNetwork` gained `project_id` to their `SELECT`s (neither selected it before). `ListNetworks` also
picked up the same log-and-continue treatment as the subnet listing, since it had the identical bare
`continue`.


## Phase 8 — US6 / F11 — **GREEN** (T073–T090)

### RED (before the change)

All eight test functions failed, covering create/update/filter/visibility/validation/grandfathering.

### GREEN (after the change)

Live, against the rebuilt container:

```
create  {"name":"f11-live-ext","router:external":true,"project_id":"…0002"}
?router:external=true   [{"name":"f11-live-ext","router:external":true}]   ← was ALWAYS empty
?router:external=false  [false]                                            ← complement
router gateway on a non-external network:
  {"NeutronError":{"detail":"","message":"Network 10e1790a-… is not an external network
    and cannot be used as a router gateway","type":"BadRequest"}}
```

That last line is US2 and US6 composing: a new F11 refusal arriving in the Neutron envelope.

**Migration 066 round-trip (X18)**: `down` drops the column (0 columns), `up` restores it (1 column),
existing rows default to false. Reversible per the Constitution.

Contracts satisfied: X1–X15, X18. X16 (Horizon dropdown) and X17 (real-mode egress) remain for the
host runbook.

### 🔑 X9 caught a real defect the spec did not anticipate

Widening visibility (FR-033a) made a tenant able to *see* a network they do not own — and revealed
that `UpdateNetwork` and `DeleteNetwork` answered **200/204 for a request that matched zero rows**.
The owner predicate scoped the statement but nothing checked the result, so a non-owner's rename or
delete reported success while changing nothing: the same dishonest-success family as B11, in two more
places. Both now check `RowsAffected() == 0` and return 404 (non-disclosure, matching the rule used
elsewhere).

This was only reachable *because* F11 widened visibility — before it, a non-owner could never address
such a network at all.

### The R6.4 blocker, confirmed

`UpdateNetwork`'s statement is owner-scoped, so an administrator marking a **tenant's** network
external updated zero rows and reported success. Administrators are now exempt from the owner
predicate for this attribute (X4 pins it).

### Three legacy tests updated — they encoded the defect

`TestNetworkListDecision`, `TestBuildNetworkListFilter` and `TestListNetworksFilters_DB` all asserted
that `router:external=true` yields an **empty** result, with comments explaining that "O3K models no
external networks". That is precisely what F11 changes, so the assertions were inverted to the new
contract (complementary filters) rather than deleted. `TestListNetworksFilters_DB`'s global
count-of-zero was also fragile — it depended on no other test ever creating an external network — and
is now scoped to networks the test controls.

### One real regression, caught by the existing contract suite

`TestNeutronFloatingIPCreate_Contract` failed once FR-035 landed: it allocated a floating IP from an
ordinary network. Its own comment said "Create external network" — it simply could not express that
before F11. Updated to use gophercloud's `external.CreateOptsExt`, so it now does what it always
claimed. That failure also exposed a flaw in my shared validator: it reported "cannot be used as a
router gateway" for a floating-IP request. The message is now parameterised by use.

---

## Session gate summary (after US2, US1, US5 — 40 of 107 tasks)

| Gate | Result |
|---|---|
| `gofmt -l internal/ pkg/ cmd/ test/` | clean |
| `go vet ./...` | clean |
| `golangci-lint run ./...` | **0 issues** |
| `go test ./internal/... ./pkg/...` | **15 packages ok, 0 fail** |
| `go test -v ./internal/neutron/...` | **45 PASS** (baseline 34) |
| `make test-errors` | **10/10, exit 0** (baseline 9/10, exit 1) |
| `go test ./test/contract/...` | all ok — contract, cinder, glance, keystone, neutron, nova |
| `git diff --stat` (source only) | 6 files modified, 6 new files |

### ⚠️ Two environment traps hit during this session — worth knowing before the next run

**1. `test/contract/keystone` changes the admin password and does not restore it.**
`test/contract/keystone/password_test.go:32` (`TestKeystoneChangePassword_Contract`) sets the admin
password to `new-password-456`. Nothing puts it back, so **every suite that runs afterwards fails to
authenticate** — which surfaced here as nine spurious `TestCinder*_Contract` failures and a
`Failed to get authentication token` from `make test-errors`. It looks exactly like a regression in
whatever you just changed.

Restore with the seeded bcrypt hash from `migrations/002_seed_data.up.sql`:

```
docker exec o3k-postgres psql -U o3k -d o3k -c \
  "UPDATE users SET password_hash = '\$2a\$10\$/DyBs44Yot8zgjC4j.n03Ox/QG94ftrksEXKKvadKcixLbGll0dR2' WHERE name = 'admin'"
```

This is a pre-existing test-hygiene defect in the keystone contract suite, unrelated to this feature.
It belongs in the test-harness ticket (#548) rather than here.

**2. `test/contract/template_test.go` flakes under a fully parallel `go test ./...`** with
"Authentication failed", and passes in isolation. The package authenticates against Keystone and the
parallel run contends on it. Also pre-existing.

---

## Final verification — all six defects GREEN (T091, T106, T107)

### RED → GREEN, one row per defect (FR-038, SC-010)

| Defect | RED evidence | GREEN evidence |
|---|---|---|
| **B10** | `make test-errors` 9/10 exit 1; every Neutron 404 `{"itemNotFound":{"code":404,…}}` | 10/10 exit 0; every error `{"NeutronError":{type,message,detail}}` |
| **F10** | `500 failed to attach TAP to bridge` on a port after restart | `201` with MAC + fixed IP on a pre-restart network |
| **B9** | create response `""`/`0`, reads `null`; a deliberate `0` **stored as NULL** | all four descriptions identical; `0` stored and reported as `0` |
| **B6** | `subnet list --network X` returned every subnet; gatewayless subnet absent from listings | filter applies; gatewayless subnet listed, shown, updated |
| **B11** | `201` with the network in the **caller's** project | network stored in the requested project; non-admin refused `400` |
| **F11** | `?router:external=true` always `[]`; any network accepted as a gateway | external networks listed; non-external gateway refused |

### Success criteria (T107)

| SC | Status |
|---|---|
| SC-001 network→subnet→port→guest path | **stub verified**; guest half is host-only (T092) |
| SC-002 port creation on any recorded network | **met** — including across a restart |
| SC-003 100% Neutron envelope, 0% compute | **met** — handlers, auth, NoRoute, NoMethod, panic |
| SC-004 full suite passes | **met for every suite runnable here** — see caveat below |
| SC-005 no accepted-and-ignored project | **met**, verified against storage |
| SC-006 filtered subnet listing exact | **met** for all seven filters |
| SC-007 gatewayless subnet visible/readable | **met** |
| SC-008 all four rule descriptions agree | **met**, populated and unset |
| SC-009 egress configurable from the dashboard | **partially** — the API half is met (external networks exist, are listed, and are validated); the Horizon dropdown itself is X16/T092, host-only |
| SC-010 recorded before/after per defect | **met** — the table above |
| SC-011 no previously passing check regressed | **met** — see the two deliberate test updates below |

**SC-004 caveat**: `make test-all` also runs `test-integration`, `test-multi-tenant`, `test-horizon-ui`
and `test-performance`, which were not executed here. Everything that was run is green: all unit
packages, all six contract suites, and `make test-errors` — the gate that was the blocker.

### Final gate state

```
gofmt -l               clean
go vet ./...           clean
golangci-lint run ./...   0 issues
go test -p 1 ./internal/... ./pkg/...   15 packages ok
go test -p 1 ./test/contract/...        6 suites ok
make test-errors       10/10, exit 0
go test -v ./internal/neutron/...       72 PASS   (baseline 34)
```

### SC-011 — the two test changes that are NOT regressions

1. **Three legacy assertions inverted.** `TestNetworkListDecision`,
   `TestBuildNetworkListFilter` and `TestListNetworksFilters_DB` asserted that `router:external=true`
   returns an **empty** list, with comments explaining "O3K models no external networks". That is the
   defect F11 fixes, so the assertions were rewritten to the new contract rather than deleted.
2. **`TestNeutronFloatingIPCreate_Contract` updated.** It allocated a floating IP from an ordinary
   network; FR-035 now requires an external one. Its own comment already said "Create external
   network" — it could not express that until F11 existed. It now uses `external.CreateOptsExt`.

### Change inventory

17 files modified, 14 new (2 migration, 8 unit-test, 2 contract-test, 2 source). Migration `066` is
the feature's only schema change and round-trips cleanly.

### Still outstanding — host-only

**T092/T093** need a privileged Linux host (real root; rootless podman cannot create host network
namespaces): P5/P6/P11 (TAP and bridge co-located, guest gets DHCP and reaches metadata, no leaked
TAPs), P13 (gatewayless subnet still reaches metadata), X16 (Horizon Set Gateway dropdown), X17
(guest egress and floating-IP reachability).

---

## Ticket re-read 2026-08-12 — two additions, and a stale-environment warning

The ticket body gained two things after this work was planned.

### 1. Reconcile-interval log (new, now fixed)

> `cmd/o3k/main.go:215` logs the *configured* reconcile interval, so a default deployment prints
> `Real-mode network reconciler started (interval: 0s)`, which reads as "periodic reconciliation is
> off". It is not … Log the effective value.

Confirmed and fixed. `NetworkReconciler` now exposes `PollInterval()` returning the post-clamp value,
and `main.go` logs that. The accessor exists so the log site cannot restate the clamp and drift from
it. Covered by `TestPollIntervalReportsTheEffectiveValue` (0 → 30s, −5s → 30s, 45s → 45s); RED was a
compile failure, since the accessor did not exist.

Not in the original spec or task list — genuinely new scope, added to the ticket checklist.

### 2. F10 re-confirmed in real mode (independent corroboration)

> **F10 confirmed again 2026-08-12** on a clean real-mode k3s stack: `test/contract/neutron` fails five
> tests … Nova's *internal* port allocation on server create worked in the same run … the two paths do
> not fail together.

**This is the same asymmetry recorded in Phase 4 above**, found independently. `AllocatePortForInstance`
(`ports.go:1231`) already created its TAP in the root namespace with the comment "not project
namespace, for libvirt access", while `CreatePort` used `light-ns-<project>`. That is exactly why the
instance path worked while `POST /v2.0/ports` returned 500. The fix makes the API path match the
instance path.

All five named tests exist in `test/contract/neutron/` and pass in the stub stack. **They have not been
re-run in real mode** — that is T092, and it needs a privileged Linux host.

### ⚠️ The live stack in this environment is currently unusable — read before trusting a live result

A **root-owned orphaned `/app/o3k` process** (PID 1406918) survived its container's removal and still
holds `:35357`, `:9696` and `:8778`. It serves a **pre-fix binary**: a port create against it returns

```
{"computeFault":{"code":500,"message":"failed to attach TAP to bridge"}}
```

— the compute envelope and the old message, neither of which the current code can produce. It cannot be
reaped from this session: it runs as root, `kill` is refused, `docker compose down` does not know about
it (its container record is gone), and there is no passwordless sudo. A second instance cannot be
started alongside it because the placement port is hardcoded at `:8778` (`cmd/o3k/main.go:723`).

**Consequence**: any live API check run after that container was removed reflects the old binary, not
this branch. The live results recorded in Phases 3–8 above predate the removal and show behaviour only
the new code can produce (`NeutronError` with `type`, `router:external: true`, the new refusal text), so
they remain valid — but they should be re-taken on a clean stack before sign-off, together with T092.

Clear it with `sudo kill 1406918`, then `docker compose -f deployments/docker-compose.yml up -d --build`.

### Also: leftover fixtures from another feature broke the unit suite

`internal/nova`'s `TestTargetInstance_*` failed with
`duplicate key value violates unique constraint "projects_name_key"` — leftover `spec023-a`/`spec023-b`
projects from an interrupted #549 run. Those tests seed by name and do not clean up on failure, so one
abandoned run poisons every later one. Removed (2 projects and their instances, networks, subnets,
ports); the suite is clean again. Same family as the known shared-postgres hazard, and another item for
the test-harness ticket (#548).
