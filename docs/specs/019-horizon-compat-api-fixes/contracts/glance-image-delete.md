# Contract — Glance image create/delete authorization (F6)

**Service**: Image (Glance) v2. **Clients**: Horizon Images panel, python-openstackclient, gophercloud.
**Upstream basis**: default `delete_image` policy = `not_protected_and_is_owner` + admin override
(Context7 `/openstack/glance`).

## Endpoints

- `POST /v2/images` — Create image
- `DELETE /v2/images/{id}` — Delete image

## Contract

**Create**: the created image records its owning project (`owner`/`project_id`) for **every** visibility
(public, private, shared, community). Visibility governs *who can see* the image; ownership governs *who can
delete* it. (Previously public images were stored ownerless.)

**Delete** authorization and status codes:

| Condition | Result |
|---|---|
| Image id does not exist | **404** Not Found |
| Image exists but caller can't see it (private, not owner, not admin) | **404** (do not leak existence) |
| Image visible to caller but not authorized to delete (e.g. public, caller not owner, not admin) | **403** Forbidden |
| Caller is the owner **or** an admin | delete succeeds → **204**, image removed from `image list` |
| Image in use by an instance/volume | **409** Conflict (existing guard, unchanged) |

- "Admin" = the JWT `roles` contain `admin` (read via `c.GetStringSlice("roles")`).
- After a successful delete the DB row is removed regardless of the best-effort backend object delete, so the
  image disappears from `image list` in **both** stub and real (RGW/S3) modes.
- A delete that matches zero rows MUST surface as 404 (rows-affected checked), never a silent 204.

## Contract-test assertions

1. Owner creates image → deletes it → `image list` no longer shows it (stub **and** real).
2. Admin creates a `--public` image → deletes it → gone (does not reappear).
3. Non-owner non-admin deletes a public image → **403**, image remains.
4. Delete a random/again-deleted id → **404**.
5. Delete an image in use by an instance/volume → **409** (unchanged).
