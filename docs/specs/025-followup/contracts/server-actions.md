# Contract: the `restore` server action (US5, FR-035 – FR-037)

**Surface**: `POST /v2.1/servers/{id}/action`
**Today**: `restore` is routed and gates on a `SOFT_DELETED` status that nothing in the product ever
writes, so it can never succeed in any state a server can reach. Upstream is in the same position at
its default reclaim interval, and keeps the action routed.

## Change

`restore` **stays routed and keeps upstream's answers**. Only its success branch, which could never
run, is removed.

```
POST /v2.1/servers/{id}/action   {"restore": null}
→ 404  the server does not exist (deletion is immediate)
→ 409  the server exists, so it is not soft-deleted — none can be
```

Upstream (`nova/api/openstack/compute/deferred_delete.py`) routes `restore` at every configuration,
with expected errors 403/404/409. Its 400 "There is no such action" is for names it does not route.

## Invariants

1. **`restore` never receives the unknown-action answer.**
2. **Deletion semantics are unchanged.** Deletion stays immediate — which is what an upstream
   deployment does at the default reclaim interval, where soft delete is disabled.
3. **No other action's answer changes.**
4. **No new persisted state.** Deferred delete is not introduced: no soft-deleted status, no
   retention window, no reclaim sweep, and no quota interaction with retained servers.

## Evidence required

409 asserted for a live server and 404 for a deleted one, and the gophercloud contract test's
restore-after-delete case (FR-037).
