# Phase 0 Research: Verify Horizon dashboard compatibility

**Feature**: SPEC-018 | **Date**: 2026-07-22 | **Plan**: [plan.md](./plan.md)

Resolves the three technical unknowns from the plan's Technical Context. Each is grounded in the Lightstack code (file:line cited). Findings feed the runbooks, the report's expected verdicts, and the verification Horizon chart's values.

---

## U1 — Can the Horizon noVNC Console panel work against Lightstack?

**Decision**: **No — record the interactive noVNC Console tab as a predetermined "not working".** The adjacent **serial Console Log** (`console log show`) works in real mode and may be reported as a working "console-adjacent" feature. This becomes a follow-up ticket (wire a real console), not a fix inside SPEC-018.

**Rationale** (three compounding gaps, all in `internal/nova/console.go`):
1. **Synthetic URL, no config knob** — VNC/remote-console handlers return HTTP 200 with a hardcoded `http://localhost:6080/vnc_auto.html?token=…` (`console.go:158-166`). No host configuration; in k3s the browser resolves `localhost:6080` to the *user's own machine*, so it is even more broken than in compose.
2. **Meaningless token, no VM-port readback** — `generateConsoleToken` returns random bytes, never persisted (`console.go:197-203`); `setupVNCConsole` invents a hash-derived port and explicitly never queries libvirt (`console.go:175-180`). Same in stub and real mode (no `libvirtMode` branch, unlike `GetConsoleOutputAction`).
3. **Unwired generic proxy** — the verification stacks ship a standalone noVNC (`bonigarcia/novnc`) pointed at a dead placeholder target, with no Lightstack-side token wiring; Lightstack runs no console proxy of its own, so no `?token=` is ever brokered to a VM's VNC.

Real VM XML *does* expose a VNC server (`pkg/hypervisor/xml_template.go:163-168`, `autoport='yes'`, listen `0.0.0.0`), but nothing reads that port back or routes a proxy to it. Existing tests only assert URL *string shape* or the serial log (`test/console_access_test.sh`, `test/console_test.sh`, `test/ws0_console_test.sh`); none connect to a framebuffer.

**Implication for this feature**: US3/FR-007's console verdict is knowable up front = **not working (no guest framebuffer)**. Still worth deploying a maintained, pinned noVNC (`bonigarcia/novnc`) in the verification stacks to reproduce the observed browser behavior (the Console tab fails to load) as report evidence — with a port-forward note, since the returned URL points at `localhost`. Spin off a follow-up: *"wire Lightstack noVNC console — token store + libvirt VNC-port readback + a real proxy, and make the proxy URL configurable."*

**Alternatives considered**: (a) Fix the console within SPEC-018 → rejected: out of scope (verification only; fixes become follow-ups, as SPEC-016→#524). (b) Drop the console workflow → rejected: the user explicitly wants it verified, and a grounded "not working + why" is exactly the report's value. (c) Report the working serial log instead → include it as a bonus, but it is not the noVNC console panel US3 targets. (d) Use kolla `nova-novncproxy:2025.2` (the real OpenStack proxy, matching our Horizon image's source/release) instead of a standalone noVNC → rejected: it validates console tokens via a Nova console-auth callback (`os-console-auth-tokens`) Lightstack does not implement, needs a `nova.conf`, and still cannot resolve a token to a VM — materially more config for a component that reproduces the same Lightstack-side gap. A maintained, pinned standalone noVNC (`bonigarcia/novnc`) is the simpler, honest choice.

---

## U2 — Wiring Lightstack Glance to an external Ceph RGW (S3) endpoint

**Decision**: Configure Lightstack Glance for the `s3` backend via chart values; provision the RGW endpoint + a pre-created bucket + static credentials on the Rook side; wire Lightstack→RGW cross-cluster over **HTTP via NodePort** (path-style, so an `IP:port` endpoint is fine). Volumes stay node-local; **no RBD**.

**Rationale** (from `pkg/storage/image_store.go`, `internal/common/config.go:83-102,145-178`, `charts/lightstack/values.yaml:66-86`, `_helpers.tpl:127-136`):
- Lightstack forces **path-style** addressing (`image_store.go:205`) → `http://<rgw-host>/<bucket>/<key>`, no virtual-host DNS wildcard needed → a NodePort `IP:port` endpoint works.
- **SigV4** signing; `s3_region` must be non-empty (default `us-east-1`); RGW is lenient / zonegroup-configurable.
- **No bucket auto-create** (no `CreateBucket` in code; SPEC-014 spec.md:84,144) → the bucket must pre-exist.
- Credentials are **not** config keys — the AWS SDK default chain reads `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` from env; the chart injects them via `extraEnv` (`_helpers.tpl:251-275`).
- `EnsureLocalBase` (`image_store.go:981`) lazily fetches + MD5-verifies the image on boot → a **pure-`s3` Glance is bootable** (Nova shares the store, `main.go:261`), so launch-instance with an RGW-backed image works.
- **Chart gaps to note**: `s3_connect_timeout` is not rendered by `lightstack.config` (set via `extraEnv` `LIGHTSTACK_GLANCE_S3_CONNECT_TIMEOUT` if needed); `caBundle` renders a path but no volume mounts a PEM there → **use a plain-HTTP RGW endpoint** to sidestep both (test setting `insecureSkipVerify` is the only other option).

**Concrete recipe** — chart values on k3s:
```yaml
glance:
  storageMode: "s3"                      # pure RGW (or "local,s3" for local-primary + RGW replica)
  s3:
    endpoint: "http://<minikube-ip>:<nodeport>"   # Rook RGW, exposed off-cluster
    bucket: "lightstack-images"                          # MUST pre-exist (lightstack never creates it)
    region: "us-east-1"                           # non-empty (SigV4 scope)
    caBundle: ""                                  # empty → HTTP
    insecureSkipVerify: false
extraEnv:                                # the ONLY place credentials go
  - { name: AWS_ACCESS_KEY_ID,     valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: accessKey } } }
  - { name: AWS_SECRET_ACCESS_KEY, valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: secretKey } } }
```
Rook side (minikube): a `CephObjectStore` (RGW service, exposed cross-cluster via NodePort / `minikube service` / tunnel), a `CephObjectStoreUser` (its Secret yields `AccessKey`/`SecretKey` → copy into `rgw-s3-creds` in the Lightstack namespace), and a pre-created bucket `lightstack-images`.

**Scope clarifier**: the server-side `/v3/s3tokens` SigV4/SigV2 verification + `implicit tenants=s3` (SPEC-014 capability B, tenant object storage) is a **separate** feature — **not** exercised by Glance-on-RGW. For Glance, RGW authenticates Lightstack with the static user keys above, not via keystone s3tokens.

**Alternatives considered**: (a) HTTPS RGW with internal CA → rejected for the runbook default (chart has no PEM-mount hook; adds friction); HTTP is fine for a verification cluster. (b) `local,s3` hybrid → viable and more robust (local primary, RGW replica), but pure `s3` more directly exercises the RGW path the ticket cares about; the runbook can mention the hybrid as a fallback. (c) RBD → out of scope (D4).

---

## U3 — Must Lightstack be browser-reachable (NodePort), or can it stay ClusterIP?

**Decision**: **Lightstack stays `ClusterIP`.** Set `endpointHost` to the in-cluster Service DNS so the catalog is resolvable by the **Horizon pod**. Expose **only Horizon (and the novnc proxy) via NodePort**. **CORS is not involved** — leave it default.

**Rationale** (from `internal/keystone/auth.go:575-759`, `internal/common/scheme.go`, `charts/lightstack/*`, `deployments/horizon-config/local_settings`, `internal/middleware/logging.go:260-287`):
- Horizon reaches Lightstack **server-side**: the Django process authenticates to `OPENSTACK_KEYSTONE_URL`, reads the catalog, and calls each service's `publicURL` from the **pod**. The AngularJS NG launch wizard (`LAUNCH_INSTANCE_NG_ENABLED = True`) calls Horizon's own same-origin `/api/…` proxy, which forwards server-side. **The browser never calls Lightstack's catalog endpoints directly.**
- Therefore the catalog only needs a host the **Horizon pod** resolves. Lightstack's catalog host is a single env `LIGHTSTACK_ENDPOINT_HOST` (default `localhost`) applied to all services, each keeping its native port (`auth.go:666-692`); endpoints are **public-interface only**. Chart `endpointHost: ""` defaults to loopback (unusable by a separate pod), so it **must** be set to `<release>-lightstack` (or the FQDN).
- **CORS** (`logging.go:260-287`, exact-match allowlist, default `["http://localhost"]`) is a browser cross-origin control. Server-to-server calls carry no browser `Origin`, so CORS is **not exercised** by the proxied integration. (The chart can't easily set it anyway — it renders no `server:` block.)
- **NodePort is a poor fit for Lightstack's APIs**: the catalog advertises fixed native ports (35357/8774/…), but NodePort allocates 30000–32767 and won't preserve them; and with a single `endpointHost` (public-only) you can't advertise an in-cluster *and* an external host at once.

**Concrete values** — k3s:
- Chart: `service.type: ClusterIP`; `endpointHost: "<release>-lightstack"` (→ catalog advertises `http://<release>-lightstack:35357/v3`, `:8774`, `:9696`, `:8776`, `:9292`); `cors_allowed_origins`: default (untouched); `tls.enabled: false`.
- Horizon `local_settings`: `OPENSTACK_HOST = "<release>-lightstack"`, `OPENSTACK_KEYSTONE_URL = "http://<release>-lightstack:35357/v3"`, `AVAILABLE_REGIONS = [('http://<release>-lightstack:35357/v3','RegionOne')]`, `OPENSTACK_ENDPOINT_TYPE = "publicURL"` (unchanged).
- Reachability needed: Horizon pod → Lightstack Services (in-cluster); browser → Horizon (NodePort); browser → novnc proxy (NodePort, only to reproduce the console behavior).

**Alternatives considered**: (a) NodePort-expose Lightstack + add the origin to CORS → rejected: broken (fixed-port catalog vs NodePort range) and unnecessary (server-side proxy). (b) LoadBalancer/ingress per port with `endpointHost` = external host → only if a *laptop `openstack` CLI* against the same deployment is later wanted (out of scope here); note it in the runbook as a future option.

---

## Consolidated real-pass deployment shape (resolved)

```text
┌─ k3s (root/privileged) ──────────────────────────────────────────────┐
│  lightstack (Helm charts/lightstack, real mode)         Service: ClusterIP          │
│    realMode.enabled=true                  endpointHost=<release>-lightstack   │
│    nova.libvirtMode=real                  (catalog → in-cluster DNS)   │
│    glance.storageMode=s3  ─────────────────────────┐                  │
│    cinder → node-local /var/lib/lightstack/volumes        │ AWS_* via extraEnv│
│                                                    │                  │
│  Horizon verification chart (new)                  │                  │
│    horizon (kolla 2025.2, ConfigMap=horizon-config)│  Service:NodePort │
│    memcached (sessions)                            │                  │
│    novnc (bonigarcia/novnc)                        │  Service:NodePort │
│    local_settings → OPENSTACK_KEYSTONE_URL=http://<release>-lightstack:35357  │
└────────────────────────────────────────────────────┼──────────────────┘
                                                       │ HTTP S3 (path-style, SigV4)
┌─ minikube ────────────────────────────────────────  ▼ ────────────────┐
│  Rook/Ceph: CephObjectStore (RGW) ── NodePort/tunnel → http://ip:port   │
│             CephObjectStoreUser  ── Secret{AccessKey,SecretKey}         │
│             bucket "lightstack-images" (pre-created)                          │
└────────────────────────────────────────────────────────────────────────┘
Browser → Horizon NodePort (dashboard) ; → novnc NodePort (console repro only)
```

## Spec/plan deltas discovered during research

- **CORS assumption corrected**: the spec's US1-scenario-3 / "remote browser origin" edge case / the assumption to add the NodePort origin to `cors_allowed_origins` are **superseded** — the integration is server-side, Lightstack stays ClusterIP, and the real knob is `endpointHost` (in-cluster DNS). Spec, plan, and the #482 checklist updated to match.
- **Console verdict is pre-known** (not working) — reflected as an expected result and a follow-up, not a blocker.
