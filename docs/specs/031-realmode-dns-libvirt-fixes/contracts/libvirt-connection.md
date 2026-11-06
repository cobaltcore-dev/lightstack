# Contract: libvirt connection — configuration, socket choice, retries

**Feature**: `031-realmode-dns-libvirt-fixes`

What an operator configures, and what the binary does with it, in real mode. Stub mode never reads `nova.libvirt_uri`.

## C-URI-1 — Accepted forms (FR-007, FR-008)

| `nova.libvirt_uri` | Dials |
|---|---|
| `""`, `qemu:///system`, `qemu+unix:///system` | the automatic choice (C-SOCK-AUTO) |
| `qemu:///system?socket=/abs`, `qemu+unix:///system?socket=/abs` | `/abs` |
| `/abs` | `/abs` |

An explicit socket is dialed whatever else exists.

## C-URI-2 — Refused forms stop startup (FR-009)

In real mode, any other value stops the process when its configuration loads:

| Entry point | Exit | Output contains |
|---|---|---|
| `lightstack --config …` | 1 | the value and the accepted forms |
| `lightstack cleanup --config …` | 2 | the same |
| `lightstack console-tokens --config … list` | 2 | the same |

Message shape (wording settled in implementation; the value and all three accepted shapes must appear):

```text
nova.libvirt_uri "<value>" is not supported; accepted: qemu:///system or qemu+unix:///system (chooses virtqemud-sock, else libvirt-sock), either with ?socket=<absolute path>, or an absolute socket path
```

Refused: another driver, `qemu:///session`, `qemu:///embed`, a host or remote transport (`qemu://h/system`, `qemu+tcp://`, `qemu+ssh://`, `qemu+tls://`, `qemu+libssh2://`, `qemu+ext://`), a relative path, a relative or repeated `socket=`, any other parameter, a fragment. In stub mode the same values are accepted unread.

## C-SOCK-AUTO — The automatic choice (FR-007, FR-012)

On **every** connection attempt:

1. `/var/run/libvirt/virtqemud-sock` exists → dial it.
2. else `/var/run/libvirt/libvirt-sock` exists → dial it.
3. else → the attempt fails naming both paths.

"Exists" follows symlinks. A monolithic `libvirtd` host has only `libvirt-sock`, so it dials what it dials today.

## C-LOG-1 — What the log says (FR-010)

| Event | Level | Names |
|---|---|---|
| connection established | INFO | the socket dialed |
| connection attempt failed | DEBUG (first failure: WARN) | the socket or sockets tried, and the cause |
| connection lost | WARN | — (as today) |

## C-CONN-SERVER — The server never waits for libvirt (FR-020)

- Startup does not block on libvirt. In real mode the hypervisor starts unconnected, and a background loop dials.
- Each attempt (dial + handshake) is bounded at 1 s. Between attempts the loop waits 1 s, doubling to at most 5 s. It stops at shutdown.
- On every connection, the first and each reconnect, the loop runs the console socket directory preparation until it succeeds (C-PREP).
- Until connected, libvirt calls fail as during a lost connection; nothing falls back to stub (see `guest-operations.md`).

## C-CONN-CLEANUP — `lightstack cleanup` makes one attempt (FR-020a)

- One attempt with the C-SOCK-AUTO choice (or the explicit socket), bounded at 1 s for dial plus handshake — the same bound as the server's attempts (Article III).
- A missing socket fails it at once; a daemon that does not answer within the bound fails it too. Either way: exit non-zero, naming the socket or sockets tried, and the uninstall stops (fail-closed, SPEC-010).
- No background dialing, no retry. A socket-activated daemon that had exited is started by the attempt itself, so re-running the uninstall finds it warm.

## C-PREP — Console socket directory preparation (FR-013, FR-015, FR-017a)

| Rule | Detail |
|---|---|
| Attempt | read libvirt's capabilities under 1 s, then make the directory QEMU's (0750) |
| Background | after every connection, attempts with the 1 → 5 s pause until one succeeds |
| On demand | one attempt per guest start while not yet prepared — or, if an attempt is already running, that attempt's result; never a wait followed by a second attempt (single-flight) |
| Sticky | the first success is never withdrawn; a later success replaces QEMU's owner |
| Log | first failure: ERROR with the cause (as today) · success after a failure: INFO, directory and uid:gid · redo after reconnect: DEBUG, or INFO if the owner changed · other retries: DEBUG |
