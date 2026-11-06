# Contract: Compute guest operations while libvirt is unreachable or unprepared

**Feature**: `031-realmode-dns-libvirt-fixes` (FR-014, FR-016, FR-019)

Real mode only. "Unreachable" covers both "never connected yet" and "connection lost" — they answer alike (spec Clarifications).

## C-GUEST-1 — Create (FR-019)

| State | `POST /servers` | Server ends |
|---|---|---|
| unreachable | `202` (accepted, as always) | `ERROR` with a `fault`, never `ACTIVE` |
| connected, not prepared, one on-demand attempt fails | `202` | `ERROR` with a `fault` |
| connected, prepared | `202` | `ACTIVE` with a guest |

Every real-mode create that ends `ERROR` records a `fault` through `markInstanceError` (code 500). The message names the step that failed, curated as `allocationFault` does for networks; the detail is in the log:

| Step that failed | `fault.message` |
|---|---|
| libvirt create (`createFault`): the console socket directory, or the connection it needs, not ready | "the hypervisor is not ready to start guests" |
| libvirt create (`createFault`): anything else | "the hypervisor could not create the guest" |
| no hypervisor wired in (real mode) | "the hypervisor is not ready to start guests" |
| per-instance directory (image or volume boot) | "the per-instance directory could not be prepared" |
| root disk staging (overlay) | "the root disk could not be staged" |
| image base not available on the host | "the image could not be made available on this host" |
| `imageRef` not a valid image ID | "the image reference is not a valid image ID" |
| panic in the create goroutine | "the server could not be created" |

The faults the create already records (network allocation, boot volume checks) are unchanged. `server show` returns the fault as upstream does for a failed build.

## C-GUEST-2 — Server actions (FR-016, FR-019)

| Action | Preparation never succeeded (includes never connected) | Prepared, connection lost later |
|---|---|---|
| start, hard reboot, resume | one on-demand attempt; if it fails, `500` naming the console socket directory and its cause — FR-016's existing refusal. While unconnected the cause reads "not connected to libvirt" | `500` "failed to perform <action>", as a lost connection answers today (`sendLifecycleError`) |
| stop, soft reboot, delete, volume attach/detach, other libvirt-backed actions | `500` "failed to perform <action>" (or the handler's existing generic `500`) | the same |
| console (`remote-consoles`) | `500` — the console readback's existing answer for a failed read (`sendConsoleEndpointError`, default case) | the same |

- `startRefusal` names the directory only for `ErrConsoleSocketDirNotReady`. Every other libvirt failure keeps the generic message.
- No `503`, and no new message.
- A delete that cannot reach libvirt leaves the server in place, as during a lost connection today.

## C-GUEST-3 — Ownership before every launch (FR-014)

Immediately before QEMU launches — create (image or volume boot), start, hard reboot, resume, self-heal:

1. If not yet prepared, one on-demand preparation attempt — or the result of the one already running — within 1 s.
2. `<state_path>/instances/<instance-id>` exists and belongs to QEMU's recorded uid:gid. It is created or chowned on the spot, idempotently.

If step 2 fails, the guest is not launched: a create ends `ERROR` with a `fault` and its defined domain is undefined; an action answers `500`.

This repairs a directory made while QEMU's user was unknown, one left root-owned by an earlier release, and one whose owner changed under a reconnect redo.

## C-GUEST-4 — What does not change

- Stub mode, and every read path (`server list`, `server show`): no libvirt involved.
- Existing guests keep running while libvirt is unreachable: libvirt being down does not stop QEMU processes, and the state reconciler treats an unanswered read as "unknown" (SPEC-025).
