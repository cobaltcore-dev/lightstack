# Feature Specification: Real mode — cluster DNS under host networking, the libvirt socket, and a capabilities read that is never retried

**Feature Branch**: `bugfix/630` (feature id: `031-realmode-dns-libvirt-fixes`)
**Created**: 2026-11-06
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#630 — "[BUG] - lightstack - Real mode - cluster DNS under hostNetwork, the libvirt socket path, and a one-shot capabilities read"

## Context

Three real-mode defects. They surfaced while planning a consumer that installs the chart in single-node real mode on k3s (`realMode.enabled`, `realMode.privileged` and `realMode.hostNetwork` all `true`), and while running real mode on a host with libvirt's modular daemons (openSUSE Tumbleweed: `virtqemud` socket-activated, no `libvirtd`). Each has a workaround, on the host or in the consumer, and each forces every real-mode consumer into it. Problems 2 and 3 block real mode on modular-daemon hosts until the workaround is in place. Paths and line numbers refer to `a3075c0`, the commit this branch starts from.

**Problem 1 — host-networked pods do not use cluster DNS.** The shared pod spec (`lightstack.podSpec`, `charts/lightstack/templates/_helpers.tpl:411-413`), used by the single-node Deployment and the multi-node DaemonSet, sets `hostNetwork` and no `dnsPolicy`. The teardown purge Job does the same (`charts/lightstack/templates/teardown-purge-job.yaml:36-38`). For a host-networked pod Kubernetes treats the default `ClusterFirst` like `Default`: the pod uses the node's resolver, and `<service>.<namespace>.svc` does not resolve. Every in-cluster endpoint Lightstack is given must therefore be a cluster IP or a node address: a PostgreSQL URL (in `database.url` or `database.existingSecret` — an operator-managed PostgreSQL such as CloudNativePG's `<cluster>-rw` Service is the common case, and the purge Job reads the database too), `glance.s3.endpoint` for an in-cluster RGW, `cache.redisUrl` for an in-cluster Redis. A cluster IP changes when its Service is re-created, and every consumer then renders the URL again. The console proxy already sets `dnsPolicy: ClusterFirstWithHostNet` (`charts/lightstack/templates/console-proxy.yaml:42-43`). A SQLite URL is not affected: that database is a node-local file.

**Problem 2 — `realMode.libvirtSocketPath` only picks what is mounted.** The value decides which host directory the pods mount (`_helpers.tpl:584,614`, `teardown-purge-job.yaml:60,78`). The rendered config always says `libvirt_uri: "qemu:///system"` (`_helpers.tpl:253`), and for that URI the binary dials the fixed path `/var/run/libvirt/libvirt-sock` (`connectLibvirt`, `pkg/hypervisor/libvirt.go:98-113`). Setting the value to another socket mounts the right directory and still dials `libvirt-sock`. The binary does take a socket path — any other `libvirt_uri` is used verbatim as one (`libvirt.go:101-107`), which is how real mode runs under Docker Compose on openSUSE (`nova.libvirt_uri: "/run/libvirt/virtqemud-sock"`) — but the chart offers no way to set it. The comment there says the scheme is stripped from a URI. It is not, so a real URI such as `qemu+unix:///system?socket=/run/libvirt/virtqemud-sock` fails as a nonexistent path, and so does every session and remote form `docs/CONFIGURATION.md:436-440` lists as an option.

Hosts running the modular daemons — the default on Fedora and openSUSE Tumbleweed — have no `libvirt-sock` unless `virtproxyd.socket` is enabled. The workarounds are on the host: enable `virtproxyd.socket`, or symlink `libvirt-sock` to `virtqemud-sock` in tmpfs, gone after every reboot. Lightstack calls only libvirt's domain and connect APIs, all served by the QEMU daemon, so that daemon's own socket is sufficient.

What follows a failed connection is worse than a refusal. `InitHypervisor` failing is logged as a warning and startup continues with no hypervisor (`cmd/lightstack/main.go:155-157`). `server create` then takes the stub branch (`internal/nova/handlers.go:1363-1371`) and reports the server `ACTIVE` with no guest behind it, while `/readyz` stays green. The console path already treats the same state as an outage (`errNoHypervisor`, `internal/nova/console.go:226-228`). The purge Job cannot connect either, so a `dataRetentionPolicy: delete` uninstall fails (`internal/teardown/teardown.go:80-83`).

**Problem 3 — one startup read of libvirt's capabilities, never retried.** At real-mode startup `PrepareConsoleSocketDir` reads libvirt's capabilities once (`cmd/lightstack/main.go:164`, `pkg/hypervisor/console_dir.go:53`) under the 1-second `consoleReadTimeout` (`pkg/hypervisor/console_endpoint.go:19`). If the read fails, the error is recorded and every guest start is refused with `console socket directory not ready` (`console_dir.go:67-77`). Nothing reads again — not a reconnect, not a later guest start — so the refusal lasts until the process restarts, although the startup log says starts are refused "until it is" ready. With socket activation the first call may have to start `virtqemud` and probe QEMU, which can take longer than a second. Lightstack then refuses every guest start while `/readyz` stays green: Kubernetes neither restarts the pod nor marks it unready, and an orchestrator sees a Ready Lightstack that cannot boot VMs. The workaround today: warm libvirt (`virsh -c qemu:///system capabilities`) before Lightstack starts.

The same read caches the user QEMU runs as. Each guest's per-instance directory is chowned to that user so QEMU can write the guest's `console.log`. While the user is unknown, `PrepareInstanceDir` (`pkg/hypervisor/disk.go:41-59`) creates the directory root-owned and relies on the refusal to make that harmless. A retry that succeeds lifts the refusal, so it must not leave a root-owned directory behind it.

Guests are not affected by the DNS change: the per-network dnsmasq serves DHCP only (`port=0`, `pkg/networking/dhcp.go:213`) and hands guests the subnet's `dns_nameservers`, not the pod's resolver.

## Clarifications

### Session 2026-11-06

- Q: The console socket directory preparation now retries until it succeeds. How does "guests cannot start yet" become visible? The issue proposes the readiness probe "or at least a log line", and SPEC-013 FR-001 rules that libvirt never affects readiness. → A: **Readiness waits for the one-time preparation, in real mode only.** `/readyz` reports not-ready until the directory has been prepared once. From then on nothing about libvirt affects readiness again — an outage, a reconnect or a failed redo does not — so SPEC-013 FR-001 keeps holding for outages. Until then the pod is out of the Service (on a single node: all six APIs), and the startup probe restarts it if the preparation does not succeed within its budget. This amends SPEC-013 FR-001 for real mode (FR-017).
- Q: Not in the issue, found in the code: when real-mode Lightstack cannot reach libvirt at startup, it carries on with no hypervisor, and `server create` reports servers `ACTIVE` with no guest behind them. Is that in scope? → A: **Yes — retry, and never fake.** While unconnected, guest operations fail — answered as a lost connection is, per the next answer — and a create ends in `ERROR`. The first connection is retried in the background like a lost one, with the socket choice made on every attempt. Once connected, the preparation runs and guests start without a restart (User Story 6, FR-019, FR-020).
- Q: While real-mode Lightstack has no libvirt connection — never connected yet, or lost — how does a server action answer? The spec said "an error naming libvirt"; SPEC-025 deliberately answers a lost connection with a generic `500`. → A: **As a lost connection.** One answer for an unreachable libvirt, never connected or lost: a server action answers `500` with the generic "failed to perform <action>" (lifecycle actions; other handlers keep their own message — analysis session), the cause in the log; a create is accepted and ends in `ERROR`. No new message, no `503`. A guest start before the first preparation keeps FR-016's refusal, which already names the directory and its cause (FR-019).
- Q: A guest must not start with a root-owned per-instance directory, or a modular `virtqemud` cannot write its `console.log`. Only the create paths prepare the directory today; self-heal re-creates a guest without that step, and guests from earlier releases keep root-owned directories. Where is the ownership ensured? → A: **Immediately before every guest start** — create (image or volume boot), start, hard reboot, resume and self-heal re-creation. Idempotent, so it also repairs a directory made while QEMU's user was unknown, one left by an earlier release, and one whose QEMU user changed under a reconnect redo (FR-014).
- Q: How does host validation prove the retry paths? A warm libvirt answers capabilities in about 0.04 s and caches QEMU's capabilities on disk, so a plain reboot may never hit the slow first read; the unreachable-libvirt path and the monolithic path had no host step. → A: **Force both paths, and add a monolithic step.** Cold: stop `virtqemud` and clear its QEMU capabilities cache before Lightstack starts. Unreachable: start Lightstack with libvirt's sockets stopped, then start them. Each passes when a VM boots to `ACTIVE` with no restart and the log shows the failed first attempt, the later success and the socket dialed. Monolithic: a default install against `libvirtd` — the same host switched over will do — still dials `libvirt-sock` (FR-010, FR-021).
- Q: The server now keeps dialing libvirt in the background. What does `lightstack cleanup` — the uninstall purge Job, fail-closed per SPEC-010 — do when libvirt is unreachable? → A: **One attempt, fail closed, as today.** One connection attempt with the socket choice of FR-007; if it fails, the purge fails at once and the uninstall stops. No background dialing in the CLI, no waiting (FR-020a).
- Q: Nothing documented Problem 1's change — host-networked pods now resolve through the cluster DNS — or the host workarounds the fixes retire. What do the docs say? → A: **The chart docs and `docs/UPGRADING.md`.** The chart README and the `realMode.hostNetwork` comment and description say host networking renders `dnsPolicy: ClusterFirstWithHostNet`, so in-cluster Service names resolve. `docs/UPGRADING.md` says names only the node's own resolver can answer must be reachable through the cluster DNS, and that the `libvirt-sock` symlink, enabling `virtproxyd.socket` and warming libvirt before start can be removed (FR-003a, FR-024).

### Session 2026-11-06 (analysis)

- Q: `lightstack cleanup`'s one connection attempt kept a 2-second dial timeout and no bound on the handshake, against Article III's "libvirt connection failures: 1 second timeout" and this spec's "no waiting". Bound it? → A: **Yes — the same 1-second bound as every other attempt.** A socket-activated daemon that has not answered within it fails the purge closed; the attempt itself started the daemon, so re-running the uninstall finds it warm (FR-020a).
- Q: A guest start that finds a preparation attempt already running could wait for it and then make its own — up to 2 seconds, against the v1.5.0 exception's "one attempt … or wait for one already running … within the dependency's bound". → A: **It takes the result of the attempt already running; only a start that finds none makes its own** (FR-013, FR-016).
- Q: A create that ends `ERROR` because libvirt is unreachable carried no `fault` — the libvirt failure path bypasses the one SPEC-025 added — while upstream reports why a build failed. Keep that out of scope? → A: **No. Every real-mode create that ends `ERROR` records a fault**, with a curated message, as the network-allocation failure already does (FR-019).
- Q: "failed to perform <action>" is what lifecycle actions answer; delete, console and volume attach/detach have their own generic `500` messages. Which does a non-start action give while libvirt is unreachable? → A: **Each handler's existing message for a lost connection** — no new text (FR-019, US6 scenario 2).

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Reach in-cluster services by name in real mode (Priority: P1)

An operator installs the chart in real mode with host networking on k3s and points Lightstack at an in-cluster PostgreSQL by its Service name (`<service>.<namespace>.svc`), and at an in-cluster RGW and Redis the same way if they have them. The pod resolves the names, becomes Ready, and keeps working when a Service is re-created. An uninstall with `dataRetentionPolicy: delete` completes, because the purge Job resolves the same names.

**Why this priority**: An operator-managed database in the same cluster is the common case, and today it works only through a cluster IP that changes under the consumer. It is also the smallest fix.

**Independent Test**: Render the chart with real mode and host networking and confirm the policy on all three workloads. On k3s, install with a Service-name PostgreSQL URL; the pod becomes Ready (`/readyz` is gated on the database); uninstall with the delete policy completes.

**Acceptance Scenarios**:

1. **Given** `realMode.enabled: true` and `realMode.hostNetwork: true`, **When** the chart renders, **Then** the Deployment, the multi-node DaemonSet and the purge Job carry `dnsPolicy: ClusterFirstWithHostNet`.
2. **Given** stub mode, or real mode with `realMode.hostNetwork: false`, **When** the chart renders, **Then** no `dnsPolicy` is rendered — as today.
3. **Given** real mode with host networking on k3s and a PostgreSQL URL naming `<service>.<namespace>.svc`, **When** the chart is installed, **Then** the pod becomes Ready.
4. **Given** that release with `dataRetentionPolicy: delete`, **When** it is uninstalled, **Then** the purge Job reaches the database by its Service name and the uninstall completes.

---

### User Story 2 — Run real mode on a host with only libvirt's modular daemons (Priority: P1)

An operator runs real mode on a host where libvirt runs as modular daemons: `virtqemud` with socket activation, no `libvirtd`, no `virtproxyd`, no symlink. They install the chart with default values. Lightstack connects to the QEMU daemon's own socket, a VM boots to `ACTIVE`, and an uninstall with the delete policy destroys it. On a host running the monolithic `libvirtd`, nothing changes.

**Why this priority**: Real mode does not work on these hosts today without a host-side workaround that does not survive a reboot — and without the workaround, Lightstack reports servers it never created.

**Independent Test**: Unit tests cover the socket choice with each socket present or absent. On a modular-daemon host with no workaround, install the chart with default values, create a server, see it `ACTIVE` with its guest in `virsh list`, uninstall with the delete policy, and see the guest gone. Switch the host to monolithic `libvirtd`, install again with default values, and see the log name `libvirt-sock` and a server reach `ACTIVE`.

**Acceptance Scenarios**:

1. **Given** the default URI and a host where only `virtqemud-sock` exists, **When** Lightstack connects, **Then** it dials `virtqemud-sock`.
2. **Given** the default URI and a host where `libvirt-sock` exists and `virtqemud-sock` does not, **When** Lightstack connects, **Then** it dials `libvirt-sock` — as today.
3. **Given** a modular-daemon host with no workaround and a chart install with default values, **When** a server is created, **Then** it boots to `ACTIVE` with a guest behind it.
4. **Given** that release with `dataRetentionPolicy: delete`, **When** it is uninstalled, **Then** the purge Job connects the same way and destroys the guest.

---

### User Story 3 — The first guest boots on a cold, socket-activated libvirt (Priority: P1)

An operator starts Lightstack on a host whose `virtqemud` is socket-activated and not yet running — after a reboot, with no warm-up. The first capabilities read takes longer than a second and fails. Lightstack keeps trying. The pod reports Ready only once guests can start, and from then on guest starts work — no restart, no warm-up step. The log says when guests became startable.

**Why this priority**: Today a cold daemon at startup disables guest starts until the pod restarts, while the pod reports Ready.

**Independent Test**: Unit: a fake libvirt whose first capabilities read times out and whose second succeeds; the next guest start succeeds without a restart, and its per-instance directory belongs to QEMU's user. Readiness: not-ready in real mode before the preparation succeeds, ready after, still ready through a later libvirt outage; stub mode unchanged. Host: stop the socket-activated `virtqemud` and clear its QEMU capabilities cache, then start Lightstack; the log shows the failed first read and the later success, the pod becomes Ready, and a server reaches `ACTIVE`.

**Acceptance Scenarios**:

1. **Given** a first capabilities read that times out, **When** libvirt later answers, **Then** the preparation succeeds without a restart, and a log line says so, naming the directory and QEMU's user.
2. **Given** the preparation has not yet succeeded, **When** a guest start arrives, **Then** it makes one attempt within the 1-second bound — or, if an attempt is already running, takes that one's result — before going on, and is refused with the existing message naming the directory and the cause only if that attempt fails.
3. **Given** a per-instance directory created while QEMU's user was unknown, **When** that guest starts after the preparation has succeeded, **Then** the directory belongs to QEMU's user and the guest's console log can be written.
4. **Given** a guest whose per-instance directory is root-owned — left by an earlier release — **When** it is started, hard-rebooted, resumed or self-healed, **Then** the directory is made QEMU's before QEMU launches, and the guest runs.
5. **Given** a host with socket-activated `virtqemud` and no warm-up, **When** Lightstack starts, **Then** the pod becomes Ready, and the first server created boots to `ACTIVE`.
6. **Given** the preparation has succeeded, **When** libvirt restarts and Lightstack reconnects, **Then** the preparation runs again, and a redo that times out does not stop guest starts.
7. **Given** real mode and a preparation that has not yet succeeded, **When** readiness is asked, **Then** it answers not-ready with a reason naming the hypervisor, and the pod is not Ready.
8. **Given** the preparation has succeeded once, **When** libvirt later goes away or a redo fails, **Then** readiness stays ready.
9. **Given** stub mode, **When** readiness is asked, **Then** it depends on the database and the listeners only, as today.

---

### User Story 4 — Point the chart at a specific libvirt socket (Priority: P2)

An operator whose libvirt socket lives elsewhere — another directory, or one specific daemon's socket — sets `realMode.libvirtSocketPath`. The pods mount its directory, as today, and now the Lightstack pod and the purge Job also dial that socket.

**Why this priority**: The value looks like the fix and does nothing today. With User Story 2 most hosts no longer need it, but a non-standard layout still does.

**Independent Test**: helm-unittest: with a non-default value, the rendered config read by the Lightstack pod and by the purge Job names that socket as `libvirt_uri`; with the default value the rendered `libvirt_uri` is unchanged.

**Acceptance Scenarios**:

1. **Given** `realMode.libvirtSocketPath: /run/libvirt/virtqemud-sock`, **When** the chart renders, **Then** the rendered config's `libvirt_uri` is that path and the pods mount `/run/libvirt`.
2. **Given** the default value, **When** the chart renders, **Then** `libvirt_uri` is `qemu:///system`, as today.
3. **Given** a configured socket, **When** the binary connects, **Then** it dials exactly that path, whatever else exists.

---

### User Story 5 — A libvirt setting Lightstack cannot honour is refused at startup (Priority: P3)

An operator writes a `libvirt_uri` Lightstack does not support: a remote URI, a session URI, another driver. Real-mode Lightstack refuses to start and names the value and the forms it accepts, instead of dialing the string as a file and carrying on without a hypervisor. A local URI that names its socket (`qemu+unix:///system?socket=<path>`) works.

**Why this priority**: A configuration error, not an outage. Today it ends in a misleading "no such file or directory" and a real mode with no hypervisor, and the documentation advertises forms that never worked.

**Independent Test**: Unit tests over the accepted and refused forms. A startup test with an unsupported URI in real mode exits non-zero with the message; the same URI in stub mode starts.

**Acceptance Scenarios**:

1. **Given** `libvirt_uri: "qemu+unix:///system?socket=/run/libvirt/virtqemud-sock"`, **When** Lightstack connects, **Then** it dials that path.
2. **Given** an unsupported URI in real mode, **When** Lightstack or `lightstack cleanup` starts, **Then** it exits non-zero naming the value and the accepted forms.
3. **Given** the same URI in stub mode, **When** Lightstack starts, **Then** it starts — stub mode never dials libvirt.

---

### User Story 6 — Real mode never reports a server it did not create (Priority: P1)

An operator starts real-mode Lightstack while libvirt cannot be reached — its socket missing, the daemon down, the node still booting. Today Lightstack carries on with no hypervisor and answers `server create` with a server that turns `ACTIVE` with no guest behind it. Instead, guest operations fail as they do while a lost connection is being re-established, the pod is not Ready, and Lightstack keeps trying to connect as it does after a lost connection. Once libvirt is reachable, it prepares, becomes Ready, and guests start normally — no restart.

**Why this priority**: It is what turns Problems 2 and 3 from refusals into wrong answers: an orchestrator, Terraform or Horizon sees a server that does not exist.

**Independent Test**: Unit: a manager whose first dial fails and a later one succeeds; guest operations fail while unconnected, as during a lost connection, and succeed after, with no restart. API: in real mode with libvirt unreachable, `server create` ends in `ERROR`, never `ACTIVE`, and a server action answers the generic `500`. Host: start Lightstack with libvirt's sockets stopped, then start them; the log shows the failed first attempt, the later connection and the socket dialed, and a server reaches `ACTIVE` with no restart.

**Acceptance Scenarios**:

1. **Given** real mode and libvirt unreachable at startup, **When** a server is created on any path that still reaches the API — Docker Compose, a bare-metal install, the pod's own address — **Then** it is accepted and ends in `ERROR`, never `ACTIVE`, with a `fault` that says why in a curated message; the log names libvirt as the cause.
2. **Given** that state, **When** a server action that does not start a guest is requested — stop, delete, console, volume attach — **Then** it answers `500` with the message its handler gives a lost connection — "failed to perform <action>" for stop, the handler's own generic message for delete, console and volume attach — and the log names libvirt.
3. **Given** that state, **When** readiness is asked, **Then** it answers not-ready with a reason naming the hypervisor.
4. **Given** that state, **When** libvirt becomes reachable, **Then** Lightstack connects without a restart, prepares the console socket directory, becomes Ready, and the next server boots to `ACTIVE`.
5. **Given** the default URI and neither socket present at startup, **When** one appears later, **Then** Lightstack finds it on a later attempt.

---

### Edge Cases

- **Real mode without host networking**: the pod already uses cluster DNS; nothing is rendered (FR-002).
- **No cluster DNS configured**: for a `ClusterFirstWithHostNet` pod, Kubernetes falls back to the node's resolver and records a `MissingClusterDNS` event; behaviour is as today.
- **Names only the node's resolver can answer**: the node's `/etc/hosts` still applies to host-networked pods. Other names now go through the cluster DNS, which forwards to the node's upstream resolvers by default (CoreDNS in k3s does). A split-horizon name the cluster DNS cannot forward stops resolving — the cost of the standard policy.
- **Upgrading an existing release**: the pod spec gains `dnsPolicy`, so `helm upgrade` replaces the pod (Recreate in real mode), as any pod-spec change does.
- **Both sockets present**: `virtqemud-sock` is chosen. Where `libvirt-sock` belongs to `virtproxyd`, both reach the same daemon. Where it belongs to a monolithic `libvirtd` running beside `virtqemud`, two daemons manage QEMU — a host misconfiguration that libvirt's migration steps avoid by stopping the monolithic daemon first. Lightstack's guests then live in `virtqemud`.
- **Neither socket exists**: the connection fails and the error names both paths (FR-010).
- **Mixed multi-node fleet**: each node's pod makes its own choice, so monolithic and modular nodes run with the same default value.
- **`secrets.existingSecret`**: the chart renders no config, so the operator's own config must name the socket the pods mount. The chart cannot check a Secret it does not render (FR-005).
- **A non-absolute `realMode.libvirtSocketPath`**: refused by the schema (FR-006). It would mount, and now dial, a path relative to the pod's working directory.
- **The explicit default**: through the chart, `libvirt-sock` at its default path cannot be forced over `virtqemud-sock` — the default value means "choose". It is what the binary picks whenever `virtqemud-sock` is absent; the binary's own config can force it with a bare path (FR-008).
- **A guest start while the preparation is pending**: one attempt within the 1-second bound, or the result of the attempt already running — never both; if it fails, the start is refused as today, and a later start succeeds once libvirt answers.
- **A per-instance directory that is not QEMU's** — made while QEMU's user was unknown, left root-owned by an earlier release, or owned by a QEMU user that has since changed: it is made QEMU's immediately before its guest starts, on every path, self-heal included (FR-014).
- **A capabilities call that outlives its bound**: it finishes in the background, as the existing bound already allows, and a later attempt gets the warm answer.
- **libvirt restarts after a successful preparation**: the preparation is redone on reconnect; a redo that times out leaves guest starts working (FR-015), and readiness stays ready (FR-017).
- **A preparation that never succeeds** — libvirt reports no DAC label for QEMU, the directory cannot be chowned, libvirt never comes up: the pod is never Ready. The startup probe restarts it once its budget is spent (`probes.startup`, 30 × 5 s by default) and it ends in CrashLoopBackOff, with the cause at the top of each start's log; `helm install --wait` fails. Before this change the same pod was Ready and refused every guest start.
- **A single node that is not yet Ready**: the Service has no endpoint, so all six APIs — Keystone included — are unreachable through it until the preparation succeeds. The listeners still answer on the pod's own address (the node's, with host networking).
- **Multi-node**: only the pod whose node cannot prepare leaves the Service; the others keep serving the shared API.
- **libvirt unreachable during an uninstall**: the purge makes one attempt, bounded at 1 s, and fails closed (FR-020a); the operator brings libvirt back and uninstalls again. With socket activation the socket exists while the daemon is stopped: the attempt starts the daemon, and if the daemon has not answered within the bound, a re-run finds it warm.
- **Docker Compose and bare metal**: nothing gates on readiness there; `/readyz` reports the same state, and guest operations behave as FR-016 and FR-019 say.
- **Stub mode**: no host networking, no libvirt, no change to readiness; nothing here changes it.

## Requirements *(mandatory)*

### Functional Requirements

**Cluster DNS under host networking (Problem 1)**

- **FR-001**: Every pod the chart runs with host networking MUST resolve in-cluster Service names. When `realMode.enabled` and `realMode.hostNetwork` are both `true`, the single-node Deployment and the multi-node DaemonSet (through the shared pod spec) and the teardown purge Job MUST carry `dnsPolicy: ClusterFirstWithHostNet`, as the console proxy already does.
- **FR-002**: Without host networking — stub mode, or real mode with `realMode.hostNetwork: false` — the chart MUST render no `dnsPolicy`, exactly as today.
- **FR-003**: helm-unittest cases MUST cover FR-001 for all three workloads and FR-002 for both cases without host networking.
- **FR-003a**: The chart README and the `realMode.hostNetwork` comment in `values.yaml` and description in `values.schema.json` MUST say that host networking renders `dnsPolicy: ClusterFirstWithHostNet`, so the Lightstack pod and the purge Job resolve in-cluster Service names.

**The libvirt socket (Problem 2)**

- **FR-004**: When `realMode.libvirtSocketPath` differs from its default (`/var/run/libvirt/libvirt-sock`), the chart MUST render it as `nova.libvirt_uri` in the configuration it renders, which both the Lightstack pod and the purge Job read, so the socket mounted and the socket dialed are the same. With the default value the rendered `libvirt_uri` MUST stay `qemu:///system`. No new chart value.
- **FR-005**: The documentation of `realMode.libvirtSocketPath` (the `values.yaml` comment, the schema description, the chart README) MUST say that a non-default value is also the socket dialed and that the default lets the binary choose (FR-007). Because the chart renders no configuration with `secrets.existingSecret`, it MUST also say that the operator's own config must then name the same socket.
- **FR-006**: The chart schema MUST require `realMode.libvirtSocketPath` to be an absolute path, as it requires `realMode.hostStoragePath`.
- **FR-007**: For `qemu:///system` — and for an empty `libvirt_uri` and `qemu+unix:///system` without a socket parameter, which mean the same — the binary MUST dial the modular QEMU daemon's socket, `/var/run/libvirt/virtqemud-sock`, when it exists, and `/var/run/libvirt/libvirt-sock` otherwise. The choice MUST be made at every connection attempt, reconnects included, so a socket that appears or disappears while Lightstack runs is followed without a restart.
- **FR-008**: The binary MUST accept, and dial exactly, an explicit socket: a `socket=<absolute path>` parameter on `qemu:///system` or `qemu+unix:///system`, or a bare absolute path (today's behaviour, kept). An explicit socket MUST win over the automatic choice.
- **FR-009**: In real mode, any other `libvirt_uri` — another driver, a session URI, a remote transport or host, a relative path, a parameter other than `socket` — MUST stop every entry point that loads the configuration — the server, `lightstack cleanup` and `lightstack console-tokens` — at configuration load, with an error naming the value and the accepted forms. Stub mode never dials libvirt and MUST NOT check the value. The code comment MUST describe what the code does.
- **FR-010**: A failed connection MUST name the socket path or paths it tried, and a successful one MUST log the socket it dialed.
- **FR-011**: Unit tests MUST cover the resolution: the default with only `virtqemud-sock` → that socket; the default with `libvirt-sock` and no `virtqemud-sock` → `libvirt-sock`; the default with both → `virtqemud-sock`; a configured socket (parameter or bare path) → that path, whatever exists; each refused form → the startup error.
- **FR-012**: On a host running the monolithic `libvirtd`, behaviour MUST be unchanged with default values: Lightstack dials `libvirt-sock`.

**The console socket directory preparation (Problem 3)**

- **FR-013**: A failed preparation MUST NOT be final. Lightstack MUST attempt it again until it succeeds — in the background with backoff from the moment it first fails, and on demand when a guest start finds it not done. A guest start that finds an attempt already running MUST take that attempt's result instead of starting a second. Every attempt MUST keep the existing 1-second bound on its libvirt call (Constitution: fail-fast), so no guest start waits longer than one bound.
- **FR-014**: Immediately before every guest start, Lightstack MUST ensure the guest's per-instance directory exists and belongs to the user QEMU runs as, as the preparation last recorded it. The step MUST be idempotent, so it also repairs a directory created while that user was unknown, one left root-owned by an earlier release, and one whose owner changed when a reconnect redo found a different user (FR-015). The on-demand preparation attempt (FR-013) MUST come before this step, which needs the user it records. A start whose directory cannot be made QEMU's MUST NOT launch the guest.
- **FR-015**: After each libvirt reconnect the preparation MUST run again, so a change of QEMU's user made while libvirt was down is picked up. A redo that fails MUST NOT withdraw a success already in effect.
- **FR-016**: While the preparation has not succeeded, a guest start that cannot complete it MUST be refused, after no more than one 1-second attempt — its own or the one it joined — with the existing message naming the directory and the cause. Every other service MUST keep answering: the pod is not Ready meanwhile (FR-017), but its listeners stay up.
- **FR-017**: In real mode Lightstack MUST report not-ready until the preparation has succeeded once. From then on nothing about libvirt MUST affect readiness again — not an outage, not a reconnect, not a failed redo — so SPEC-013 FR-001 keeps holding for outages; this amends it for the one-time preparation only. The answer MUST come from the recorded state, never from a libvirt call, and its reason MUST name the hypervisor without error detail, as the database reason does. Stub mode's readiness MUST NOT change.
- **FR-017a**: The log MUST show the first failure with its cause (as today), a line when the preparation succeeds after having failed — naming the directory and QEMU's user — and retries at debug level only, so a long outage does not flood the log.
- **FR-018**: A unit test MUST show that a first capabilities read that times out, followed by one that succeeds, lets the next guest start succeed without a restart, with its per-instance directory owned by QEMU's user. Tests MUST also show that each kind of guest start — create, start, hard reboot, resume, self-heal — makes a root-owned per-instance directory QEMU's before the guest launches.
- **FR-018a**: Readiness tests MUST show: in real mode, not-ready before the preparation succeeds, with a reason naming the hypervisor; ready after it; still ready after a later loss of libvirt; and stub mode unchanged.

**libvirt unreachable at startup (User Story 6)**

- **FR-019**: In real mode Lightstack MUST NOT report a server it did not create. Until its first libvirt connection it MUST behave as it does while a lost connection is being re-established (SPEC-025): a server create is accepted and ends in `ERROR`, never `ACTIVE`, with a `fault` — every real-mode create that ends `ERROR` records one, its message curated as the network-allocation fault's is, the detail in the log; a server action answers the `500` its handler already gives a lost connection — "failed to perform <action>" for lifecycle actions, the handler's own generic message for delete, console and volume attach or detach — the cause in the log; a guest start meets FR-016's refusal first, since the preparation cannot have succeeded. Nothing takes the stub-mode path, and no new HTTP status or action message is introduced.
- **FR-020**: When the server's first connection fails, it MUST keep dialing in the background with the same backoff it uses after a lost connection, making the socket choice of FR-007 on every attempt. Once connected, it MUST run the preparation (FR-013); the pod becomes Ready when that succeeds (FR-017), and guests MUST start without a restart.
- **FR-020a**: `lightstack cleanup` MUST keep SPEC-010's fail-closed contract: one connection attempt — dial and handshake bounded at 1 second, as Article III requires — with the socket choice of FR-007; if it fails, the purge exits non-zero at once, naming the socket or sockets it tried, and the uninstall stops. It MUST NOT dial in the background or retry. Tests MUST show it failing without waiting when the socket is absent, and within the bound when the daemon does not answer.

**Validation and documentation**

- **FR-021**: Host validation through the chart on k3s MUST confirm:
  - (a) real mode with host networking and a Service-name PostgreSQL URL becomes Ready, and a delete-policy uninstall completes;
  - (b) on a host with only modular daemons — no `libvirtd`, no `virtproxyd`, no symlink — Lightstack connects with default values, a VM boots to `ACTIVE`, and a delete-policy uninstall destroys it;
  - (c) the cold path, forced: with `virtqemud` stopped and its QEMU capabilities cache cleared before Lightstack starts, the log shows the failed first read and the later success, the pod becomes Ready, and the first guest boots to `ACTIVE` with no restart. If a fast host answers even the cold read within 1 s, the step records that the retry went unexercised, and FR-018's unit test remains the proof;
  - (d) the unreachable path, forced: with libvirt's sockets stopped when Lightstack starts, a create ends in `ERROR` and the pod is not Ready; once the sockets are started, the log shows the failed first attempt, the later connection and the socket dialed, the pod becomes Ready, and a VM boots to `ACTIVE` with no restart;
  - (e) the monolithic path: a default install against monolithic `libvirtd` — the same host switched over will do — logs `libvirt-sock` as the socket dialed and boots a VM to `ACTIVE`.
- **FR-022**: The libvirt documentation MUST match the result: `docs/CONFIGURATION.md` lists the accepted `libvirt_uri` forms and the automatic choice instead of the session and remote forms it lists today; the `qemu+tcp://` example in `docs/SCALING.md` goes; the `realMode.libvirtSocketPath` documentation follows FR-005; the operator docs that check for `libvirt-sock` (`docs/TROUBLESHOOTING.md`, `docs/REAL_LIBVIRT_MODE.md`, `docs/REAL_MODE_TESTING.md`, `docs/HORIZON_INTEGRATION.md`) also name `virtqemud-sock`.
- **FR-023**: Every description of the readiness inputs MUST add the real-mode condition of FR-017: `charts/lightstack/CONTRACT.md` (§2), the chart README's health table, `templates/NOTES.txt`, the `probes` comments in `values.yaml` and descriptions in `values.schema.json`, and `docs/blueprint.md`.
- **FR-024**: `docs/UPGRADING.md` MUST state what an existing real-mode release meets:
  - an unsupported `libvirt_uri` now stops startup;
  - a pod that cannot prepare the console socket directory is no longer Ready — its startup probe restarts it — where before it was Ready and refused every guest start;
  - with host networking, the pods now resolve through the cluster DNS, so a name only the node's own resolver can answer must be reachable through it;
  - the host workarounds can be removed: the `libvirt-sock` symlink, enabling `virtproxyd.socket`, and warming libvirt before Lightstack starts.

### Key Entities

- **libvirt connection setting** (`nova.libvirt_uri`): how the binary finds libvirt. Accepted: the system URI (automatic socket choice), the same with an explicit socket, a bare absolute socket path. Anything else stops real-mode startup.
- **Socket choice**: the socket the binary dials for the system URI, decided at each connection attempt — `virtqemud-sock` when present, otherwise `libvirt-sock`.
- **Chart socket value** (`realMode.libvirtSocketPath`): the socket whose directory the pods mount. A non-default value is also the socket dialed; the default lets the binary choose.
- **Guest start**: any operation that launches a guest's QEMU process — a create (image or volume boot), a start, a hard reboot, a resume, and a self-heal re-creation. Each needs the preparation done (FR-013, FR-016) and its per-instance directory owned by QEMU's user (FR-014).
- **Console socket directory preparation**: either not yet succeeded, with the last cause, or succeeded, with the user QEMU runs as. Retried until it succeeds, redone after each reconnect, and required before any guest starts. In real mode, readiness waits for its first success, and only for that.
- **Host-networked workload**: the Lightstack Deployment or DaemonSet pod and the teardown purge Job, when real mode runs with host networking. The console proxy already resolves cluster names; the quiesce Job does not run on the host network.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In real mode with host networking, Lightstack and its purge Job reach every in-cluster dependency by Service name: an install whose database URL names a Service becomes Ready, and a delete-policy uninstall completes, with no cluster IP or node address in any dependency's URL.
- **SC-002**: On a host running only libvirt's modular daemons, a chart install with default values boots a VM to `ACTIVE` on the first attempt, and a delete-policy uninstall removes it — with zero host-side workarounds: no proxy daemon, no symlink, no warm-up.
- **SC-003**: On a host running the monolithic daemon, a default install dials `libvirt-sock` and boots guests as before; its default render differs only by the DNS policy line under host networking.
- **SC-004**: After a startup at which libvirt answered slowly, the pod becomes Ready and guest starts work without a restart: the first guest created boots to `ACTIVE`. Zero restarts, zero warm-up steps.
- **SC-004a**: In real mode a Ready pod has a prepared console socket directory: zero cases of a pod that is Ready while every guest start is refused with `console socket directory not ready`.
- **SC-005**: A non-default socket set in the chart is the socket both the Lightstack pod and the purge Job dial.
- **SC-006**: Every unsupported `libvirt_uri` stops real-mode startup with a message naming the value and the accepted forms; none is dialed as a file.
- **SC-007**: In real mode, no server created while libvirt is unreachable is ever reported `ACTIVE`, and every one that ends `ERROR` shows a `fault` saying why.
- **SC-008**: Every acceptance criterion of the issue has an automated test or a host-validation step, and the existing unit, contract and chart suites stay green on both database backends.

## Assumptions

- The pods affected by Problem 1 are the Lightstack workload (Deployment or DaemonSet) and the purge Job. The console proxy already sets the policy; the quiesce Job runs `kubectl` without host networking.
- `ClusterFirstWithHostNet` is the standard Kubernetes policy for this case. No chart value for the DNS policy or DNS config is added.
- The default `realMode.libvirtSocketPath` keeps its value, and keeps meaning "mount `/var/run/libvirt`"; it now also means "let the binary choose". Only a different value is passed through (FR-004).
- libvirt's run directory is `/var/run/libvirt`; distributions that use `/run` link `/var/run` to it, and the mounted directory is the same either way.
- Lightstack needs only the domain and connect APIs (`Domain*`, `ConnectGetCapabilities`, `ConnectGetLibVersion`), all served by the QEMU daemon, so `virtqemud-sock` alone is sufficient — no `virtproxyd`, no other modular daemon.
- The background retry reuses the existing reconnect backoff (1 s, doubling to 5 s). The 1-second bound per libvirt call is unchanged.
- The startup probe's existing budget (30 × 5 s, about 150 s) is the grace for a slow libvirt at boot. No new chart value; an operator with a slower host raises `probes.startup.failureThreshold`.
- SPEC-013's own documents are not edited; the amendment of its FR-001 is recorded here (Clarifications, FR-017).
- An unsupported `libvirt_uri` is a configuration error, so it is refused when the configuration loads, like the other real-mode checks there (`internal/common/config.go`), not degraded around.
- No database schema change, no migration, no new Go module, no new chart value.

## Out of Scope

- Remote libvirt (TCP, TLS, SSH) and session URIs: refused with a clear message, not implemented.
- A chart value for an arbitrary DNS policy or DNS config.
- Guest DNS: dnsmasq serves DHCP only, and guests get the subnet's nameservers.
- Readiness that follows libvirt outages: once the preparation has succeeded, SPEC-013 FR-001 holds, and an outage does not unready the pod.
- Reporting compute's hypervisor or service state as `down` (both are hard-coded `up` today).
- Host firewall (firewalld, nftables) and SELinux interplay with real mode.

## Dependencies

- Kubernetes `dnsPolicy: ClusterFirstWithHostNet` (https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#pod-s-dns-policy).
- libvirt's modular daemons and their socket names, `/var/run/libvirt/virt${DRIVER}d-sock` (https://libvirt.org/daemons.html), and the `socket` URI parameter (https://libvirt.org/uri.html).
- The existing libvirt reconnect loop (`keepConnected`) and the 1-second bound (`consoleReadTimeout`) in `pkg/hypervisor`.
- The chart's helm-unittest harness; host validation needs k3s and a host running only libvirt's modular daemons, switchable to monolithic `libvirtd` for FR-021 (e).
