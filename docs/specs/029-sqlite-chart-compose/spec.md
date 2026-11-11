# Feature Specification: SQLite backend, part 2 — Helm chart and Docker Compose

**Feature Branch**: `feature/599` (feature id: `029-sqlite-chart-compose`)
**Created**: 2026-10-28
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#599 — "[FEAT] - lightstack - Database - SQLite 2/3: Helm chart and Docker Compose". Part 2 of 3 of the epic cobaltcore-dev/cloud-storage#595 (SQLite as an alternative to PostgreSQL), which holds the design, the decisions, the constraints and the accepted drawbacks. Part 1 (cloud-storage#598, `028-sqlite-database-layer`) added the database layer, the migrations, the binary guards and the node-local path helper. Part 3 (cloud-storage#600) adds backup, integrity check and restore.

## Context

Part 1 taught the binary to keep its state in one SQLite file on the node: the database URL's scheme picks the backend, the SQLite path helper `pkg/paths.DBDir()` exists, the binary refuses a `sqlite://` URL with multi-node networking, it refuses a URL whose scheme disagrees with `LIGHTSTACK_DB_BACKEND` when that variable is set, and `lightstack cleanup` opens SQLite must-exist so a missing file is never created. The real-mode Docker Compose stack (`deployments/docker-compose-kvm.yml`) already runs either backend. The chart already sets `LIGHTSTACK_NODE_NAME` from the pod's node name and feeds the teardown purge Job the database URL from `database.existingSecret`.

What part 1 did **not** do: it left the Helm chart and the stub-mode Docker Compose file on PostgreSQL only. The chart deploys no database and documents an external PostgreSQL as required (`values.schema.json`: "The chart deploys no database (FR-006)"); the main `deployments/docker-compose.yml` runs a postgres service. The node-local path helper `DBDir()` is wired into nothing — not the chart's directory-creation list, not the node-local purge.

This feature makes SQLite a production backend of the Helm chart and of Docker Compose. With SQLite the database is one file beside the node-local state under `realMode.hostStoragePath` (default `/var/lib/lightstack`), on a hostPath, with no PVC. The database survives container restarts, pod recreation, `helm upgrade` and node reboots, and is lost with the node — accepted, as the VM disks in real mode already are. Because the database lives on one node, the chart guards the cases that would lose it or split it: it refuses SQLite without a node pin, with multi-node, or with more than one replica, and it schedules the uninstall purge on the same node as the workload.

Two facts shape the work:

- **The chart cannot read a Secret.** SQLite is therefore selected only through `database.url` in values, where the chart can see the scheme and render the right storage. A `sqlite://` URL hidden in `database.existingSecret` or `secrets.existingSecret` must still be caught: the chart sets `LIGHTSTACK_DB_BACKEND` on every container that runs the binary, defaulting to `postgres` whenever it did not itself see a SQLite URL, and the binary (part 1) refuses the mismatch at startup.
- **SQLite is single-writer.** One pod opens the file at a time. A pod rescheduled to another node, or a second replica, or an upgrade that starts the new pod before the old one stops, would open an empty or a second database — SQLite creates one without an error. The chart prevents each: a required node pin, the single-node/single-replica refusals, and `strategy: Recreate` for SQLite in both modes.

## Clarifications

### Session 2026-10-28

- Q: Where does the stub-mode SQLite Docker Compose stack live, and what does it mount? → A: **A new `deployments/docker-compose-sqlite.yml`.** It drops the postgres service, keeps Redis optional as the default file does, puts the database file on a named volume mounted at the database directory, and sets `LIGHTSTACK_DB_URL=sqlite:///…`. The default `deployments/docker-compose.yml` stays PostgreSQL and is not changed. `LIGHTSTACK_DB_BACKEND` is left unset (part 1: unset means no check).
- Q: In stub mode the chart mounts no `host-storage` volume today. What does SQLite add there? → A: **One hostPath volume for the database directory**, with `type: DirectoryOrCreate`, on the workload pod and on the purge Job. In real mode the database directory is already inside the existing `host-storage` mount, so only the `init-host-storage` mkdir list gains a `db` entry. A namespace enforcing the `baseline` or `restricted` Pod Security level rejects the hostPath volume — an accepted drawback, inherited from real mode.
- Q: Which containers carry `LIGHTSTACK_DB_BACKEND`? → A: **Every container that runs the Lightstack binary** — the workload container (Deployment, and the DaemonSet path even though SQLite is refused there) and the teardown purge Job — through a shared helper, mirroring `lightstack.dbUrlEnv`. The quiesce Job runs `kubectl`, not the binary, and carries nothing.
- Q: Does the node pin the chart requires count node affinity, or only `nodeSelector`? → A: **Either.** A non-empty `nodeSelector` or a node affinity pins the pod; the refusal fires only when both are absent, as `values.yaml` ships them today (`nodeSelector: {}`, `affinity: {}`).
- Q: `db/` is added to the node-local purge. When is it removed relative to reading the database? → A: **Last, and only after the enumeration connection is closed.** The purge reads the database to enumerate the release's VMs, closes it, purges the other node-local trees, and removes `db/` (`lightstack.db`, `-wal`, `-shm`) last.
- Q: Does the purge-Job node pin apply only to SQLite? → A: **Both backends.** The scheduling block (nodeSelector/affinity/tolerations) moves into a shared helper included by `lightstack.podSpec` and by the purge Job, so the Job always runs where the workload does. For SQLite this is what lets it reach the database on the node; for PostgreSQL it is a harmless correctness fix. A helm-unittest case asserts the Job's scheduling equals the Deployment's.
- Q: A SQLite `database.url` is combined with a Secret-based config (`database.existingSecret` or `secrets.existingSecret`), which the Secret then overrides at runtime. How does the chart behave? → A: **Fail at render time, naming the conflict.** SQLite is selected only through `database.url`; combining it with a Secret-based config is self-contradictory (the chart would render SQLite storage for a URL the Secret overrides). The chart refuses it at render time, consistent with its other SQLite guards, rather than leaving the binary to refuse it later at startup.
- Q: How wide is the stub-mode SQLite hostPath mount — the whole state root (as real mode mounts) or only the database directory? → A: **The workload mounts only the database directory** (`<hostStoragePath>/db`) — least privilege, since a stub workload reads and writes only its database. **The purge Job mounts the whole state root** (`<hostStoragePath>`): `lightstack cleanup` does `RemoveAll(db/)`, which fails when `db/` is itself the mountpoint, so `db/` must be a removable subdirectory of the mount — and the purge legitimately needs the state root, since it removes every node-local tree under it. (Correction, 2026-10-28: an earlier answer scoped the purge Job to the database directory too; host testing showed that makes `RemoveAll(db/)` fail with the directory busy, leaving the files gone but the Job non-zero.) Real mode is unchanged — the database directory already sits inside the existing `host-storage` mount of the whole state root.
- Q: SQLite needs working POSIX file locks, so the hostPath must be a local filesystem (a network filesystem such as NFS or CephFS can silently corrupt it). The chart cannot detect the filesystem type. How is this handled? → A: **Documented, as an explicit requirement.** The SQLite docs and the `values.yaml` comment MUST warn that `realMode.hostStoragePath` must be a local POSIX filesystem, not NFS or CephFS. It cannot be enforced at render time, so a clear warning is the mitigation.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Install Lightstack on SQLite with the Helm chart on k3s (Priority: P1)

An operator of a single k3s node sets `database.url: sqlite:///var/lib/lightstack/db/lightstack.db` and pins the pod to the node (`nodeSelector: { kubernetes.io/hostname: <node> }`). `helm install` renders a hostPath volume for the database directory, no PVC, and `strategy: Recreate`. Lightstack creates the directory and the database, applies the schema and seed data, and serves every service. No PostgreSQL runs anywhere.

**Why this priority**: It is the feature's point on Kubernetes — Lightstack on k3s with no external database.

**Independent Test**: On a single-node k3s with no PostgreSQL, install the chart with a SQLite URL and a node pin. Confirm the pod starts, migrates and answers. Delete the pod and confirm it comes back on the same database; run `helm upgrade` and confirm the data survives.

**Acceptance Scenarios**:

1. **Given** a SQLite URL under `realMode.hostStoragePath` and a node pin, **When** the chart is installed, **Then** it renders a hostPath volume (`DirectoryOrCreate`) for the database directory, no PVC, `strategy: Recreate`, and `LIGHTSTACK_DB_BACKEND=sqlite` on the workload container.
2. **Given** that release, **When** the pod starts, **Then** Lightstack creates the database, migrates it, and every service answers — with no PostgreSQL in the namespace.
3. **Given** a running SQLite-backed release, **When** the pod is deleted and recreated, **Then** it opens the same database on the node and every resource created before is still there.
4. **Given** a running SQLite-backed release, **When** `helm upgrade` runs, **Then** the old pod stops before the new one opens the database (Recreate), migrations apply once, and the data survives.
5. **Given** real mode, **When** the chart renders, **Then** the `init-host-storage` initContainer creates the `db` directory alongside the other node-local subdirectories.

---

### User Story 2 — Run Lightstack on SQLite with Docker Compose (Priority: P1)

An operator brings up the stub-mode stack from a SQLite Compose file: no postgres service, the database file on a named volume, `LIGHTSTACK_DB_URL=sqlite:///…`. Lightstack migrates, starts and serves. The default PostgreSQL Compose file is unchanged.

**Why this priority**: Docker Compose is the other supported deployment, and the one most contributors run locally.

**Independent Test**: `docker compose -f deployments/docker-compose-sqlite.yml up`. Confirm there is no postgres container, the database file lands on the named volume, and `openstack token issue` works. Confirm `deployments/docker-compose.yml` still runs PostgreSQL.

**Acceptance Scenarios**:

1. **Given** the SQLite Compose file, **When** the stack starts, **Then** no postgres service runs, the database file is created on the named volume, and every service answers.
2. **Given** the stack stopped and started again, **When** it comes back, **Then** the named volume still holds the database and the resources created before are present.
3. **Given** the default `deployments/docker-compose.yml`, **When** it is brought up, **Then** it runs PostgreSQL exactly as before.

---

### User Story 3 — PostgreSQL deployments render exactly as before (Priority: P1)

An operator running Lightstack on PostgreSQL — through the chart or Docker Compose — upgrades to this version and notices nothing. The chart renders the same manifests for `postgres://` values; the default Compose file is untouched.

**Why this priority**: PostgreSQL is the default and today's production path. A regression there costs more than SQLite gains.

**Independent Test**: Run helm-unittest for PostgreSQL values before and after the change and diff the rendered output. Confirm `deployments/docker-compose.yml` is unchanged.

**Acceptance Scenarios**:

1. **Given** PostgreSQL values (a `postgres://` `database.url`, or a `database.existingSecret`), **When** the chart renders, **Then** the workload, storage and strategy are identical to today, with `LIGHTSTACK_DB_BACKEND=postgres` added.
2. **Given** the helm-unittest suite, **When** it runs for PostgreSQL values, **Then** it shows no change beyond the added `LIGHTSTACK_DB_BACKEND` env and the purge-Job scheduling helper.
3. **Given** the default Compose file, **When** the change is complete, **Then** it still defines a postgres service and the PostgreSQL `LIGHTSTACK_DB_URL`.

---

### User Story 4 — The chart refuses unsafe SQLite configurations (Priority: P2)

An operator who points SQLite at a path outside the node storage, omits a node pin, sets multi-node or more than one replica, or hides a SQLite URL in a Secret is stopped — at render time where the chart can see the mistake, and at startup where only the binary can.

**Why this priority**: SQLite on one node splits or loses the cloud's state if any of these slips through, and silently — SQLite creates a fresh database without an error.

**Independent Test**: Render the chart once per refused configuration and confirm each fails with a message naming the problem. Deploy with a SQLite URL in a Secret and confirm the binary refuses to start.

**Acceptance Scenarios**:

1. **Given** a SQLite `database.url` whose file path is not under `realMode.hostStoragePath`, **When** the chart renders, **Then** it fails, naming the path and the storage root.
2. **Given** a SQLite `database.url` with neither a `nodeSelector` nor a node affinity, **When** the chart renders, **Then** it fails, requiring a node pin.
3. **Given** a SQLite `database.url` with `deploymentMode` other than `single-node`, or with `replicaCount > 1`, **When** the chart renders, **Then** it fails, naming the setting.
4. **Given** a `sqlite://` URL supplied through `database.existingSecret` or `secrets.existingSecret` (invisible to the chart), **When** the chart renders, **Then** it sets `LIGHTSTACK_DB_BACKEND=postgres`, and the binary refuses to start, naming both values.
5. **Given** a `sqlite://` `database.url` combined with `database.existingSecret` or `secrets.existingSecret`, **When** the chart renders, **Then** it fails, naming the conflict.
6. **Given** PostgreSQL values with the same multi-node, replica or missing-node-pin settings, **When** the chart renders, **Then** none of the SQLite refusals fires.

---

### User Story 5 — Uninstall and retention protect the SQLite database (Priority: P2)

An operator uninstalls a SQLite release. Under the delete policy the purge runs on the node that holds the database, reads it to find the release's VMs, removes them and the other node-local state, and removes the database directory last. Under the retain policy nothing runs and the database stays; a reinstall on the same node resumes from it. A purge that somehow reaches a node without the database warns and exits cleanly, leaving the VMs running on the right node.

**Why this priority**: The database is on a hostPath the uninstall must reach correctly; a purge on the wrong node, or one that removes the database before reading it, would lose data or leave VMs orphaned.

**Independent Test**: Install a SQLite release, create resources, uninstall with `dataRetentionPolicy: delete`, and confirm the VMs and the database directory are gone and removed in that order. Repeat with `retain` and confirm the database stays and a reinstall on the same node picks it up. Render the purge Job and confirm its scheduling equals the Deployment's.

**Acceptance Scenarios**:

1. **Given** a SQLite release and `dataRetentionPolicy: delete`, **When** it is uninstalled, **Then** the purge Job is pinned to the workload's node, reads the database, removes the release's VMs and node-local trees, and removes the database directory last.
2. **Given** `dataRetentionPolicy: retain`, **When** the release is uninstalled, **Then** nothing runs and `db/` stays on the node; a reinstall on the same node opens the database and resumes.
3. **Given** a purge that reaches a node holding no database (must-exist open fails), **When** it runs, **Then** it warns naming the path, purges that node's host state and exits 0, and the VMs on the correct node keep running.
4. **Given** the chart, **When** the purge Job and the Deployment are rendered, **Then** their nodeSelector, affinity and tolerations are equal (asserted by a helm-unittest case), on both backends.

### Edge Cases

- **A path outside the storage root** is refused at render time (FR-002), so the chart never mounts an arbitrary host path.
- **A rescheduled pod** finds an empty directory and SQLite would create a fresh database; the required node pin (FR-008) prevents it.
- **`helm upgrade`** could otherwise start the new pod before the old one releases the file; `strategy: Recreate` for SQLite in both modes (FR-007) serializes them, at the cost of a brief API outage — accepted.
- **Stub mode gains a hostPath volume** for the database directory; a namespace enforcing the `baseline` or `restricted` Pod Security level rejects the pod — accepted, as real mode already requires hostPath.
- **A hard kill** leaves `-wal` and `-shm` beside `lightstack.db`; under `retain` a reinstall on the same node opens the database and replays them. The three files are never moved or copied one at a time.
- **A network filesystem under `hostStoragePath`** (NFS, CephFS) can break SQLite's POSIX locking and corrupt the database; the chart cannot detect it, so the docs and `values.yaml` warn against it (FR-019a).
- **The purge removes `db/` before closing the enumeration connection** — must not happen; `db/` is removed last, after the connection is closed (FR-013).
- **WAL sidecar files** (`lightstack.db-wal`, `lightstack.db-shm`) live next to the database; the mount is the directory, not the file (FR-004), so they are visible to the pod.
- **An operator on SQLite who also enables multi-node VXLAN** is refused by the binary at startup (part 1), independent of the chart's render-time refusals.

## Requirements *(mandatory)*

### Functional Requirements

**Chart: backend selection and SQLite storage**

- **FR-001**: The chart MUST accept a `sqlite:///<absolute path>` value for `database.url` and select the backend from that URL's scheme — the only place it can see the scheme. When `database.url` is a SQLite URL the chart MUST render SQLite storage; otherwise it MUST render for PostgreSQL. The `values.yaml` comment and the `values.schema.json` description for `database.url` MUST document the SQLite form, and the schema MUST NOT reject a `sqlite://` URL.
- **FR-002**: For SQLite, the file path in `database.url` MUST lie under `realMode.hostStoragePath`; a path outside it MUST fail at render time with a message naming the path and the storage root, so the chart never mounts an arbitrary host path.
- **FR-003**: The chart MUST set `LIGHTSTACK_DB_BACKEND` on every container that runs the Lightstack binary — the workload container and the teardown purge Job — through a shared helper: `sqlite` when the chart rendered SQLite storage, `postgres` otherwise. Because the chart cannot read a Secret, a `sqlite://` URL supplied through `database.existingSecret` or `secrets.existingSecret` renders `LIGHTSTACK_DB_BACKEND=postgres`, and the binary (part 1, SPEC-028 FR-004) refuses the mismatch at startup.
- **FR-003a**: The chart MUST fail at render time when a `sqlite://` `database.url` is combined with `database.existingSecret` or `secrets.existingSecret`, naming the conflict. SQLite is selected only through `database.url`, and a Secret-based config (whose URL overrides the rendered config at runtime) contradicts it; the chart refuses the combination rather than rendering SQLite storage for a URL the Secret would override.
- **FR-004**: For SQLite, the chart MUST mount the database's directory — the directory containing the file named in `database.url` — as a hostPath volume with `type: DirectoryOrCreate`, so `lightstack.db`, `lightstack.db-wal` and `lightstack.db-shm` share one mounted directory. In real mode the directory lies inside the existing `host-storage` mount of the whole state root, and `db` MUST join the `init-host-storage` mkdir list (which mirrors `pkg/paths/paths.go`). In stub mode, where `host-storage` is not mounted today, the workload pod MUST gain a hostPath volume and mount scoped to the database directory only (`<hostStoragePath>/db`), not the whole state root — a stub pod has no other node-local storage to reach.
- **FR-005**: The chart MUST keep the container running as root and MUST NOT add `runAsUser`; kubelet creates the hostPath directory as root and the image sets no `USER`.
- **FR-006**: For SQLite the chart MUST NOT create a PVC or expose any PVC option (creation, existing claim, storage class, size). The database shares the node's disk with images and volumes.
- **FR-007**: For SQLite the Deployment MUST use `strategy: Recreate` in both stub and real mode, so the old pod releases the database before the new pod opens it and runs migrations. PostgreSQL MUST keep today's strategy: `Recreate` in real mode, `RollingUpdate` in stub mode.

**Chart: single-instance guards**

- **FR-008**: The chart MUST refuse to render SQLite (fail at render time) when the pod is pinned to no node — neither a non-empty `nodeSelector` nor a node affinity — with a message requiring a node pin.
- **FR-009**: The chart MUST refuse to render SQLite (fail at render time) when `deploymentMode` is other than `single-node`, or when `replicaCount` is greater than 1, naming the setting.

**Chart: teardown and retention**

- **FR-010**: The pod-scheduling block — `nodeSelector`, `affinity`, `tolerations` — MUST move into a shared helper included by `lightstack.podSpec` and by the teardown purge Job, for both backends, so the purge Job is scheduled exactly as the workload. A helm-unittest case MUST assert the Job's scheduling equals the Deployment's.
- **FR-011**: Under `dataRetentionPolicy: delete`, the purge Job MUST mount the node-local **state root** (`realMode.hostStoragePath`) — for SQLite in stub mode as well as real mode — so it can read the database and then remove the `db/` directory as a subdirectory of the mount (a direct `db/` mount makes `db/` a mountpoint that `RemoveAll` cannot unlink, failing the purge). It already receives `LIGHTSTACK_DB_URL` from `database.existingSecret` (SPEC-028 FR-044) and `LIGHTSTACK_NODE_NAME` from the pod's node name (SPEC-028 FR-043); no change is needed there.
- **FR-012**: When the purge Job reaches a node that holds no database, `lightstack cleanup`'s must-exist open (SPEC-028 FR-012) MUST fail rather than create one; cleanup then warns naming the path, purges that node's host state and exits 0, leaving the VMs on the correct node and completing the uninstall.
- **FR-013**: `purgeLocalStorage` (`internal/teardown/teardown.go`) MUST add the database directory (`paths.DBDir()`) to the node-local purge, removed last, after the enumeration connection is closed — so `lightstack.db`, `lightstack.db-wal` and `lightstack.db-shm` are removed together and only once the database is no longer open.
- **FR-014**: Under `dataRetentionPolicy: retain`, nothing MUST run at uninstall and `db/` MUST stay on the node as it is; a reinstall on the same node opens the database and replays any `-wal`/`-shm` left by a hard kill. The three files MUST never be moved or copied individually.

**Docker Compose**

- **FR-015**: A new SQLite variant of `deployments/docker-compose.yml` MUST run the stub-mode stack with no postgres service, the database file on a named volume mounted at the database directory, and `LIGHTSTACK_DB_URL=sqlite:///…`. The default `deployments/docker-compose.yml` MUST stay PostgreSQL and MUST NOT change. (The real-mode KVM stack already runs either backend — SPEC-028 FR-042.)

**Tests and documentation**

- **FR-016**: helm-unittest cases MUST cover both backends for the SQLite work: the storage volume and mount, `LIGHTSTACK_DB_BACKEND`, `strategy: Recreate`, the `db` mkdir entry in real mode, each render-time refusal (path outside the storage root, missing node pin, non-single-node, replica count over 1), and the purge-Job scheduling equalling the Deployment's.
- **FR-017**: A PostgreSQL release MUST render as before: helm-unittest MUST show no change for `postgres://` values beyond the added `LIGHTSTACK_DB_BACKEND=postgres` env and the shared scheduling helper.
- **FR-018**: Real-mode single-node host validation on k3s with SQLite MUST confirm: the chart installs with a SQLite URL and a node pin; Lightstack migrates, starts and serves; the database survives a pod restart and a `helm upgrade`; and `dataRetentionPolicy` delete and retain behave as FR-013 and FR-014 require.
- **FR-019**: Documentation MUST describe the result: `docs/KUBERNETES_DEPLOYMENT.md` (SQLite on k3s — the SQLite URL, the required node pin, the hostPath, no PVC, `Recreate`, and the delete/retain behaviour), `docs/DOCKER_DEPLOYMENT.md` (the SQLite Compose variant), and the chart's `values.yaml` comments.
- **FR-019a**: The SQLite documentation and the chart's `values.yaml` comment MUST warn that `realMode.hostStoragePath` must be a local POSIX filesystem, not a network filesystem such as NFS or CephFS: SQLite needs working POSIX file locks, which a network filesystem can fail to provide and thereby corrupt the database. The chart cannot detect this at render time, so the warning is the mitigation.

### Key Entities

- **SQLite database directory**: the hostPath directory (`<hostStoragePath>/db`, `paths.DBDir()`) that holds `lightstack.db`, `lightstack.db-wal` and `lightstack.db-shm`. Mounted as a directory, `DirectoryOrCreate`, in both modes; shared with images and volumes on the node's disk.
- **Declared backend env** (`LIGHTSTACK_DB_BACKEND`): the value the chart sets on every binary-running container so the binary can verify a URL the chart cannot see. `sqlite` only when the chart rendered SQLite storage, `postgres` otherwise.
- **Node pin**: a non-empty `nodeSelector` or a node affinity that binds the single pod (and the purge Job) to one node. Required for SQLite.
- **Scheduling helper**: the shared `nodeSelector`/`affinity`/`tolerations` block, included by `lightstack.podSpec` and by the purge Job.
- **Data retention policy** (`dataRetentionPolicy`): `delete` purges the release's VMs and the database directory (last); `retain` leaves the database on the node for a later reinstall.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a SQLite database URL under the node storage path and a node pin, Lightstack migrates, starts and serves every service with no PostgreSQL running anywhere — installed by the Helm chart on single-node k3s, and by Docker Compose.
- **SC-002**: The chart refuses, at render time, SQLite combined with a path outside the node storage path, no node pin, a deployment mode other than single-node, or more than one replica — each failure names the problem.
- **SC-003**: A URL that selects SQLite but is hidden in a Secret causes the deployed binary to refuse to start, naming the declared backend and the URL's backend.
- **SC-004**: A SQLite database survives a pod restart and a chart upgrade — every resource created before the event is present afterwards.
- **SC-005**: Uninstall with the delete policy removes every VM and the database directory, the database directory last; uninstall with the retain policy leaves the database in place, and a reinstall on the same node resumes from it.
- **SC-006**: An existing PostgreSQL release renders identically — the chart's unit tests show no difference for PostgreSQL values beyond the added declared-backend env and the shared scheduling helper.
- **SC-007**: The teardown purge Job is scheduled on the same node as the workload (asserted by a unit test); a purge that reaches a node without the database warns and exits cleanly, leaving the VMs running.

## Assumptions

- The stub-mode SQLite Compose file is named `deployments/docker-compose-sqlite.yml` and keeps the default file's Redis service and port layout; only the database changes.
- The chart derives the mounted database directory from the file path in `database.url` (its parent directory). The recommended URL is `sqlite:///var/lib/lightstack/db/lightstack.db`, giving `/var/lib/lightstack/db`.
- "Every container that runs the binary" means the workload container and the teardown purge Job. The quiesce Job (which runs `kubectl`) and the console-proxy container do not open the database and carry no declared-backend env.
- The node pin the chart requires is satisfied by either a non-empty `nodeSelector` or a node affinity; both being absent (the shipped defaults) is the refused state.
- The binary-side guards this feature relies on — the `LIGHTSTACK_DB_BACKEND` scheme check, the multi-node VXLAN refusal, the must-exist cleanup open, and `pkg/paths.DBDir()` — are delivered by part 1 (SPEC-028) and are not re-implemented here.
- No new database schema, migration or Go dependency is introduced. The only Go change is adding the database directory to `purgeLocalStorage`.

## Out of Scope

- Backup, integrity check and restore — part 3 (cloud-storage#600).
- The database layer, query translation, migrations and binary guards — part 1 (cloud-storage#598), already delivered.
- Everything cloud-storage#595 lists as out of scope: moving data between backends, multi-node or HA on SQLite, SQLite as the default, other backends, a per-service repository layer, an ORM.
- Changing the other PostgreSQL Compose files (`docker-compose-horizon.yml`, `docker-compose.rgw-test.yml`, `docker/docker-compose.yaml`, `horizon/docker-compose.yaml`) — they remain PostgreSQL test stacks.
