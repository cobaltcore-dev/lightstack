# Contract: usage counts, action history, log attribution, boot volumes
# (US7 / B7, US8 / B15, US9 / F16)

Three smaller surfaces grouped so each gets its rules without a file of its own.

---

## Usage counts (FR-040, FR-041)

`GetLimits` (`nova/handlers.go:2151`) counts instances with `AND i.status != 'DELETED'` (`:2164`) — the
only remaining occurrence of that string, and **nothing anywhere writes that status**. Every delete path
is a hard `DELETE FROM instances`. Drop the clause.

| Requirement | Rule |
|---|---|
| FR-040 | reported usage counts every server the project owns, and agrees with the quota views |
| FR-041 | no usage figure excludes a server on the basis of a state the product never assigns |

Noted, **not** fixed here: `RestoreInstance` (`advanced_actions.go:1074`) gates on `SOFT_DELETED`, another
status nothing writes. Same family, its own ticket (A12).

---

## Action history: retention and access (FR-042 – FR-044)

### Retention

```
server deleted        ──> action records SURVIVE                          FR-042
start_time older than instance_action_retention  ──> reclaimed            FR-042a
server still exists   ──> never reclaimed, whatever the record's age      FR-042c
```

The retention age is a **deployment setting** with a documented default of 90 days, not a constant baked
into the product (FR-042b) — see data-model.md for the config field and its clamp.

Verify the three delete paths leave the history alone — `DeleteServer` (`handlers.go:1433`) and
`ForceDeleteInstanceAction` (`:2683`, `:2716`) — i.e. **no change needed there**, plus a test pinning it.
The absence of `REFERENCES` on `instance_actions.instance_id` is now intentional and should be commented
so nobody "fixes" it into a cascade.

### Access (FR-043 – FR-043c)

`ListInstanceActions` (`nova/diagnostics.go:68`) resolves the instance first, so a deleted server answers
*absent* to **everyone**, administrators included. That contradicts retaining the records.

| Caller | Server exists | Server deleted |
|---|---|---|
| owner | records | **records** (was: absent) |
| administrator | records | **records** (was: absent) |
| any other project | absent | absent — indistinguishable from never having existed (FR-043b) |

Authorization comes from `instance_actions.project_id` once the server row is gone (FR-043a). SPEC-023
fixed that column to hold the **owning** project, which is what makes it usable here.

### Microversion gate (research.md R7)

Upstream serves deleted-instance actions only from **microversion 2.21**, and restricts access to the
owner or an administrator — which confirms the table above independently. Gate on the requested
microversion read from `OpenStack-API-Version`, else `X-OpenStack-Nova-API-Version`, else the advertised
current (`2.90`), so ordinary clients are unaffected.

This is the **product's first microversion gate** — Nova emits the header and has never read it.
Generalising microversion negotiation is out of scope and flagged for its own ticket.

---

## Access-log attribution (FR-045 – FR-048)

`LoggingMiddleware` reads `project_id` at `middleware/logging.go:101` — **after** `c.Next()`, so it picks
up SPEC-023's rebind to the target project. Capture the caller's project **before** the handler runs.

| Requirement | Rule |
|---|---|
| FR-045 | the log line's caller project is the **operator's** |
| FR-046 / FR-046a | the project acted upon is on the **same log line** as a distinct field beside the caller's, present **only when they differ**. A stored record does not satisfy this — SC-012 forbids consulting one. The existing caller field is never renamed or replaced |
| FR-047 | `user_id` still correct; same-project requests log exactly as today |
| FR-048 | **do not remove the rebind** at `handlers.go:1619` — ~25 sub-handlers scope their queries by it and the override depends on it |

---

## Boot from a new volume (FR-049 – FR-056)

### Request shape

```jsonc
{"server": {"name": "…", "flavorRef": "…",
  "block_device_mapping_v2": [{
     "boot_index": 0, "uuid": "<image id>",
     "source_type": "image", "destination_type": "volume",
     "volume_size": 25, "delete_on_termination": true}]}}
```

`volume_size` **is not parsed at all today** — the struct at `nova/handlers.go:314-320` has no such field,
so it cannot be honoured. Add it.

### Behaviour

| Requirement | Rule |
|---|---|
| FR-049 | a bootable volume is created from the image and the server boots from it |
| FR-050 | a requested size is honoured |
| FR-051 / FR-051a | omitted size → flavor `disk_gb`, floored per FR-052; never zero |
| FR-052 / FR-052a | a size below **either** the image's stated minimum **or** its actual size — whichever is larger — is refused **before** a volume is created, whether that size was requested or defaulted. `images.min_disk_gb` defaults to `0`, so it cannot be the only floor |
| FR-053 | `delete_on_termination` honoured both ways on server deletion |
| FR-054 | the volume appears in the volume service: owner's project, bootable, in use, root attachment |
| FR-055 | volume creation failing → the boot fails per US3, **not** an image-boot fallback |
| FR-056 | an explicit `imageRef`, and an existing boot volume, behave exactly as today |

### Reuse, do not reimplement

- Volume creation: the block-storage service's create-from-image path (`cinder/volumes.go:443-472`,
  sets `source_image_id`, `bootable`).
- Attach and boot: the existing root-volume path (`nova/handlers.go:716-780`), records `/dev/vda` and
  marks the volume in use.
- Deletion: `DeleteServer` already honours `delete_on_termination` (`handlers.go:1392-1400`).

Compute has **no block-storage handle** — add a one-method `CinderService` interface mirroring the
existing `NeutronService`, wired in `cmd/lightstack/main.go` (research.md R8).

Update the `LIMITATION:` comment at `handlers.go:364-369`, which documents the current downgrade.

### Stub mode

Creates the volume row and the attachment with no backing file, matching how the block-storage service
already behaves in stub mode.

### Out of scope, recorded

Quota on the created volume. `CheckQuota` appears **zero** times in `internal/cinder/` — volume creation
is unmetered today, so an implicit volume bypasses no existing check. Adding the product's first
volume-quota enforcement inside a compute ticket would be inconsistent; the gap is pre-existing and
wider than F16.
