# Contract: compute quota on server create

FR-035, FR-035a, SC-005 (research R14). Same on both backends.

## Order of checks in `POST /servers`

1. **Early checks** (non-binding, unchanged): instances, cores, RAM — refuse before anything is provisioned.
2. Boot volume, when requested, is provisioned under its own Cinder reservation (contract [block-storage-reservation.md](block-storage-reservation.md)).
3. **Binding recheck**, in the transaction that inserts the instance: a per-project advisory lock (`nova_quota:<project>`; a no-op on SQLite, where immediate transactions serialise), then usage and limit for instances, cores and RAM, then the insert. A refusal rolls the insert back and removes the boot volume provisioned in step 2.

## Responses

| Outcome | Status | Body |
|---|---|---|
| created | `202` (unchanged) | unchanged |
| refused by step 1 or step 3 | **`403`** (was `413 overLimit`) | `{"forbidden": {"code": 403, "message": "Quota exceeded for instances: Requested 1, but already used 10 of 10 instances"}}` — `cores` and `ram` analogous |
| refused for more than one resource at once | **`403`** | every exceeded resource named, sorted, each field joined by `, ` — e.g. `Quota exceeded for cores, ram: Requested 2, 4096, but already used 1, 512 of 2, 4096 cores, ram` (upstream's `get_over_quota_detail`) |
| boot volume refused by its block-storage quota | **`403`** (was `413`) | `Quota exceeded for gigabytes: Requested <n>, but already used <u> of <l> gigabytes` (or `volumes`); a request over the per-volume size ceiling answers `Requested volume size <n>G is larger than maximum allowed limit <l>G.` |

Message form: upstream's `TooManyInstances` (`nova/exception.py:1383-1385`, Flamingo), raised as `HTTPForbidden` on create (`nova/api/openstack/compute/servers.py:797-799`). Cinder's own endpoints keep their `413`, which is upstream's.

## Concurrency guarantee

Two creates racing for the last instance of a project end as exactly one `202` and one `403`, in 20 of 20 runs, on each backend (SC-005) — including projects that have no `quotas` row and run on the code defaults, which is why the lock is advisory rather than a row lock.
