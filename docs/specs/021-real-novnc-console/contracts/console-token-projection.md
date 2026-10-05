# Contract: console-token projection directory (SPEC-021)

The interface between Lightstack and the console proxy. Lightstack writes; websockify reads. Neither calls the other.

## Location

`<state_path>/console-tokens/` — `/var/lib/lightstack/console-tokens/` on the default root — a subdirectory of the existing node-local root (`state_path`, which the chart renders from `realMode.hostStoragePath`), pre-created by the same initContainer that creates `images`, `volumes`, `instances`, `cloud-init` and `dhcp` (`charts/lightstack/templates/_helpers.tpl:177-193`).

Exposed to Go as `ConsoleTokenDir()` in `pkg/paths/paths.go`, alongside the existing `ImagesDir` / `VolumesDir` / `InstancesDir` / `CloudInitDir` / `DHCPDir`, and asserted by `TestLayout` in `pkg/paths/paths_test.go` — that layout is a contract, not a convention. The subdirectory names are fixed; the root is not, and Lightstack and the proxy must see it at the same path.

## File format

One file per live token. Exactly one line:

```text
<token>: unix_socket:<path>
```

Example:

```text
9f8c1a2b3d4e5f60718293a4b5c6d7e8: unix_socket:/var/lib/lightstack/console-sockets/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d.sock
```

`<path>` is the guest's VNC socket at its fixed per-instance path in
`<state_path>/console-sockets/` (FR-017), never a TCP port, which libvirt would hand to
the next guest that starts. The path survives restarts. The run is tracked by the row's
`domain_id`, which the sweep compares with the guest's current one (FR-014a).

**The format is dictated by websockify's parser**, read out of the pinned image (`token_plugins.py`, verified):

- Lines are split with `re.split(r':\s', line)` — the separator is a colon **followed by whitespace**. A token containing `": "` would corrupt parsing, which is why tokens stay hex-encoded.
- The target is split with `rsplit(':', 1)`, and a first half of `unix_socket` makes the second a socket path (`websocketproxy.validate_connection`). A path containing `:` or `": "` would corrupt the split, so Lightstack refuses such a path at issue time — along with anything relative, unclean, or longer than a `sockaddr_un` holds (107 bytes).
- Lines starting `#` are comments. A malformed line logs a syntax error and is skipped — it does not kill the proxy.
- `--token-source` may be a file or a **directory**; we use a directory, so one file per token means no read-modify-write races.

## Naming and permissions

| Property | Value | Why |
|---|---|---|
| Filename | the token value | Unique by construction; makes deletion an O(1) unlink with no scan |
| Mode | readable by the proxy's user, no wider | The proxy pod drops ALL capabilities and may run non-root; 0600 by a root Lightstack would make **every token fail**. Resolve explicitly (0640 + shared group, or matching UID) and assert it in a chart test |
| Owner | the Lightstack process | Only Lightstack writes |

The permission decision is deliberately left to implementation with one constraint: it must be asserted by a test, because the failure mode — every token refused — looks exactly like a token bug and would burn a host-testing cycle to diagnose.

## Lifecycle invariants

1. **The database is authoritative; this directory is derived.** It may be deleted and rebuilt; it is never the source of truth.
2. **Fail closed.** A missing or unwritable directory means tokens cannot be issued and existing ones stop resolving. It must never mean "allow unvalidated access". If the directory cannot be written, the console request fails rather than returning a token that cannot be revoked.
3. **A file must never outlive its row.** websockify performs **no expiry check** — it resolves whatever it finds. Deletion therefore removes the file *before or with* the row, never after.
4. **`TokenFile`, not `ReadOnlyTokenFile`.** `TokenFile.lookup()` re-reads the source on every lookup, so writes take effect immediately and deletion is revocation. `ReadOnlyTokenFile` caches on first use and would keep serving deleted tokens — using it would silently void FR-010 and FR-014.

## Write and delete triggers

| Trigger | Action |
|---|---|
| Authorized console request | Write the file, insert the row. Also unlink this instance's already-expired entries. |
| `expires_at` passed | Reconciler unlinks the file, then deletes the row. |
| `os-stop`, `os-start`, `reboot`, suspend, resume, shelve | Unlink every entry for that instance, then delete the rows, so a link never outlives the console it was issued for. |
| Reconciler sweep (real mode) | Compare every live token with the socket and domain ID its guest has now. Unlink and delete those whose guest serves another socket, runs under another domain ID, or is not running: a guest that powered itself off, crashed, or was restarted outside the API. On a readback *error* the tokens stay for the next sweep. |
| Instance deleted (incl. `forceDelete`) | Same; rows also cascade via the foreign key. |
| Service start | One sweep for orphans left by an unclean shutdown. |

## Verified proxy behaviour

Against the pinned image with this contract in place (research.md R5; the unix-socket rows re-verified in review against a libvirt 10.0 domain, through a read-only mount of its directory):

| Probe | Result |
|---|---|
| `WS /?token=<known>` | `101 Switching Protocols`, the guest's `RFB 003.008` greeting, log `connecting to unix socket: <path>` |
| `WS /?token=<known>` after the guest restarted | Since 2026-10-01 the path is fixed, so until the sweep revokes the token (≤ one interval: the domain ID changed) it still reaches **the same** guest's new run, never another guest. Re-verify on the host (runbook R7). Before the amendment: `101`, then `Failed to connect to downstream server`, because the per-start path was gone |
| `WS /?token=<unknown>` | Connection closed, **no response**, log `Token '<x>' not found` — no guest contacted |
| `WS /websockify?token=<known>` | `101 Switching Protocols` (either path shape works) |

The bad-token case means **FR-011's fail-closed requirement is satisfied by the component itself**. Lightstack's obligation is narrower than it first appears: keep the directory truthful.
