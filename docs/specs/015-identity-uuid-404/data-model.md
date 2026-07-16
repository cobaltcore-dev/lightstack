# Phase 1 Data Model: Identity resource not-found for malformed ids

## Database

**No schema change. No migration.** The identity id columns remain Postgres `uuid`
(`migrations/001` users/projects/roles/role_assignments, `028` groups, `029` application_credentials,
`037` domains, `040` services/endpoints, `041` credentials). `credentials.access_key` stays
`VARCHAR(255)` (`062`). This feature only changes how errors map to HTTP responses.

## Conceptual entities (no persistence changes)

### Identity resource identifier
- The opaque value in a request path addressing one identity resource.
- Issued as a UUID; compared as `uuid`. A value that is not a well-formed UUID matches no row.

### Error response (existing shape, reused)
- `common.OpenStackError{ StatusCode, Code, Message, Details }` → JSON `{ "<Code>": { "message", "code" } }`.
- Not-found here: `StatusCode=404`, `Code="itemNotFound"`, `Message="Could not find <resource>: <id>."`, no `Details`. (Nova-style envelope retained; Identity-v3 envelope → #518.)

## Route → param → resource classification

The middleware/handlers derive the resource noun as `singularize(path segment before the param)`
(`singularize`: strip trailing `s`, `_`→space; override `application_credentials → "Application Credential"`).

| Route (method) | Path params (uuid) | Non-id params | Resource noun(s) | Handling |
|---|---|---|---|---|
| `/projects/:id` (GET/PATCH/DELETE) | `id` | — | project | middleware |
| `/projects/:id/users/:user_id/roles/:role_id` (PUT/DELETE/GET) | `id`,`user_id`,`role_id` | — | project / user / role | middleware |
| `/projects/:id/users/:user_id/roles` (GET) | `id`,`user_id` | — | project / user | middleware |
| `/users/:id` (GET) | `id` | — | user | middleware |
| `/users/:id` (PATCH `UpdateUser`) | `id` | — | user | middleware **+** masking fix |
| `/users/:id` (DELETE `DeleteUser`, admin) | `id` | — | user | **in-handler after authz** |
| `/users/:id/password` (POST `ChangePassword`, self-or-admin) | `id` | — | user | **in-handler after authz** |
| `/users/:id/projects`, `/users/:id/groups` (GET) | `id` | — | user | middleware **+** masking fix |
| `/users/:id/application_credentials[/:cred_id]` (POST/GET/DELETE) | `id`,`cred_id` | — | user / application credential | middleware |
| `/application_credentials/:id` (GET) | `id` | — | application credential | middleware |
| `/users/:id/credentials/OS-EC2[/:access]` (POST/GET/DELETE, self-or-admin) | `id` | `access` | user | **in-handler after authz** (`access` never validated) |
| `/groups/:id`, `/groups/:id/users`, `/groups/:id/users/:user_id` | `id`,`user_id` | — | group / user | middleware (+ masking fix on Get/Update/Delete group) |
| `/roles/:id` (GET/PATCH/DELETE) | `id` | — | role | middleware |
| `/domains/:id`, `/domains/:id/config` | `id` | — | domain | middleware **+** masking fix |
| `/services/:id` (GET/PATCH/DELETE) | `id` | — | service | middleware |
| `/endpoints/:id` (DELETE) | `id` | — | endpoint | middleware |
| `/credentials/:id` (GET/PATCH/DELETE) | `id` | — | credential | middleware |

Public routes (`GET /v3`, `/v3/auth/*`, `/v3/s3tokens`, `/v3/ec2tokens`) have no uuid path param and are not on `mgmt` → untouched.

## State / validation rules

- **Malformed id** (fails `uuid.Validate`): → 404 verbatim. On middleware-covered routes, before the DB. On authz-gated routes, after the authz check (so 401/403 win).
- **Well-formed, unknown id** (`pgx.ErrNoRows`): → 404 verbatim (identical response).
- **Well-formed, existing id**: unchanged (200/204).
- **Genuine backend error** on a touched handler: → 5xx (never masked as 404) — FR-010.
- **`access` and non-`*_id` params**: never validated as uuids — FR-005.
