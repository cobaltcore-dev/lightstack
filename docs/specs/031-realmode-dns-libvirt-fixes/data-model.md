# Data Model: Real mode — cluster DNS under host networking, the libvirt socket, and a capabilities read that is never retried

**Feature**: `031-realmode-dns-libvirt-fixes` | **Date**: 2026-11-06

No database entity, schema change or migration. What this feature models is configuration and in-process state: one configuration key, two chart values, and three pieces of state inside the real-mode hypervisor manager. Requirement numbers refer to `spec.md`; research items (R*n*) to `research.md`.

---

## 1. libvirt connection setting — `nova.libvirt_uri`

| Value | Target | Notes |
|---|---|---|
| `""` | automatic | as `qemu:///system` |
| `qemu:///system` | automatic | the default; what the chart renders for the default socket |
| `qemu+unix:///system` | automatic | same meaning |
| `qemu:///system?socket=<abs>` or `qemu+unix:///system?socket=<abs>` | explicit `<abs>` | FR-008 |
| `<abs>` (no scheme) | explicit `<abs>` | kept from today; what the chart renders for a non-default socket |

**Validation** (real mode only, at configuration load — R3): anything else is refused with one message naming the value and the accepted forms — another driver, `qemu:///session`, `qemu:///embed`, a host or remote transport, a relative path or relative `socket=`, a repeated `socket=`, any other parameter, a fragment. Stub mode does not check it (FR-009).

## 2. Socket choice

| Target | Each connection attempt dials |
|---|---|
| explicit `<abs>` | `<abs>`, whatever else exists |
| automatic | `/var/run/libvirt/virtqemud-sock` if it exists, else `/var/run/libvirt/libvirt-sock` if it exists, else fails naming both |

- Decided on **every** attempt, reconnects included (FR-007, R4).
- "Exists" follows symlinks.
- The socket dialed is logged on success; a failure names what was tried (FR-010).

## 3. Connection state (server)

```text
            start (real mode)
                  │
                  ▼
           ┌─────────────┐  attempt fails or times out (1 s)
           │ unconnected │◄──────────── back off 1 s → 5 s ──┐
           └──────┬──────┘                                   │
                  │ attempt succeeds (socket logged)          │
                  ▼                                           │
           ┌─────────────┐   Disconnected() fires            │
           │  connected  │───────────────────────────────────┘
           └──────┬──────┘
                  │ on entering: run preparation (§4) until it succeeds
                  ▼
              shutdown → loop stops (Close)
```

- While **unconnected**, every libvirt call fails as during a lost connection (FR-019). Nothing falls back to stub.
- `lightstack cleanup` has no loop: one attempt, bounded at 1 s like the loop's. A missing socket, or a daemon that does not answer in time, fails it (FR-020a, R5).

## 4. Console socket directory preparation

| Field | Meaning |
|---|---|
| `prepared` | set by the first success; **never cleared** |
| owner | uid:gid QEMU runs as, from libvirt's capabilities; **replaced by every success** |
| last cause | why the latest attempt failed; used in the refusal message and the first ERROR line |
| directory | `<state_path>/console-sockets` |

**Transitions**:

| Event | Effect |
|---|---|
| attempt succeeds | `prepared` ← true; owner ← reported; directory chowned 0750 to it |
| attempt fails while not prepared | last cause ← error; guest starts keep being refused |
| attempt fails while prepared | nothing in effect changes (FR-015) |

**Attempts**:
- background, after every (re)connection, until one succeeds;
- on demand, one per guest start that finds `prepared` false.

Callers share one in-flight attempt (single-flight): a guest start that arrives while an attempt runs takes that attempt's result. Each attempt is bounded at 1 s, so no caller waits longer than one bound (FR-013, FR-016, R6).

## 5. Guest start

Any launch of a guest's QEMU process: **create** (image or volume boot), **start**, **hard reboot**, **resume** (from managed save), **self-heal** re-creation. All of them reach `DomainCreate` in one of four functions (R7).

**Preconditions, checked immediately before the launch**:
1. **Prepared** — if not, one on-demand attempt; if it fails, refused with the existing message naming the directory and the cause (FR-016).
2. **Per-instance directory** `<state_path>/instances/<instance-id>` exists and belongs to the recorded owner — created or chowned on the spot, idempotently (FR-014). The domain UUID is the instance ID.

## 6. Readiness inputs

| Mode | `/readyz` ready when |
|---|---|
| stub | database reachable **and** all listeners bound (unchanged) |
| real | the above **and** `prepared` (sticky, §4) |

- Reason when unprepared: `hypervisor unavailable` (combined with others as `database,hypervisor unavailable`).
- `/healthcheck` mirrors `/readyz` (FR-017).

## 7. Chart values

| Value | Renders |
|---|---|
| `realMode.enabled` + `realMode.hostNetwork` both true | `dnsPolicy: ClusterFirstWithHostNet` on the Deployment or DaemonSet pod and the purge Job (FR-001); otherwise no `dnsPolicy` (FR-002) |
| `realMode.libvirtSocketPath` = default `/var/run/libvirt/libvirt-sock` | mount `/var/run/libvirt`; `libvirt_uri: "qemu:///system"` (unchanged) |
| `realMode.libvirtSocketPath` = anything else (absolute) | mount its directory; `libvirt_uri: "<that path>"` in the rendered config (FR-004) |
| `realMode.libvirtSocketPath` relative or empty | refused by the schema (FR-006) |
| `secrets.existingSecret` set | no rendered config: the operator's config must name the mounted socket (FR-005, documentation only) |
