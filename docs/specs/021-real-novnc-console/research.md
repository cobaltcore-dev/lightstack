# Phase 0 Research: a real console / noVNC (SPEC-021)

**Feature**: `021-real-novnc-console` | **Branch**: `feature/534` | **Date**: 2026-07-30
**Spec**: [spec.md](./spec.md) — all 12 clarification decisions are already recorded there; this document resolves the *technical* unknowns those decisions left, and records what was verified rather than assumed.

Everything below marked **verified** was executed against the actual pinned image, the actual go-libvirt module, or the locally installed libvirt schema — not inferred.

---

## R1 — Upstream console API contract (Constitution I is non-negotiable)

**Decision**: Implement three surfaces, matching Flamingo shapes exactly:

| Surface | Microversion | Shape |
|---|---|---|
| `POST /v2.1/servers/{id}/remote-consoles` | 2.6+ | request `{"remote_console":{"protocol","type"}}` → `{"remote_console":{"protocol","type","url"}}` |
| `POST /v2.1/servers/{id}/action` → `os-getVNCConsole` | legacy | → `{"console":{"type","url"}}` |
| `GET /v2.1/os-console-auth-tokens/{token}` | 2.31+ | → `{"console":{"instance_uuid","host","port","tls_port"?,"internal_access_path"?}}` |

**Rationale**: Sourced from the Nova api-ref and `rest_api_version_history.rst` via Context7 (`/openstack/nova`). The upstream response example is:

```json
{"remote_console": {"protocol": "vnc", "type": "novnc",
 "url": "http://example.com:6080/vnc_auto.html?path=%3Ftoken%3DXYZ"}}
```

The `path` query parameter is a **URL-encoded `?token=<token>`**, which the noVNC client passes to its WebSocket URL — so the browser opens `ws://<origin>/?token=<token>` against the proxy. This matters: it is the exact shape our proxy must accept, and R5 confirms it does.

`os-console-auth-tokens` is the compatibility surface only (FR-015). Our proxy resolves tokens from the mapping directory, so the console does not depend on this endpoint working. RDP returns 400 upstream (the Hyper-V driver was removed); we refuse it too (FR-025).

**Alternatives considered**: Skipping `os-console-auth-tokens` since nothing consumes it → rejected, the ticket asks for it and Constitution I makes the surface itself the requirement.

---

## R2 — HTTP status codes for the four refusals

**Decision**:

| Condition | Code | Reason |
|---|---|---|
| Unsupported console type (`xvpvnc`, `serial`, `spice-html5`, `rdp-html5`) | **400** | Matches upstream: an unavailable console type is a request-level error, and upstream returns 400 for RDP specifically |
| Instance not running / no graphical console | **409** | Upstream maps `InstanceNotReady` to 409 Conflict |
| Instance absent, or in another project without the `admin` role | **404** | Existing project-scoping idiom; never reveals existence |
| **Stub mode** | **501** | No upstream analogue exists — see below |
| Token unknown / expired (`os-console-auth-tokens`) | **404** | Upstream `itemNotFound` |

**Rationale**: Nova's documented error vocabulary for these endpoints is 400/401/403/404/409. Stub mode has *no* upstream counterpart — Flamingo always has a driver — so no code can be "the compatible one". 501 Not Implemented is chosen because the caller did nothing wrong (which rules out 4xx as misleading) and it states plainly that this deployment has no console subsystem. Constitution I is not strained: there is no Flamingo behaviour to contradict.

**Alternatives considered**: 400 with an `ConsoleTypeUnavailable`-style message for stub mode, on the grounds that "the driver can't do consoles" is upstream's nearest condition → rejected as diagnostically worse; a developer hitting stub mode should not have to wonder what was wrong with their request. Recorded here so the choice is reversible with an argument rather than a guess.

---

## R3 — Reading the assigned console port back from libvirt

**Decision**: `conn.DomainLookupByUUID(uuid)` → `conn.DomainGetXMLDesc(domain, 0)` → parse `/domain/devices/graphics[@type='vnc']/@port` and the listen address.

**Verified**: `func (l *Libvirt) DomainGetXMLDesc(Dom Domain, Flags DomainXMLFlags) (rXML string, err error)` exists in the pinned module (`go-libvirt@v0.0.0-20260609165003-6254771e63a8`, `remote_protocol.gen.go:4766`). No wrapper exists in `pkg/hypervisor/libvirt.go` today — this is a new capability, not a new caller.

**Flags = 0 is load-bearing**: flag `0` returns the **running** domain definition, where `autoport='yes'` has been resolved to a concrete port. `DomainXMLInactive` would return the persistent definition, where the port is still `-1`. Getting this wrong reproduces the original bug in a new costume.

**Rationale**: Matches the existing real-mode idiom exactly (`getVMStateReal` uses `DomainLookupByUUID` + `DomainGetState` with the same error-wrapping shape), so the new method is symmetric with its neighbours. Parsing is a narrow `encoding/xml` struct over just the `graphics` element — not a full domain model.

**Bounding the call at 1s (Constitution III)**: go-libvirt's generated methods take **no `context`** — verified, `DomainGetXMLDesc(Dom, Flags)` has no ctx parameter — and `WithLocalTimeout` on the dialer applies **only at connect time**, not per call. So the 1s budget must be imposed by the caller: run the lookup+readback in a goroutine and `select` on a 1s `context.WithTimeout`, returning a timeout error if it wins. The dialer's existing 2s value is therefore *not* inside a console request — the connection is established once at startup — which resolves what looked like a 1s/2s/2s contradiction between FR-007, this document and SC-010: FR-007's ≤1s governs the per-request readback, SC-010's <2s is the outer observed bound including handler overhead.

**Alternatives considered**: `virsh vncdisplay` via exec → rejected, shells out where a typed API exists. Tracking the port at define time → rejected in clarify (the port is only known once running, and the whole point is not to cache it). Leaving the call unbounded and relying on the dialer → rejected, it bounds the wrong thing.

---

## R4 — websockify token-file format and semantics

**Decision**: `--token-plugin TokenFile --token-source /var/lib/lightstack/console-tokens` (a **directory**), one file per token, each containing a single line `<token>: <host>:<port>`.

**Verified** by reading `token_plugins.py` out of the pinned image:

- `ReadOnlyTokenFile._load_targets()` accepts a **directory** and reads every file in it; each line is split with `re.split(r':\s', line)` and the target with `rsplit(':', 1)`.
- **`TokenFile.lookup()` calls `_load_targets()` on every lookup** — so writes take effect live and deletion is revocation. (`ReadOnlyTokenFile` caches on first use; it must **not** be used.)
- Lines beginning `#` are comments; a malformed line logs a syntax error and is skipped rather than killing the process.

**Two further websockify options matter, both verified in `websocketproxy.py`**:

- **`--idle-timeout` and `--timeout`** exist and both default to **0 (disabled)**. `--idle-timeout` is what makes FR-019's "idle browser session must release the guest console" implementable — without it, an abandoned tab holds the guest console open indefinitely. `--heartbeat` is also available and keeps NAT/proxy paths alive. Decision: set `--idle-timeout`, leave `--timeout` (a hard session cap) at 0 so a long legitimate console session is never cut off mid-use.
- **`--host-token`** exists: it makes websockify take the target **host** from the client rather than solely from the token. It defaults off, and it MUST stay off — enabling it would let a caller pick the target at connect time, directly violating FR-012. Because the requirement is satisfied by an *absent* flag, a chart test must assert the absence; nothing else would catch a future well-meaning addition.

**Two constraints fall out of the parser**:

1. The separator is `:` followed by **whitespace**. A token containing `": "` would corrupt parsing — hex-encoded tokens cannot, so the existing hex token alphabet is retained deliberately.
2. **The file format carries no expiry.** This is the single most consequential finding in this document — see R7.

**Alternatives considered**: `JSONTokenApi` (websockify calls Lightstack per connection — architecturally cleaner, no shared volume) → rejected in clarify because `requests` is absent from the image; `TokenRedis` → python `redis` absent; `JWTTokenApi` → `jwcrypto` absent *and* a self-contained signed token cannot be revoked (FR-014). `TokenFileName` (token as filename, content `host:port`) remains a viable equivalent if per-file parsing proves awkward.

---

## R5 — Does the URL actually work? (verified end to end)

**Decision**: issued URL = `<configured-origin>/vnc_lite.html?path=%3Ftoken%3D<token>`.

**Verified** — the pinned image was run with `--web /root/noVNC --token-plugin TokenFile --token-source /tokens 6080` against a fake guest on `127.0.0.1:5999`:

| Probe | Result |
|---|---|
| `GET /vnc_lite.html` | `200 OK` |
| `GET /vnc.html` | `200 OK` |
| `GET /vnc_auto.html` | **`404 File not found`** |
| `WS /?token=abc123` | **`101 Switching Protocols`**, log: `connecting to: 127.0.0.1:5999` |
| `WS /?token=bogus` | connection closed with **no response**, log: `Token 'bogus' not found` |
| `WS /websockify?token=abc123` | `101 Switching Protocols` |

Three conclusions:

1. Upstream's `path=%3Ftoken%3D…` encoding works unchanged against our proxy — no bespoke URL shape.
2. **A bad token fails closed inside the proxy**, contacting no guest. FR-011 is satisfied by the component itself, not by code we write.
3. `vnc_auto.html` really is gone. The image tag `1.3.0` is the *image's* version; it ships **noVNC 1.6.0**. The four deployment guides name a page that 404s.

**Why `vnc_lite.html` and not `vnc.html`**: `vnc_lite.html` connects on load, which is what an iframe in Horizon's Console tab needs. `vnc.html` is the full application and requires `autoconnect=1`. `vnc_lite.html` is the smaller contract.

**Alternatives considered**: `vnc.html?autoconnect=1&path=…` → viable fallback if the lite page proves too bare (no clipboard/fullscreen controls); recorded so the choice can be revisited without re-deriving it.

---

## R6 — Guest console binding and multi-viewer policy

> **Superseded for the binding** (review, 2026-09-24): the console is a unix socket, not loopback TCP. Since 2026-10-01 that socket sits at an explicit per-instance path (R12). `sharePolicy='force-shared'` still holds.

**Decision**: `<graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1' sharePolicy='force-shared'>` with a matching `<listen type='address' address='127.0.0.1'/>`.

**Verified**: the locally installed libvirt RELAX-NG schema (`/usr/share/libvirt/schemas/domaincommon.rng`) constrains `sharePolicy` to exactly `allow-exclusive` | `force-shared` | `ignore`. `force-shared` is the value that satisfies FR-018a; the default `allow-exclusive` is what would evict the first viewer.

**Reachability verified by inspection of the chart**: `charts/lightstack` mounts the **host's** libvirt socket (`realMode.libvirtSocketPath`) and runs `hostNetwork` + `privileged`, so libvirtd and its qemu processes live in the host network namespace. A proxy pod with `hostNetwork: true` shares that namespace, so `127.0.0.1:<port>` reaches the guest while nothing off-node can.

**Alternatives considered**: binding the node's routable IP behind firewall rules → rejected in clarify (protection becomes rule-dependent); `sharePolicy='ignore'` → leaves behaviour to whatever each client requests, which is how the current console got into its state.

---

## R7 — Enforcing expiry when the proxy cannot (⚠️ the hard one)

**Problem**: FR-010 requires a token to be refused after its lifetime. **websockify's token file has no expiry field** (R4), so the proxy will happily resolve an expired token for as long as its file exists. Expiry is therefore enforced *solely* by Lightstack deleting the file — deletion is not hygiene, it is the mechanism.

Lazy reaping (delete expired entries whenever a console is next requested) is **insufficient**: between a token expiring and the next console request — possibly hours — the token stays live. That would make FR-010 false.

**Decision**: a **periodic reconciler** that deletes expired rows and their projection files, plus opportunistic reaping at issue time and a sweep at startup. Default interval **30s**, configurable.

**Consequence to state honestly**: effective maximum validity is `TTL + reconcile interval` — 10m30s at the defaults, not 10m00s. The spec's FR-010 should be read with that bound; it is measurable and testable (SC-004), which a vague "about ten minutes" would not be.

**Constitution tension**: the Forbidden Patterns list bans "async workers: Celery, background job processors". A periodic reconciler is not a job processor — it holds no queue, accepts no work items, and only reconciles Lightstack's own projection to its own database. In-repo precedent: the real-mode network reconciler introduced by SPEC-009 WS-2 (`neutron.reconcile_interval`). Recorded as a justified deviation in `plan.md`, not waved away.

**Alternatives considered**: (a) lazy-only reaping → rejected, breaks FR-010 as shown; (b) push expiry into the proxy → impossible with any file-based plugin, and the HTTP-callback plugin that *could* do it is unavailable (R4); (c) encode expiry in the filename → websockify never reads it; (d) make TTL so short that lingering matters less → degrades usability without closing the hole.

---

## R8 — Proxy placement, permissions and chart wiring

> **Superseded for the mounts** (2026-10-01, R17): the proxy mounts only `console-tokens` and `console-sockets`, read-only, instead of the storage root and libvirt's domain directory. Node pinning and the schema notes below still hold.

**Decision**: a Deployment + Service in `charts/lightstack`, gated on real mode, with `hostNetwork: true`, pinned to the Lightstack node, mounting `realMode.hostStoragePath` (hostPath) to read the token directory. The `console-tokens` subdirectory joins the five the SPEC-011 initContainer already creates (`_helpers.tpl:177-193`), asserted in `charts/lightstack/tests/init_host_storage_test.yaml`.

**Rationale**: the mapping is node-local, so a proxy scheduled elsewhere sees an empty directory and refuses every token — placement must be pinned, not lucky. Single-node k3s makes this trivial today; pinning makes it correct rather than coincidental.

**Pinning mechanism — `podAffinity` with `topologyKey: kubernetes.io/hostname` targeting the Lightstack pod's labels.** Chosen over the alternatives because it is the only one that *follows* Lightstack: a `nodeSelector` or a hard-coded `nodeName` pins the proxy to a node the Lightstack pod might later leave, at which point the proxy silently reads a stale, empty directory on the wrong host — the exact failure this requirement exists to prevent. A `nodeAffinity` expression has the same weakness. Required affinity, not preferred: a "preferred" rule would schedule anyway and fail every token.

**Two sharp edges**:

1. **Permissions.** The existing noVNC pod spec drops ALL capabilities and inherits `podSecurityContext`, which may impose a non-root UID. Token files written 0600 by a root Lightstack would then be unreadable. Resolution at implementation time: write 0640 with a shared group, or run the proxy as the same UID. Whichever is chosen must be asserted by a chart test, because the failure mode is "every token refused" — indistinguishable from a token bug.
2. **`values.schema.json` uses `additionalProperties: false`**, so new console values fail validation until the schema is extended in the same commit.

**Alternatives considered**: hanging the proxy off the dashboard chart → rejected in clarify (pins Horizon to the hypervisor node); an `emptyDir` shared volume → impossible, the two pods are separate.

---

## R9 — Migration mechanics

**Decision**: one migration (`065_*`), reversible:

- **up**: create the console-token table with its indexes; `ALTER TABLE instances DROP COLUMN console_vnc_port, DROP COLUMN console_vnc_password`.
- **down**: drop the table; re-add both columns (nullable, as migration `005` created them).

**Rationale**: Constitution requires reversible up/down migrations. The dropped columns are provably dead: the port is now read per request (FR-004) and the password was never written into a domain definition. Down-migration restores the columns but not their contents — acceptable, since their contents were fabricated.

**Alternatives considered**: leaving the columns → rejected in clarify; reusing them instead of a new table → rejected, one row per instance cannot hold concurrent tokens.

---

## R10 — Scope of the chart relocation (⚠️ narrower than FR-023e implies)

**Finding**: `deployments/horizon-chart` is referenced by **18 files**. Only three may be edited:

| Referencing file | Editable? |
|---|---|
| `Makefile` | ✅ yes |
| `docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md` | ✅ yes (general docs) |
| `deployments/horizon-config/local_settings` | ✅ yes — the canonical source this feature already edits |
| `deployments/horizon-chart/files/local_settings` | n/a — the generated copy, relocated with the chart |
| `docs/specs/018-*` (4 files), `docs/specs/019-*` (4), `docs/specs/020-*` (6) | ❌ **no** — other features' spec directories |

> **Corrected 2026-09-11 during the rebase onto #533.** This first read said "16 files" and listed only
> two as editable, counting the 14 other-branch spec references plus `Makefile` and the report. It omitted
> both `local_settings` copies. That omission is not cosmetic: `deployments/horizon-config/local_settings`
> names the generated copy's path explicitly at line 102, so the relocation left a dead path there and in
> the generated `charts/horizon/files/local_settings`. The two must change **together** — the sync copies
> that line verbatim and `make horizon-chart-check` diffs them, so fixing one alone fails `chart-test` and
> CI. Fix the source and re-run `make horizon-chart-sync`; never hand-edit the chart copy. The per-directory
> split was also wrong (018 has 4 and 020 has 6, not 5 and 5), though the total of 14 was right.

**Decision**: the relocation updates `Makefile` (the `chart-test`, `horizon-chart-sync` and `horizon-chart-check` targets), the compatibility report, and the generated-copy path named in `deployments/horizon-config/local_settings` — re-running `make horizon-chart-sync` so the chart copy follows — and adds a note to this feature's own runbook. The 14 references inside SPEC-018/019/020 directories are **left stale deliberately** — those are historical records of what was run at the time, and the project rule is not to edit specs owned by other features.

**Rationale**: FR-023e says "documentation references updated to the new path", which read literally would have us rewriting three other features' spec directories. This narrows it to what is actually ours, and records why the remainder stays.

**Consequence**: anyone following the SPEC-018 real-mode runbook after this lands will hit a missing path. Mitigated by a one-line pointer in this feature's runbook and a note in the compatibility report.

---

## R11 — Package structure and the CLI surface (Articles I and II)

**Decision**: the token lifecycle ships as a self-contained library package, **`internal/console`** (`store.go`, `projection.go`, `reconciler.go`, `cli.go`), exposed by an **`lightstack console-tokens list|purge`** subcommand. `internal/nova` keeps only thin handlers that call it.

**Rationale**: an earlier draft of the plan recorded Articles I (Library-First) and II (CLI Interface) as deviations, claiming they conflict with Lightstack Principle VI (one binary, all services in `main.go`). **That was wrong, and the user rejected it.** A library is *compiled into* the binary, and a subcommand is *part of* it — neither implies a separate process. The repo already demonstrates the pattern twice:

| Library package | Subcommand | Dispatch |
|---|---|---|
| `internal/teardown` | `lightstack cleanup` | `cmd/lightstack/main.go:48` |
| `internal/health` | `lightstack healthcheck` | `cmd/lightstack/main.go:55` |

Both use `flag.NewFlagSet`, print to stdout, write errors to stderr and document their exit codes — exactly Article II's protocol. So `internal/console` + `lightstack console-tokens` is the idiomatic in-repo shape, not a novelty, and it satisfies both Articles without touching Principle VI.

**Secondary benefit**: it replaces hand-inspection of the token directory. The quickstart previously told operators to `ls` and `cat` files to diagnose a console; `lightstack console-tokens list` (with `--json`) is inspectable, scriptable and testable, which also serves Article V.

**Alternatives considered**: keeping the store in `internal/nova/console_token.go` → rejected, it buries independently testable logic inside HTTP handlers and satisfies neither Article. A `pkg/console` package → rejected, the store is Lightstack-specific and depends on the database, matching `internal/teardown`'s placement rather than the reusable `pkg/*` helpers. Exposing the store through the existing `lightstack cleanup` subcommand → rejected, cleanup is destructive teardown; console tokens need routine read access.

---

## Amendment 2026-10-01: guest sockets out of libvirt's per-domain directories

Context: review comment r3987379050. The proxy's mount of `/var/lib/libvirt/qemu`, combined with `DAC_OVERRIDE`, reached every guest's QMP `monitor.sock` and `master-key.aes`, which sit next to `vnc.sock` in `domain-<id>-<name>/`. Decisions are recorded in spec.md, Session 2026-10-01. Each fact below was read from source, not assumed.

## R12 — An explicit socket path in the domain XML

**Decision**: `<graphics type='vnc' …><listen type='socket' socket='<state_path>/console-sockets/<instance-uuid>.sock'/></graphics>`.

**Verified**:
- libvirt documents `listen type='socket'` with a `socket` attribute "since 2.0.0 (QEMU only)". Without the attribute, libvirt generates `<libDir>/vnc.sock` (`src/qemu/qemu_process.c:5575-5576`). That generated path is why the proxy needed libvirt's tree.
- `src/security/virt-aa-helper.c:1074-1081` adds every explicit listen socket to the guest's AppArmor profile with `rw`, so AppArmor allows the path for that guest only.

**Alternatives considered**: `listen type='none'` with `virDomainOpenGraphicsFD` (needs Lightstack to relay the FD, which contradicts the no-relay decision); keeping libvirt's path (the exposure this amendment removes).

## R13 — Socket path length

**Fact**: QEMU refuses a unix socket path longer than `sizeof(sun_path)` = 108 bytes, with the error "Path must be less than …" (`util/qemu-sockets.c`, `unix_listen_saddr`). The path is `state_path` + `/console-sockets/` (17) + uuid (36) + `.sock` (5).

**Decision**: in real mode, startup refuses a `state_path` longer than **50** characters. The default `/var/lib/lightstack` gives a 77-byte path. Stub mode is exempt, because it creates no sockets.

## R14 — Run identity: the libvirt domain ID

**Decision**: each token stores the domain ID read alongside the socket. The sweep revokes a token whose guest now runs under a different ID, or not at all.

**Verified**:
- go-libvirt's `Domain` carries `ID int32` (`remote_protocol.gen.go:77-81`), and `DomainLookupByUUID` returns it. The readback already makes that call.
- libvirt assigns a new ID on every start. At daemon start it continues from the highest ID among running domains (`qemuDomainFindMaxID`, `src/qemu/qemu_driver.c:509-517,913`), so a libvirtd restart neither reuses nor renumbers IDs.
- A host reboot restarts numbering at 1. A pre-reboot token can then coincide with its guest's new ID and survive until its TTL, reaching only that same guest. Accepted and recorded as an edge case.
- The ID is also what says a guest is not running. For a stopped domain libvirt returns the persistent definition, and an explicit socket is written into every definition except the migratable one (`src/conf/domain_conf.c:27963-27967`, libvirt master `f5ad405`). So the XML names the socket whether or not QEMU serves it. A stopped domain has ID −1 (`qemuProcessStop` sets it, `src/qemu/qemu_process.c:9461`; `qemuDomainLookupByUUID` returns it, `src/qemu/qemu_driver.c:1454`). The readback therefore refuses an ID ≤ 0 as not running (409, FR-006), and the sweep revokes. Without that check, a stopped guest got a token for a run that does not exist, and the `domain_id > 0` constraint turned the 409 into a 500.

**Alternatives considered**: no run identity (a token survives `virsh start` until its TTL); a per-start nonce in the path (covers only API starts, which are revoked anyway).

## R15 — Who owns `console-sockets`

**Decision**: Lightstack reads QEMU's uid:gid from libvirt's capabilities, `<host><secmodel><model>dac</model>…<baselabel type='kvm'>+UID:+GID</baselabel>`, falling back to `type='qemu'`. It then runs `MkdirAll`, `Chown` and `Chmod 0750` at real-mode startup, right after connecting.

**Verified**:
- libvirt's own example is `<baselabel type='kvm'>+77:+77</baselabel>` (`docs/formatcaps.rst:333-337`), and go-libvirt exposes `ConnectGetCapabilities() (string, error)` (`remote_protocol.gen.go:4585`).
- QEMU creates the socket itself, as that user, and unlinks a stale path before binding (`util/qemu-sockets.c`: `if (!abstract && unlink(path) < 0 && errno != ENOENT)`). So a fixed path survives restarts.
- On an orderly close QEMU removes the socket (`io/channel-socket.c` → `socket_listen_cleanup`). Leftovers only follow a crash.

**Works with**: the chart and both deploy scripts. Lightstack runs as root in the host user namespace (privileged pods cannot use user namespaces), and the state root is mounted at the same path.

**Alternatives considered**: 1777 + sticky (a squatted path blocks that guest's start); 0770 root:kvm (every `kvm` member could create files); an operator-set owner (no case needs it).

## R16 — libvirt does not prepare a socket directory the domain names

**Fact**: libvirt creates and labels only its own private directories (`qemuProcessMakeDir`, `src/qemu/qemu_process.c:5790-5804`, libvirt master), never the parent of a socket the domain XML names. A missing, unmounted or wrongly owned `console-sockets` therefore fails **every guest start**, not just the console.

**Decision**: the hypervisor manager records whether the directory is ready, and refuses before each `DomainCreate`: `CreateVM` (`pkg/hypervisor/libvirt.go:212`), hard `RebootVM` (`:371`), `StartVM` (`:465`) and `ResumeVM` (`:703`). The error is a typed `ErrConsoleSocketDirNotReady` that names the directory.

The answers are the ones the API already gives for a hypervisor failure:
- create returns 202, and its background boot marks the instance ERROR (`internal/nova/handlers.go:874-886`);
- `os-start` answers 500 (`:1726-1729`);
- hard reboot answers through the same 500 path, naming the directory;
- resume, like create, runs after its 202 and leaves the instance in ERROR, now with the cause logged;
- unshelve never starts a domain in this codebase.

Lightstack keeps running, since it also serves Keystone, Neutron, Cinder and Glance.

## R17 — Proxy mounts and privilege

**Decision**: the proxy mounts `<state_path>/console-tokens` and `<state_path>/console-sockets` only, both read-only and `DirectoryOrCreate` (whichever pod starts first creates them; Lightstack fixes the socket directory's owner at startup without changing its inode). `DAC_OVERRIDE` stays as its only capability. QEMU's socket is `srwxrwxr-x libvirt-qemu:kvm` in a 0750 directory, so root without it is "other" and can neither traverse nor connect. A read-only mount still allows `connect()` (verified in review).

**Finding**: both deploy scripts give their `novnc` container all of `$STORAGE_DIR` and `/var/lib/libvirt/qemu`, read-only (`scripts/deploy-turnkey.sh:784,788`, `scripts/deploy-single-node.sh:695,699`). That is the chart's comment-4 exposure plus this one. Both are narrowed the same way. `consoleProxy.libvirtDomainDir` is removed from the chart's values and schema.

## R18 — Domains defined earlier, teardown, and the KVM compose

- **Pre-move domains**: the readback sees a socket outside `console-sockets` and refuses it as misconfigured (500, the existing TCP-console answer). `docs/UPGRADING.md` explains recreating or redefining the domain. There is no automatic rewrite, because nothing is deployed beyond test hosts.
- **Teardown**: `purgeLocalStorage` (`internal/teardown/teardown.go:202-211`) adds `console-tokens` and `console-sockets`. The proxy is removed in the same uninstall, and Lightstack recreates both at its next start.
- **KVM compose**: `deployments/docker-compose-kvm.yml` mounts only `volumes`, `images` and `instances`. It gains `/var/lib/lightstack/console-sockets`, or the host's QEMU could not create the socket and no guest would start. It runs no proxy (`console_enabled: false`). Its missing `cloud-init`/`dhcp` mounts are pre-existing and out of scope.

---

## Resolved / not-a-question

- **No new Go module.** The relay is not ours to write (clarify decision), so the WebSocket dependency question evaporates and the binary-size gate is untouched.
- **Single-use tokens** are not implementable and not required — Lightstack never observes a proxy connection (spec FR-010a).
- **Multi-node** is out of scope for this feature and v1; no cross-node addressing is designed.

## Known limitations carried into the plan

- **`test/contract/nova` is stub-only** (finding F15 in the compatibility report), so the console's real-mode behaviour cannot be gated by the existing contract harness. Unit tests plus the host runbook carry it. Not fixed here.
- **The existing libvirt dialer uses a 2s timeout** (`WithLocalTimeout(2*time.Second)`, `libvirt.go:78`) while Constitution III specifies 1s. It applies at **connect** only, which happens once at startup, so it is not inside a console request — the per-request readback carries its own 1s deadline (R3). Pre-existing and out of scope to change.
