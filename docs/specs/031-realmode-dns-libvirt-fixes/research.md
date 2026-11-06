# Research: Real mode — cluster DNS under host networking, the libvirt socket, and a capabilities read that is never retried

**Feature**: `031-realmode-dns-libvirt-fixes` (cloud-storage#630) | **Date**: 2026-11-06
**Method**: read the code this branch starts from (`a3075c0`): `charts/lightstack` (templates, values, schema, tests), `pkg/hypervisor` (connection, console directory, disk, fake libvirtd tests), `internal/nova` (create, actions, self-heal, console), `internal/health`, `internal/teardown`, `cmd/lightstack`; the pinned go-libvirt module (`v0.0.0-20260609165003-6254771e63a8`); the libvirt and Kubernetes documentation the spec cites; and the openSUSE host's own systemd units.

Each item: **Decision / Rationale / Alternatives**.

---

## R1 — Cluster DNS for host-networked pods

**Decision**: Render `dnsPolicy: ClusterFirstWithHostNet` next to `hostNetwork` in `lightstack.podSpec` (`_helpers.tpl:411-413`, shared by the Deployment and the DaemonSet) and in the purge Job (`teardown-purge-job.yaml:36-38`), only when `realMode.enabled` and `realMode.hostNetwork` are both true. The pod spec already renders `hostNetwork: <value>` in real mode, `false` included, so the condition is on the value, not on real mode.

**Rationale**: For a host-networked pod Kubernetes demotes `ClusterFirst` to the node's resolver; `ClusterFirstWithHostNet` is the documented policy for exactly this case, and the console proxy already uses it (`console-proxy.yaml:42-43`). Where the kubelet has no cluster DNS configured it falls back to the node's resolver and records `MissingClusterDNS`, so no cluster loses resolution it had. The node's `/etc/hosts` keeps applying to host-networked pods; other names go through the cluster DNS, which forwards upstream (CoreDNS in k3s forwards to the node's resolvers). Guests are untouched: dnsmasq serves DHCP only (`port=0`, `pkg/networking/dhcp.go:213`).

**Alternatives**: a `dnsPolicy` or `dnsConfig` chart value (spec Out of Scope; nothing needs a choice); `dnsPolicy: None` with an explicit nameserver (the cluster DNS address differs per cluster); `hostAliases` (static, breaks when a Service is re-created — the defect itself).

## R2 — The chart passes a non-default socket to the binary

**Decision**: In `lightstack.config` (`_helpers.tpl:253`), render `libvirt_uri` as the bare `realMode.libvirtSocketPath` when it differs from `/var/run/libvirt/libvirt-sock`, else `"qemu:///system"` as today. The default is compared in one place. The schema gains `"pattern": "^/.+"` for `realMode.libvirtSocketPath`, as `realMode.hostStoragePath` already has. The rendered config is the Secret both the Lightstack pod and the purge Job mount, so one change reaches both.

**Rationale**: A bare absolute path is a form the binary has always taken and keeps taking (FR-008), the form real mode under Docker Compose on openSUSE already uses, and it needs no URL escaping. Rendering it only when non-default keeps the default render byte-identical (spec FR-004, the ticket's acceptance criterion) and leaves the default meaning "let the binary choose" (R4). With `secrets.existingSecret` the chart renders no config at all, so only documentation can reach that case (FR-005).

**Alternatives**: render `qemu+unix:///system?socket=<path>` (equivalent, needs escaping, gains nothing); always render the path (the default would then force `libvirt-sock` and defeat R4 on modular hosts, and change the default render); a new `realMode.libvirtUri` value (spec: no new value).

## R3 — The `libvirt_uri` grammar, and where it is refused

**Decision**: One pure function in `pkg/hypervisor`, `ParseLibvirtURI(string) (target, error)`, parsed with `net/url`:

| Form | Meaning |
|---|---|
| `""`, `qemu:///system`, `qemu+unix:///system` | automatic socket choice (R4) |
| either URI with `?socket=<absolute path>` | that socket, exactly |
| `<absolute path>` (no scheme) | that socket, exactly (kept) |
| anything else | refused |

Refused, each with one message naming the value and the accepted forms: another driver (`lxc://`, `xen://`, `test://`), a session or embedded URI (`qemu:///session`, `qemu:///embed`), any host or remote transport (`qemu://host/system`, `qemu+tcp://`, `qemu+ssh://`, `qemu+tls://`, `qemu+libssh2://`, `qemu+ext://`), a relative path, a relative `socket=`, a repeated `socket=`, any other parameter, a fragment.

It is called from `cmd/lightstack`'s `loadConfig` when `nova.libvirt_mode` is `real`. Every entry point already goes through `loadConfig`: the server (exit 1), `lightstack cleanup` (exit 2) and `lightstack console-tokens` (exit 2) — the exit codes `startup_refusal_test.go` already pins for refused configurations. The manager constructors call it again, so no path dials a refused value. Stub mode never dials libvirt and does not check the value.

**Rationale**: `connectLibvirt` (`libvirt.go:98-113`) uses any value other than `qemu:///system` verbatim as a socket path, so every form `docs/CONFIGURATION.md:436-440` advertises — session, TCP, SSH — has only ever failed as "no such file". A configuration error belongs at configuration load, like the existing real-mode checks there. `internal/common` stays free of the hypervisor package: importing it would pull go-libvirt into every package that reads the config. `pkg/hypervisor` imports only `pkg/paths`, so the parser adds no cycle.

**Alternatives**: validate in `internal/common.LoadConfig` (acyclic, but makes the config package depend on the hypervisor package); honour libvirt's `mode=legacy|direct` parameter (no requirement for it; the explicit socket already forces either daemon); keep the verbatim fallback (the defect).

## R4 — Choosing the socket on every attempt

**Decision**: A dialer of our own, implementing go-libvirt's one-method `socket.Dialer` (`Dial() (net.Conn, error)`, `socket/socket.go:60-62`). For the automatic form it checks `/var/run/libvirt/virtqemud-sock`, then `/var/run/libvirt/libvirt-sock`, with `os.Stat` on **every** `Dial`, dials the first that exists, and remembers which one, for the log line FR-010 requires. Neither present → an error naming both. An explicit socket dials that path only. The run directory is a constant that tests can point at a temporary directory.

**Rationale**:
- go-libvirt asks the dialer for a connection on every connect and reconnect, so a per-`Dial` choice is what makes FR-007's "made at every connection attempt" hold with no further machinery.
- `os.Stat` follows symlinks, so a host still carrying the `libvirt-sock → virtqemud-sock` workaround resolves to the same daemon.
- Stale socket files are not a practical risk: the openSUSE host's `virtqemud.socket` unit has `ListenStream=/run/libvirt/virtqemud-sock` and `RemoveOnStop=yes`, so a stopped socket unit leaves no file behind (`systemctl cat virtqemud.socket`).
- The QEMU daemon's socket is sufficient: Lightstack calls only `Domain*`, `ConnectGetCapabilities` and `ConnectGetLibVersion` — no network, storage, node-device or secret API (grep of `pkg/`, `internal/`, `cmd/`).
- libvirt's own daemon page says its RPC client "still prefers connecting to the monolithic daemon". Lightstack deliberately prefers the modular socket when it exists. Where `libvirt-sock` is `virtproxyd`'s, both reach the same daemon; a host running both daemon kinds is the misconfiguration the spec's edge case names.

**Alternatives**: choose once at startup (a daemon whose socket appears later would never be found — the reason FR-007 exists); prefer `libvirt-sock` (the defect on modular hosts); fall back to `libvirt-sock` when dialing `virtqemud-sock` fails (silently switches daemons and hides a broken one).

## R5 — Connecting: the server in the background, `lightstack cleanup` once

**Decision**:
- **Server**: a new constructor, `StartVMManager(uri, mode, consoleDir)`, returns at once in real mode. A background loop owns the connection:
  1. Dial with the R4 dialer. Each attempt — dial plus handshake — is bounded at 1 second. On timeout the attempt closes the connection its dialer returned, which unblocks go-libvirt.
  2. Back off between attempts with the existing `reconnectBackoffMin`/`reconnectBackoffMax` (1 s → 5 s).
  3. Once connected, publish the connection through the existing atomic pointer, log the socket, and run the preparation (R6).
  4. Watch `Disconnected()` and start again. This subsumes today's `keepConnected`/`redial`.

  Startup no longer waits on libvirt; readiness covers the gap (R8).
- **`lightstack cleanup`**: `NewVMManager` keeps its contract — one synchronous attempt with the R4 socket choice, an error if it fails (FR-020a) — and that attempt is bounded exactly as the loop's are: 1 second for dial plus handshake, through the same helper. A missing socket fails at the dial at once. A socket-activated `virtqemud` that has exited since the quiesce hook stopped Lightstack (`--timeout 120`) is started by the attempt itself; if it has not answered within the bound, the purge fails closed and re-running the uninstall finds it warm (spec Clarifications, analysis session).

**Rationale (go-libvirt, read)**: `ConnectToURI` (`libvirt.go:286-308`) dials, then runs `initLibvirtComms` — auth list and connect-open RPCs — with no deadline; today's dialer timeout (2 s) covers only the dial, so the bound is imposed around the whole connect, by closing the connection the dialer returned. `Disconnected()` returns a channel created only at the end of a successful connect, so a never-connected `*Libvirt` must never be watched; the loop keeps `m.conn` nil until a connect succeeds. Of the 15 `m.conn.Load()` call sites, 14 already handle nil: 13 return "not connected to libvirt" and `Close` skips it. The 15th is the reconnect loop's own watch (`libvirt.go:166`), which would panic on nil, and this loop is the one being rewritten. A never-connected manager therefore fails exactly as a disconnected one does, with no change at the other 14 sites. A reconnect builds a fresh `*Libvirt`, as `6ed30f7` does: re-opening one in place races in-flight calls.

**Alternatives**: keep the server's first attempt synchronous (a wedged daemon blocks startup, as it can today); leave any attempt unbounded — the loop's (a stuck attempt never retries) or cleanup's (a wedged daemon holds the uninstall until the Job's deadline) — against Article III's 1-second libvirt connection timeout.

## R6 — Preparing the console socket directory: retried, shared, sticky

**Decision**: The manager keeps:
- `prepared` — set by the first success, never cleared;
- the last failure's cause, for messages;
- the QEMU owner — replaced by every success;
- the directory, passed to `StartVMManager`;
- single-flight (`golang.org/x/sync/singleflight`): every attempt — the loop's or a guest start's — goes through one `Do`, so callers that arrive while an attempt runs share its result.

Attempts are made in two places:
- **Background**: after every connection, the first and each reconnect, the loop makes attempts with the same 1 → 5 s backoff until one succeeds, even when an earlier success is in effect (FR-015). A failed redo changes nothing that is in effect.
- **On demand**: a guest start that finds `prepared` false joins the attempt already running, or starts one if none is, and takes its result — one bounded attempt either way, never a wait followed by a second attempt (Article III v1.5.0; FR-013, FR-016).

Every attempt is today's `prepareConsoleSocketDir` under `withDeadline` (1 s, `console_endpoint.go:19`). Logging (FR-017a):

| Event | Level |
|---|---|
| First failure, with its cause | ERROR (today's line) |
| Success after a failure — directory, QEMU's uid:gid | INFO |
| A redo after a reconnect | DEBUG, or INFO if QEMU's user changed |
| Further retries | DEBUG |

**Rationale**: The clarified readiness gate needs a retry that does not depend on a guest start. No guest start reaches a not-Ready pod through its Service, so on-demand-only retrying, the ticket's minimum, would deadlock. A capabilities call that outlives its bound finishes in the background (`withDeadline` lets it), warming the daemon for the next attempt. Sharing the in-flight result is what keeps a guest start within one bound. `golang.org/x/sync/singleflight` does exactly that and is already required in `go.mod` (indirect, v0.23.0): it becomes a direct dependency, not a new module.

**Alternatives**: retry only on guest start (the deadlock above); let `/readyz` drive attempts (readiness would call libvirt; FR-017 forbids it); a mutex with a re-check (a start could wait for one attempt and then make another — up to 2 s, against the v1.5.0 exception and FR-016); lengthen the 1-second bound (Article III; the 2026-10-14 host run that found this defect pointed to retrying, not lengthening).

## R7 — One step before every guest launch

*Launch*, here and in the plan and tasks, means a *guest start* as the spec's Key Entities define it.

**Decision**: Every QEMU launch goes through `DomainCreate` in four functions — `createVMReal` (create and self-heal, both via `CreateVM`), `startVMReal`, `hardRebootVMReal`, `resumeVMReal`. These are the four existing `consoleDirReady()` call sites (`libvirt.go:224, 373, 535, 787`). In each:
- replace `consoleDirReady()` with `ensurePrepared(ctx)` — FR-013's on-demand attempt, refusing with FR-016's existing named error when it fails;
- call `PrepareInstanceDir(<domain UUID>)` (`disk.go:41-59`: `MkdirAll` plus a chown to the recorded owner, idempotent) immediately before `DomainCreate`.

In `createVMReal` the step runs after define, using the defined domain's UUID, and a failure undefines the domain on the existing path. `hardRebootVMReal` has no context and uses a background one; the attempt is bounded by `withDeadline` anyway. The `disk.go` comment claiming a chown-less directory "never gets used" goes, since a retried preparation makes that false.

**Rationale**: the domain UUID is the instance ID (`UUID: instanceID`, `internal/nova/handlers.go:1314`; `selfheal.go:73`), so `InstanceDir(uuid)` is the instance's directory. Self-heal calls `CreateVM` without `PrepareInstanceDir` (`selfheal.go:200`), and directories from releases before the ownership fix are root-owned. Placing the step in the four launch functions covers both with no change in `internal/nova`, and keeps FR-014's ordering — preparation before ownership — inside one function.

**Alternatives**: add the step at each `internal/nova` call site (create, self-heal, start, reboot, resume across several files — easy to miss one); chown only at create (the clarification rejected it); read the UUID out of the XML before define (an extra parse to avoid a path that already exists).

## R8 — Readiness waits for the first preparation (real mode only)

**Decision**: A `hypervisor` check (`health.Check`) whose probe reads the manager's sticky `prepared` — never a libvirt call — registered only when `libvirt_mode` is `real`. `health.New` moves from `main.go:109` to after the hypervisor starts; nothing uses the checker in between (its uses: `main.go:371, 415, 436`). The existing reason format then reads `hypervisor unavailable`, or `database,hypervisor unavailable`. `/healthcheck` mirrors readiness already (`TestHealthcheck_MirrorsReadiness`).

**Rationale**: FR-017 as clarified: not-ready until the first success, then never again because of libvirt. That keeps SPEC-013 FR-001's rule for outages and adds no error detail (CONTRACT.md §2 promises a coarse reason). The 800 ms per-check timeout (`health.go`, `defaultCheckTimeout`) is never at stake: the probe is an atomic load.

**Alternatives**: a `Checker.AddCheck` method (unneeded once the checker is created later); a separate endpoint (Kubernetes reads only the probes, and the clarification chose readiness); a live libvirt call (flaps on every outage; FR-017 forbids it).

## R9 — Real mode never fakes a server

**Decision**:
- `nova.Service.InitHypervisor` uses `StartVMManager`, so real mode always has a manager, connected or not. An unreachable libvirt fails each call as a lost connection does: the create goroutine marks the server `ERROR` (`handlers.go:1330-1340`) — now through `markInstanceError` (`handlers.go:644`), so the `fault` SPEC-025 added is recorded — and an action answers `sendLifecycleError`'s generic `500` (`handlers.go:2713-2721`). A guest start before the first preparation meets FR-016's named refusal (R7).
- In real mode `main.go` treats an `InitHypervisor` error as fatal. The only error left is an unsupported URI, which `loadConfig` has already refused.
- The create's stub branch (`handlers.go:1363-1371`) marks `ERROR` in real mode instead of `ACTIVE`, also through `markInstanceError`. That is now reachable only in tests that run real mode without a manager.
- The fault's message names the step that failed, curated as `allocationFault` does for networks; the detail stays in the log. For the libvirt create, `createFault`: `ErrConsoleSocketDirNotReady` (which also covers a missing connection) → "the hypervisor is not ready to start guests", anything else → "the hypervisor could not create the guest". The no-hypervisor branch reuses the first. The other fault-less `ERROR` writes in `CreateServer` — per-instance directory, root disk staging, image base, invalid `imageRef`, panic — get their own messages, listed in `contracts/guest-operations.md` C-GUEST-1. Every real-mode create that ends `ERROR` thus records a fault, closing the gap where a libvirt failure left none (spec Clarifications, analysis session).
- The other 12 nil-manager branches in `internal/nova` stay: real mode cannot reach them in production — `InitHypervisor` always leaves a manager in real mode, which a test now pins — and the tests that model a missing hypervisor keep their behaviour.

**Rationale**: FR-019 as clarified — the lost-connection answer, no new HTTP status or action message — plus the fault the analysis session added. Making the manager unconditional removes the fake path by construction. Hardening only the create branch keeps tests that model a missing hypervisor honest: `TestConsoleWithoutAHypervisorIsAnOutage` and the state reconciler's "blind" service keep their assertions.

**Alternatives**: harden all 12 other branches (no production path reaches them, and the guarantee is tested); answer `503` (rejected in clarification: upstream compute does not, Article I); leave the fault out (an `ERROR` with no reason, unlike upstream's).

## R10 — Test strategy (Test-first; real protocol over mocks)

**Decision**: RED first, per Development Workflow. The strategy below is reviewed with the user before the first test is written (step 2, "get approval").
- **`pkg/hypervisor`**:
  - a table test for `ParseLibvirtURI` covering every accepted and refused form (FR-011);
  - the R4 dialer against real unix listeners named `virtqemud-sock`/`libvirt-sock` in `t.TempDir()`: only one, the other, both, neither, and one appearing between attempts (FR-007, FR-010);
  - `StartVMManager` against the existing fake libvirtd: a first dial that fails and a later one that succeeds, with no restart (FR-020); a handshake the fake never answers is abandoned after 1 s and retried (R5); `NewVMManager` against a daemon that never answers fails after ≈1 s (R5);
  - the preparation: the fake delays its first `ConnectGetCapabilities` reply past 1 s, then answers at once → prepared with no restart, and the next start launches with its directory chowned (FR-018); `prepared` survives a dropped connection; a redo after a reconnect replaces the owner; a guest start arriving while an attempt runs takes that attempt's result within one bound (R6);
  - waits use an explicit deadline (`waitForWithin`, 10 s): the fake helpers' `waitFor` gives up after 2 s, too close to a 1.5 s first reply plus a retry;
  - each of the four launch functions chowns a root-owned directory before `DomainCreate` (FR-018, second sentence), observed without root through the existing `otherOwner` helper (`disk_test.go`).
- **`internal/nova`** (database-backed, both backends): real mode with an unconnected manager — `StartVMManager` given an explicit socket path that does not exist, since `internal/nova` reaches neither the fake libvirtd nor the run directory — booting from an existing volume under a temporary state path, so the create reaches `CreateVM` (an image boot would fail at disk staging first) → create ends `ERROR` with the hypervisor fault, and an `ACTIVE` server's `os-stop` answers the generic `500` (FR-019); `createFault`'s mapping has its own pure test; real mode without a manager → `ERROR` with a fault, not `ACTIVE`; real-mode `InitHypervisor` with no socket returns no error and a manager (the guarantee R9 relies on); `HypervisorReady` errors without a prepared manager — the prepared and sticky cases are `pkg/hypervisor`'s.
- **`test/contract/nova`**: a gophercloud test against a real-mode Lightstack whose libvirt socket is absent — a create ends `ERROR` with a fault, never `ACTIVE` (FR-019; Development Workflow step 1, contract tests with OpenStack clients). Gated by `LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1` and skipped otherwise, as SPEC-025 ran its real-mode contract cases on the host.
- **`internal/health` / `cmd/lightstack`**: the `hypervisor` check — reason while unprepared, ready after, ready through a later loss, absent in stub mode (FR-018a).
- **`cmd/lightstack/startup_refusal_test.go`**: an unsupported `libvirt_uri` in real mode → server exit 1, cleanup and console-tokens exit 2, each with the message; the same value in stub mode starts (FR-009).
- **`internal/teardown`**: real mode with a socket that does not exist → `Run` fails at once, with no dialing afterwards; a socket that accepts and never answers → `Run` fails within ≈1 s (FR-020a).
- **`charts/lightstack/tests`**: `dnsPolicy` on the Deployment, the DaemonSet and the purge Job, and its absence in stub mode and in real mode without host networking (FR-003); `libvirt_uri` rendered for a non-default socket and unchanged for the default (FR-004); the schema refusing a relative path (FR-006).

The fake libvirtd gains a configurable domain UUID with a `DomainDefineXML` reply, a delay before the first capabilities reply, and a mode that never answers the handshake.

**Rationale**: the fake speaks libvirt's real wire protocol and echoes serials, which is what makes reconnect tests possible: go-libvirt's `libvirttest` mock cannot follow a client across a reconnect, as `fake_libvirtd_test.go` records. Real unix listeners exercise `os.Stat` and `net.Dial` as production does. Chown is observed without root, as the existing ownership tests do.

**Alternatives**: mocks of the manager in `internal/nova` (would not exercise the nil-guard and lost-connection paths that make FR-019 hold); host-only verification of the retry (not deterministic — R12).

## R11 — Documentation the requirements name

**Decision**: These files change, per FR-003a, FR-005, FR-022, FR-023 and FR-024:
- **chart** — `charts/lightstack/README.md` (the `libvirtSocketPath` row per FR-005, the `hostNetwork` row, the health table), `values.yaml` (the `realMode` and `probes` comments), `values.schema.json` (the `libvirtSocketPath`, `hostNetwork` and probe descriptions), `templates/NOTES.txt`, `CONTRACT.md` §2;
- **`docs/`** — `CONFIGURATION.md` (the `libvirt_uri` forms), `SCALING.md` (drop `qemu+tcp://`), `TROUBLESHOOTING.md`, `REAL_LIBVIRT_MODE.md`, `REAL_MODE_TESTING.md`, `HORIZON_INTEGRATION.md` (name `virtqemud-sock` beside `libvirt-sock`), `blueprint.md` (readiness), `UPGRADING.md` (four notes).

`CLAUDE.md`'s "Mode Detection" idiom (`svc.vmManager == nil` as stub) follows the Article IV amendment (R13).

**Rationale**: each file states something this feature makes false. The `NOTES.txt` change is text only; `tests/notes_test.yaml` asserts nothing about readiness (grep).

**Alternatives**: none worth recording — the spec names the files.

## R12 — Host validation that exercises the retry paths

**Decision**: The FR-021 steps:

| Step | How it is forced | It passes when |
|---|---|---|
| (c) cold | `systemctl stop virtqemud`, then empty `/var/cache/libvirt/qemu/capabilities/`, before Lightstack starts | the log shows a failed first read and a later success. If a fast host still answers within 1 s, the step records "retry not exercised", and FR-018's unit test is the proof |
| (d) unreachable | stop the `virtqemud` socket units, which `RemoveOnStop=yes` makes remove the socket, then start Lightstack; create a server on the node's own address, since the pod is not Ready and its Service has no endpoint | the create ends `ERROR`; after the socket units are started again, the log shows the later connection and the socket dialed, the pod becomes Ready, and a VM boots |
| (e) monolithic | stop the modular socket units and start `libvirtd.socket` — enabled but inactive on the openSUSE host | the log names `libvirt-sock` and a VM boots |

**Rationale**: a warm daemon answers capabilities in about 0.04 s and caches QEMU's capabilities on disk, so a plain reboot may never hit the slow path. The cache directory exists on the host (root-only, `ls` → permission denied for the user).

**Alternatives**: a fresh boot only (may pass without exercising the fix — the clarification rejected it).

## R13 — Constitution v1.5.0 (ratified 2026-11-06)

Ratified in `.specify/memory/constitution.md` with two refinements of the draft below. Article III's first limit also lets a request "wait for one [attempt] already running", which R6's shared (single-flight) preparation attempts need. Article IV's Mode detection names `svc.networkingMode` beside `svc.libvirtMode`, since the article covers every service. The constitution file is authoritative.

The plan's Constitution Check finds two articles contradicted by the clarified behaviour, and the user chose amendment over a plan-only justification (2026-11-06), following the v1.1.0–v1.4.0 precedent. Draft text:

**Article III — add after "Never: …":**

> **Permitted exception — re-establishing a dependency in the background** (added in v1.5.0). A background loop that restores the process's own connection to an external dependency, or completes a one-time preparation that depends on it, does not violate this principle when all of the following hold:
> - No request waits on the loop. While the dependency is unreachable or the preparation incomplete, a request that needs it fails at once, as it would without the loop; it may make at most one attempt of its own, under the dependency's bound.
> - Every attempt is bounded as the dependency's calls are — 1 second for libvirt.
> - The pause between the loop's attempts is capped (1 to 5 seconds for libvirt), and the loop stops with the process.
> - The loop carries no queue, no job records and no per-request state, and accepts no work from a request.
>
> **Rationale**: the libvirt reconnect loop merged with `6ed30f7` (2026-09-24) — without it a libvirtd restart left real mode dead until the pod was restarted — contradicted "Never: retry loops, exponential backoff" without any spec or plan recording it. SPEC-031 extends it to the first connection and to the console socket directory preparation that real-mode readiness waits for. What the article protects is unchanged: a request still gets immediate feedback.

**Article IV — replace the "Mode detection" and "Default to stub mode" bullets:**

> - **Mode selection**: the configuration chooses the mode; an unset mode defaults to stub.
> - **Mode detection**: check `svc.libvirtMode == "stub"`. A nil `svc.vmManager` means no hypervisor was wired in (stub-mode tests); it never stands for real mode.
> - **No runtime fallback**: a configured real mode never falls back to stub. An unreachable dependency is an outage — the operations that need it fail as they do when it is lost later — never fake data.
>
> **Rationale**: read at runtime, "default to stub mode when dependencies unavailable" is how a real-mode deployment whose libvirt was unreachable at startup reported servers `ACTIVE` with no guest behind them (SPEC-031 Context, `internal/nova/handlers.go:1363-1371`), while the console path already treated the same state as an outage (`errNoHypervisor`). Stub mode stays the default when no mode is configured, which is what the rule was for.

**Runtime guidance to update with the amendment**: `CLAUDE.md` "Mode Detection" (`if svc.libvirtMode == "stub" || svc.vmManager == nil`). The known mismatch between `CLAUDE.md`'s "Nine Articles" and this constitution, flagged since v1.2.0, is unchanged by this amendment.

**Migration plan for the amendment**: none. Article III's exception records behaviour already shipped; Article IV's change ships with SPEC-031 and is documented in `docs/UPGRADING.md` (FR-024).
