# Phase 0 Research: SPEC-008 Helm Charts

The spec is decision-complete (8 clarification rounds). This records the *how* — implementation decisions grounded in the current codebase and the landed SPEC-007. No open `NEEDS CLARIFICATION` items remain (the one deferred clarify item, minimum Kubernetes version, is resolved in R3).

## R1 — Chart layout & distribution

**Decision**: A single chart at `charts/o3k/`, installed from a local path/checkout (`helm install o3k ./charts/o3k`). No registry publishing in this feature; structure must not preclude adding it (FR-019).

**Rationale**: Matches the in-repo clarification; lowest setup. A standard chart layout (Chart.yaml/values.yaml/values.schema.json/templates/) is registry-publishable later with no rework.

## R2 — Configuration delivery (values → product config)

**Decision**: Render the **full** `o3k.yaml` from values into a **Secret** and mount it at the binary's `--config` path (`/app/config/o3k.yaml`); this one file carries all settings, including the sensitive `keystone.jwt_secret`, `keystone.admin_password`, and `database.url`. Use environment variables only for the four overrides the binary actually honors: `O3K_DB_URL` and `O3K_JWT_SECRET` (`internal/common/config.go` `LoadConfig`) plus `O3K_ENDPOINT_HOST` and `O3K_ENDPOINT_SCHEME` (read at request time). Those env vars — sourced from a Secret — are the wiring for `existingSecret` and the TLS scheme; everything else (admin password, service modes, vxlan, cache, logging) is set in the rendered file.

**Rationale**: Verified against the code — `LoadConfig` parses the YAML `--config` file and then applies **only** `O3K_DB_URL` → `database.url` and `O3K_JWT_SECRET` → `keystone.jwt_secret`. There is **no** generic `O3K_<SECTION>_<KEY>` / viper `AutomaticEnv` layer, so `admin_password`, `libvirt_mode`, `networking_mode`, `storage_mode`, `vxlan_enabled`, `cache.*`, etc. take effect **only** from the file. Rendering the file into a Secret keeps every credential out of any plaintext ConfigMap without depending on a non-existent env layer.

**Product divergence (flag)**: `docs/CONFIGURATION.md` documents a broad `O3K_<SECTION>_<KEY>` env convention (e.g. `O3K_KEYSTONE_ADMIN_PASSWORD`, `O3K_NOVA_LIBVIRT_MODE`, `O3K_NEUTRON_VXLAN_ENABLED`) that the code does **not** implement. A follow-up product fix should either wire a real env-override layer or correct the docs; this chart design needs neither.

**Alternative rejected**: a ConfigMap for non-secret values + env-injected secrets — unsound, because the binary ignores all but the four env vars above (notably it never reads `O3K_KEYSTONE_ADMIN_PASSWORD`, so the admin-password gate would silently fall back to the file's default `secret`).

## R3 — Minimum Kubernetes version

**Decision**: **Kubernetes ≥ 1.25** (and Helm ≥ 3.12).

**Rationale**: The chart relies on PodSecurity Admission namespace labels (`restricted` for stub, `privileged` for real — FR-012d), which is GA at 1.25. 1.25+ is within the current upstream support window; k3s tracks upstream. Resolves the clarify item deferred on 2026-06-11.

**Alternative rejected**: supporting PodSecurityPolicy (removed in 1.25) — dead end.

## R4 — Workload kinds per mode

**Decision**: Single-node → **Deployment** (stub may set `replicas≥1`, all-stateless; real pins `replicas=1` with `strategy: Recreate`). Multi-node → **DaemonSet** with a `nodeSelector` on the designation label (one instance per labeled node). (FR-003b.)

**Rationale**: Real mode is privileged + hostNetwork + host ports, so two pods cannot overlap on one node; Recreate accepts the brief upgrade downtime. DaemonSet gives exactly-one-per-node without per-node config.

## R5 — Exposure & optional TLS

**Decision**: A single Service exposing the seven ports, `ClusterIP` by default; `service.type` (LoadBalancer/NodePort) and an optional Ingress are selectable (FR-008a, FR-009d). Optional TLS = **per-service-port termination** (SPEC-007 R8 / WS-3); when TLS is enabled the chart sets `O3K_ENDPOINT_SCHEME=https` so the catalog and self-links advertise `https://<host>:<port>` (FR-008b). The chart references an operator- or cert-manager-supplied certificate and never generates certs. Metadata is excluded from this front-door Service (reached node-locally — FR-009a).

**Rationale**: O3K serves each service on its own port; a scheme-only rewrite keeps the port, composing cleanly with per-port TLS. Single-`:443` path/subdomain routing is out of scope (would need per-service path advertising — beyond the landed SPEC-007 WS-3).

## R6 — Secrets handling & install gates

**Decision**: Each required secret (token signing secret, admin password, DB credentials; optional Redis/storage creds) may be **created from values OR referenced via `existingSecret`** (FR-007b). `values.schema.json` plus a `_helpers.tpl` guard `fail` the install when a required secret or the image reference is absent (FR-007/007a/018). No deployment runs with `admin/secret` — this works because SPEC-007 WS-2 (admin-password bootstrap) is landed.

**Rationale**: Supports both quick-start (inline values) and GitOps (`existingSecret`); fail-fast install mirrors the constitution's fail-fast principle at deploy time.

## R7 — Health & readiness

**Decision**: Per-service liveness/readiness probes on each port — Keystone `GET /v3`, Nova `/v2.1`, Neutron `/v2.0`, Cinder `/v3`, Glance `/v2`, Placement `/`, plus metadata locally. (FR-013.) Readiness gates traffic so rolling updates don't route to a starting/shutting-down instance.

**Rationale**: The reference image's HEALTHCHECK already probes `http://localhost:35357/v3`; reuse that pattern per port.

## R8 — Database migrations

**Decision**: Rely on the product's startup migration path serialized by golang-migrate's DB lock; the chart adds **no** migration Job (FR-012b). The chart ensures the DB is reachable before instances become ready (startup migration fails fast otherwise).

**Rationale**: `cmd/o3k/main.go` runs `database.MigrateUp` at startup; the reference image CMD additionally runs `o3k-migrate up` first — both take golang-migrate's lock, so concurrent multi-node boots and rolling upgrades are safe (first instance migrates, others no-op). A dedicated Job would need a product change and is unnecessary.

## R9 — Real-mode host access

**Decision**: Behind `realMode`/privileged toggles only: `privileged` securityContext, `hostNetwork: true`, and `hostPath` mounts for the host libvirt socket (`/var/run/libvirt/libvirt-sock`) and host storage (`/var/lib/o3k`) — FR-012c/012d/012a. The chart can apply the namespace `privileged` PodSecurity label (real-mode toggle) and documents the equivalent OpenShift SCC. Stub mode mounts none of this and passes `restricted` PSA.

**Rationale**: O3K connects to a host libvirtd over the unix socket and does not bundle qemu; VMs are host processes that survive pod restarts. **Their networking survives a restart only once WS-6 startup reconciliation lands (FR-012g) — deferred per R12.** Privileged/host access is confined to designation-labeled nodes in multi-node.

## R10 — Container image (`dnsmasq`) — FR-012f

**Decision**: Add `dnsmasq` to the reference `build/package/Dockerfile` (which currently installs `iproute2`/`iptables`/`ipset` but **not** `dnsmasq`), keeping the other tools. The chart references the image by tag only (required, no default — FR-018).

**Rationale**: Verified — without `dnsmasq` in the image, real-mode DHCP fails on first run. This is the only product-repo file (besides docs) that this feature touches; the binary/API are unchanged.

## R11 — Multi-node node designation & overlay

**Decision**: A configurable label (default `o3k.io/compute=true`) selects nodes via the DaemonSet `nodeSelector`. Applying/removing the label starts/evicts an instance with no chart upgrade (FR-005a). VXLAN overlay via `neutron.vxlan_enabled: true` in the rendered config (the binary reads this from the file, not env); ensure node-to-node UDP 4789 reachability (FR-009). Privileged/host access is confined to labeled nodes.

**Rationale**: Matches the product's existing node-registry + VXLAN coordinator; the chart only sets config and scheduling.

## R12 — Deferred real-mode & multi-node validation (2026-06-11 clarification)

**Decision**: Author all templates now; **validate single-node stub (+ optional TLS) end-to-end**; real-mode and multi-node render/lint-clean but their e2e success criteria (SC-003, SC-004, real-VM login/floating-IP/restart) are gated on the product post-conditions (WS-1/4/5/6, issue #52). Express FR-010's "minimum product version" as a documented `multiNode.minProductVersion` value (filled when WS-1 ships); the chart templates without it and documents/optionally warns that multi-node is not yet functional.

**Rationale**: Matches the spec's own "authorable now, functional once the post-conditions land" stance; delivers the single-node MVP without blocking on unbuilt product work.

## R13 — Storage-backend validation (multi-node real)

**Decision**: In multi-node real mode require a shared backend — Glance `rbd|s3`, Cinder `rbd`; a `_helpers.tpl` guard `fail`s (or hard-warns) on local backends in that mode, and the values docs mark local backends single-node-only (FR-012e). Single-node real may use local.

**Rationale**: Local Glance/Cinder data is node-local; a VM on another node can't boot/attach it. Parallels the external-DB and shared-Redis constraints.

## R14 — Testing strategy

**Decision**: `helm lint` + `helm template` snapshots over all mode permutations (every combination renders); helm-unittest for value→manifest assertions and the fail-fast gates (missing secret/image, local-backend-in-multinode-real); `helm test` identity-check hook (FR-015); a kind/k3s CI smoke that installs single-node stub and runs `openstack token issue` + server create/list/delete (the stub-applicable subset of SC-002). Deferred: `test/vxlan_multinode_test.sh` and real-mode bash suites — run once the post-conditions land.

**Rationale**: Renders/gates are validatable now without the post-conditions; e2e real/multi-node validation is honestly deferred per R12.

## R15 — Multi-release isolation

**Decision**: All resource names release-prefixed (`{{ .Release.Name }}-o3k-*`); per-release scoping of node registration; the configured endpoint host points at the release's Service (FR-016).

**Rationale**: Two releases in one cluster must not collide on names, networking, or registrations.
