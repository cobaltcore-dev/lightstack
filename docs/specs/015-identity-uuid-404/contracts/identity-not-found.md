# Contract: Identity by-id not-found behavior

Scope: every Keystone management route that addresses an identity resource by a **path** id
(see `data-model.md` for the full route table). Query/filter-parameter ids are out of scope.

## Response rules

| # | Precondition | Response |
|---|---|---|
| C1 | id is **malformed** (fails `uuid.Validate`), route not authz-gated | `404`, body `{"itemNotFound":{"code":404,"message":"Could not find <resource>: <id>."}}` |
| C2 | id is **well-formed but unknown** | `404`, **identical** body to C1 (same code + message) |
| C3 | id is **valid and exists** | unchanged success (`200` GET/PATCH, `204` DELETE) |
| C4 | request **unauthenticated** (no/invalid token), any id | `401` (auth middleware; precedes id validation) |
| C5 | request **authenticated but unauthorized** on an authz-gated route (`DeleteUser`, `ChangePassword`, EC2-cred endpoints), id malformed **or** valid | `403` (authz precedes the 404) |
| C6 | id **malformed** on an authz-gated route, caller **is** authorized (admin/owner) | `404` verbatim (in-handler check after authz) |
| C7 | **mutating** op (PATCH/DELETE) with malformed id | `404`, and **no stored data changed** |
| C8 | **non-id** path segment (EC2 `access` key) malformed/arbitrary | **not** treated as a bad id — existing behavior (its own lookup → `404` credential-not-found or success) |
| C9 | **genuine backend/DB error** during a lookup on a touched handler | `5xx` (never collapsed into `404`) |

`<resource>` per route: project, user, role, group, domain, service, endpoint, credential,
application credential (title-cased). Message strings verified against reference Keystone
(`exception.py` / DevStack) at implementation — see `research.md` D7.

## Examples

```bash
# C1 — malformed id → 404 (was 500)
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" \
  "$OS_AUTH_URL/projects/not-a-uuid"                       # => 404

curl -s -H "X-Auth-Token: $TOKEN" "$OS_AUTH_URL/projects/not-a-uuid"
# => {"itemNotFound":{"code":404,"message":"Could not find project: not-a-uuid."}}

# C2 — valid-but-unknown UUID → identical 404
curl -s -H "X-Auth-Token: $TOKEN" \
  "$OS_AUTH_URL/projects/00000000-0000-0000-0000-0000000000ff"
# => {"itemNotFound":{"code":404,"message":"Could not find project: 00000000-0000-0000-0000-0000000000ff."}}

# C5 — non-admin deleting a user by a malformed id → 403 (authz precedes 404)
curl -s -o /dev/null -w '%{http_code}\n' -X DELETE -H "X-Auth-Token: $NONADMIN_TOKEN" \
  "$OS_AUTH_URL/users/not-a-uuid"                          # => 403

# C8 — EC2 access key is not a uuid path-id; still works
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Auth-Token: $TOKEN" \
  "$OS_AUTH_URL/users/$USER_ID/credentials/OS-EC2/somekey" # => 404 credential-not-found (not a malformed-id 400/500)
```

## Client-level contract (Constitution Art. I)

- `openstack project show not-a-uuid` → `NotFound` (not `HttpException 500`).
- Name→id resolution via gophercloud / python-openstackclient / Terraform (probe `/{name}` → 404 → fall back to `?name=`) succeeds for `--project <name>`, `role add <role-name>`, etc.

## Non-goals (this contract)

- Identity-v3 `{"error":{code,title,message}}` envelope (kept Nova-style; #518).
- Query/filter-parameter ids.
- Error-masking cleanup on handlers this feature does not touch.
