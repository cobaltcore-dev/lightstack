# Contract: console-token projection directory (SPEC-021)

The interface between O3K and the console proxy. O3K writes; websockify reads. Neither calls the other.

## Location

`/var/lib/o3k/console-tokens/` — a subdirectory of the existing node-local root (`realMode.hostStoragePath`), pre-created by the same initContainer that creates `images`, `volumes`, `instances`, `cloud-init` and `dhcp` (`charts/o3k/templates/_helpers.tpl:177-193`).

Exposed to Go as `ConsoleTokenDir()` in `pkg/paths/paths.go`, alongside the existing `ImagesDir` / `VolumesDir` / `InstancesDir` / `CloudInitDir` / `DHCPDir`, and asserted by `TestLayout` in `pkg/paths/paths_test.go` — that layout is a contract, not a convention.

## File format

One file per live token. Exactly one line:

```text
<token>: <host>:<port>
```

Example:

```text
9f8c1a2b3d4e5f60718293a4b5c6d7e8: 127.0.0.1:5901
```

**The format is dictated by websockify's parser**, read out of the pinned image (`token_plugins.py`, verified):

- Lines are split with `re.split(r':\s', line)` — the separator is a colon **followed by whitespace**. A token containing `": "` would corrupt parsing, which is why tokens stay hex-encoded.
- The target is split with `rsplit(':', 1)`, so an IPv6 literal host would need bracketing. Not applicable in the supported topology (loopback), but worth knowing before someone tries.
- Lines starting `#` are comments. A malformed line logs a syntax error and is skipped — it does not kill the proxy.
- `--token-source` may be a file or a **directory**; we use a directory, so one file per token means no read-modify-write races.

## Naming and permissions

| Property | Value | Why |
|---|---|---|
| Filename | the token value | Unique by construction; makes deletion an O(1) unlink with no scan |
| Mode | readable by the proxy's user, no wider | The proxy pod drops ALL capabilities and may run non-root; 0600 by a root O3K would make **every token fail**. Resolve explicitly (0640 + shared group, or matching UID) and assert it in a chart test |
| Owner | the O3K process | Only O3K writes |

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
| `os-stop`, `os-start`, `reboot`, suspend, resume | Unlink every entry for that instance, then delete the rows. The endpoint may move, and a stale entry could resolve to a port since reassigned to a **different** instance. |
| Instance deleted | Same; rows also cascade via the foreign key. |
| Service start | One sweep for orphans left by an unclean shutdown. |

## Verified proxy behaviour

Against the pinned image with this contract in place (research.md R5):

| Probe | Result |
|---|---|
| `WS /?token=<known>` | `101 Switching Protocols`, log `connecting to: <host>:<port>` |
| `WS /?token=<unknown>` | Connection closed, **no response**, log `Token '<x>' not found` — no guest contacted |
| `WS /websockify?token=<known>` | `101 Switching Protocols` (either path shape works) |

The bad-token case means **FR-011's fail-closed requirement is satisfied by the component itself**. O3K's obligation is narrower than it first appears: keep the directory truthful.
