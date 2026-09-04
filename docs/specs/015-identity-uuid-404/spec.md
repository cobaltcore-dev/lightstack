# Feature Specification: Identity resource not-found for malformed ids

**Feature Branch**: `015-identity-uuid-404`  
**Created**: 2026-07-10  
**Status**: Draft  
**Input**: cloud-storage#515 — "lightstack Identity API returns 500 instead of 404 for non-UUID project/user/role ids" (issue title + body + the two scoping comments agreeing full-identity-surface coverage)

## User Scenarios & Testing *(mandatory)*

The Identity service exposes resources (projects, users, roles, and others) that clients look up by an identifier in the request path. The product's identifiers are UUIDs. Today, when a client supplies an identifier that is **not a well-formed UUID**, the service returns a **server error (HTTP 500)** instead of **not-found (HTTP 404)**. A well-formed-but-unknown identifier already returns 404 correctly — so the fault is triggered purely by the identifier's *shape*, not by whether the resource exists.

This breaks the standard **name → id resolution** that OpenStack clients (CLI, Terraform provider, SDKs, dashboard) rely on: to resolve a resource by name, a client first probes `GET /v3/{resource}/{name}` and expects a **404** so it can fall back to a `?name=` filter. A 500 aborts that flow, so any command that references an identity resource by name fails.

### User Story 1 - Reference identity resources by name (Priority: P1)

An operator or automation references an identity resource by its **name** rather than its id — for example `openstack project show <name>`, `--project <name>`, or granting a role with `role add <role-name>`. The client's resolver probes the name as if it were an id, receives a clean not-found, falls back to a name-filtered lookup, and the command succeeds.

**Why this priority**: This is the user-visible breakage in the ticket. Without it, everyday CLI and Terraform workflows that use names (the common case) fail with an opaque server error. Restoring it delivers the core value on its own.

**Independent Test**: With a running service, run `openstack project show <existing-name>` and a Terraform plan that references a project/role by name. Both complete successfully; neither returns a 500. Can be validated end-to-end without any other story.

**Acceptance Scenarios**:

1. **Given** a project that exists by name, **When** a client requests it by name via the standard probe-then-filter flow, **Then** the initial by-id probe returns 404 and the client successfully resolves the project by name.
2. **Given** the OpenStack CLI, **When** an operator runs `openstack project show <a-name-that-is-not-a-uuid>`, **Then** the result is a clean not-found (or the resolved resource), never a server error.
3. **Given** a Terraform configuration referencing a role by name, **When** it is applied, **Then** role resolution succeeds without a 500.

---

### User Story 2 - Consistent not-found across the whole identity surface (Priority: P2)

A client (or test) requests any identity resource by a malformed identifier — on read, update, or delete, including nested resources such as role assignments and group memberships. The service consistently returns not-found, matching the Identity API v3 contract and matching the response already returned for a well-formed-but-unknown id.

**Why this priority**: The root cause is shared by every identity lookup keyed on an identifier, not only projects/users/roles. Fixing the whole surface at once prevents an inconsistent, whack-a-mole contract where some resources 404 and others 500.

**Independent Test**: For each identity resource type, issue a by-id request with a malformed identifier and assert the response is not-found (404) and never a server error (500). Assert a well-formed-but-unknown identifier still returns 404, and an existing identifier still succeeds.

**Acceptance Scenarios**:

1. **Given** any identity resource type (project, user, role, group, domain, service, endpoint, credential, application credential, EC2 credential), **When** a client reads/updates/deletes it by a malformed identifier, **Then** the response is not-found (404).
2. **Given** a nested identity path (e.g. a role assignment `.../users/{user}/roles/{role}` or a group membership), **When** any identifier segment is malformed, **Then** the response is not-found (404).
3. **Given** a well-formed-but-unknown identifier, **When** a client requests it, **Then** the response remains 404, with its message normalized to the same Keystone-verbatim text as the malformed case (FR-004).
4. **Given** an existing identifier, **When** a client requests it, **Then** the resource is returned (or deleted) successfully — unchanged.

---

### User Story 3 - Malformed input does not masquerade as a server fault (Priority: P3)

Operators monitoring the service do not see malformed-identifier requests reported as internal server errors, and genuine backend failures are still reported honestly as server errors (they are not hidden as not-found).

**Why this priority**: Correctness of observability. A malformed id is a client mistake (4xx), not a service fault (5xx); miscategorising it creates false alarms. Conversely, the fix must not "achieve" 404 by swallowing real faults — that would trade one wrong signal for another.

**Independent Test**: Send malformed-id requests and confirm they are categorised as client errors (4xx), not server errors (5xx). Separately, simulate a genuine backend failure and confirm it still surfaces as a server error (5xx), not a not-found.

**Acceptance Scenarios**:

1. **Given** a malformed identifier, **When** the request is handled, **Then** it is categorised as a client error (4xx), not a server error (5xx).
2. **Given** a genuine backend/data-store failure, **When** an identity request is handled, **Then** it still surfaces as a server error (5xx) — the not-found behavior for malformed ids does not mask real faults.

### Edge Cases

- **Malformed identifier variants**: empty, whitespace, overly long, or punctuation/query-like strings all resolve to not-found (404), never a server error, and never cause unsafe behavior.
- **Non-identifier path segments**: path segments that are *not* resource identifiers — notably an EC2 credential **access key** — must continue to be accepted and matched as before; they must not be rejected as "malformed ids".
- **Mutating operations**: a malformed identifier on an update or delete returns not-found and **modifies no stored data**.
- **Multi-segment paths**: when several identifiers appear in one path, a malformed value in *any* required identifier segment yields not-found.
- **Well-formed-but-unknown**: continues to return 404 (status unchanged — regression guard); its message is normalized to the same Keystone-verbatim text as the malformed case (FR-004).
- **Authorization precedes not-found**: on authz-gated endpoints, an unauthorized caller gets 403 (and an unauthenticated caller 401) even for a malformed id — see FR-009.

## Clarifications

### Session 2026-07-14

- Q: Which not-found cases get Keystone's verbatim message? → A: All identity not-found responses — both malformed and valid-but-unknown ids — return Keystone-verbatim `"Could not find <resource>: <id>."`; the two cases are byte-identical and match Keystone (normalizing the valid-but-unknown path is in scope, not only the malformed path).
- Q: Are query/filter-parameter ids (e.g. `role_assignments ?user.id=`) in scope? → A: No — path ids only. Malformed ids in query *filters* are a distinct surface (different expected behavior — often a 200 empty-list, not 404) and are tracked as a separate follow-up.
- Q: Which error envelope does the 404 use? → A: Keep lightstack's existing Nova-style envelope (`{"itemNotFound": {…}}`); only status + message are made Keystone-exact. Identity-v3 `{"error":{…}}` envelope conformance is deferred surface-wide to #518.
- Q: For admin-/owner-gated endpoints, does an unauthorized caller's *malformed* id return 403 or 404? → A: 403 — authorization precedence (like authentication → 401), decided before id validation, matching Keystone's authn→authz→look-up order. Affects user-delete, password-change, and the EC2-credential endpoints; implies id validation on those runs *after* their authz check, not via a pre-handler middleware.
- Q: On handlers that currently mask all errors as 404 (domains, `UpdateUser`, `GET /users/:id/{projects,groups}`), fix the masking or just change the message? → A: Fix it. Since #515 already rewrites their not-found path for the verbatim message (Q1), those handlers also start distinguishing `ErrNoRows` (→ verbatim 404) from a genuine error (→ 500) — matching Keystone (verified via `/openstack/keystone` api-ref: 404 = "resource could not be found", a distinct status from the 5xx server/service errors; a backend fault is never collapsed into 404). Masking on handlers #515 does not touch stays a separate follow-up.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Identity resource operations addressed by an identifier in the request path (read, update, delete, and existence-style checks) MUST return not-found (404) when the identifier does not correspond to an existing resource, **including** when the identifier is not a well-formed resource identifier. Subject to FR-009 (authorization precedence).
- **FR-002**: This behavior MUST apply uniformly across the entire identity resource surface — projects, users, roles, groups, domains, services, endpoints, credentials, application credentials, and EC2 credentials — and to nested/related resources addressed by **path** id (role-assignment paths, group memberships, and user sub-resource listings). Ids supplied as query/filter parameters are out of scope (see Out of Scope).
- **FR-003**: A malformed identifier MUST NOT produce a server-error (5xx) response.
- **FR-004**: Malformed and valid-but-unknown identifiers MUST return the **identical** not-found response, whose message matches Keystone verbatim — `"Could not find <resource>: <id>."` — so the two cases are indistinguishable to clients and conform to Keystone. Normalizing the valid-but-unknown path to this message is in scope (not only the malformed path). The 404 status is what triggers client name→id resolution (probe-by-id, then fall back to name filter).
- **FR-005**: Path segments that are not resource identifiers (e.g. an EC2 credential access key) MUST continue to be accepted and MUST NOT be treated as malformed identifiers.
- **FR-006**: A malformed identifier supplied to a mutating operation (update or delete) MUST NOT modify any stored data.
- **FR-007**: The malformed-identifier not-found behavior MUST NOT be implemented by masking genuine backend/server faults as not-found; real server errors MUST continue to surface as server errors.
- **FR-008**: Clients referencing identity resources by name (e.g. `--project <name>`, adding a role by name) MUST succeed via the standard probe-then-name-filter flow.
- **FR-009**: On identity endpoints gated by authorization (admin-only or owner-or-admin — e.g. user delete, password change, EC2-credential access), the authorization outcome MUST take precedence over the malformed-id not-found: an unauthorized caller receives **403** even when the id is malformed, and an unauthenticated caller receives **401**. Id validation on these endpoints therefore occurs **after** the authorization check (matching Keystone's authenticate → authorize → look-up order), so it cannot rely solely on a pre-handler validation step.
- **FR-010**: Where this feature rewrites an identity handler's not-found path for the verbatim message (FR-004), that handler MUST distinguish a genuine backend error (→ **5xx**) from an actual not-found (→ **404**); pre-existing "treat any error as not-found" masking MUST be removed on the handlers this feature touches. Masking on handlers this feature does **not** touch remains a separate follow-up (see Out of Scope). Matches Keystone: 404 only for a missing resource, 5xx for an unexpected backend/database error.

### Key Entities

- **Identity resource identifier**: the opaque value in a request path that addresses a single identity resource. The product issues these as UUIDs; from the client's perspective they are opaque strings. An unknown or malformed value addresses no resource and therefore resolves to not-found.
- **Identity resources in scope**: project, user, role, group, domain, service, endpoint, credential, application credential, EC2 credential — plus nested addressing (role assignment, group membership, user sub-resources).
- **Non-identifier path values**: values in the path that are not resource identifiers (e.g. EC2 access key), which are matched by their own rules and are out of scope for identifier validation.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Across every in-scope identity endpoint, 100% of by-id requests using a malformed identifier return not-found (404) and 0% return a server error (500).
- **SC-002**: OpenStack CLI and Terraform commands that reference identity resources by name complete successfully, with no 500 arising from the by-id probe step.
- **SC-003**: For an identical not-found request (malformed **or** valid-but-unknown id), the service returns the same status **and** the same message text as a reference OpenStack Keystone deployment.
- **SC-004**: No regression: well-formed-but-unknown identifiers still return 404, existing identifiers still return their resource (or delete successfully), and genuine backend faults still report as 5xx.
- **SC-005**: Server-error (5xx) responses attributable to malformed identity identifiers drop to zero.

## Assumptions

- **404, not 400, for malformed ids**: Upstream OpenStack Keystone treats identifiers as opaque strings and returns not-found for anything unknown; it does not reject non-UUID ids as bad-request. The ticket also requires 404 specifically, because client name→id resolution keys its fallback on a 404. A 400 would not trigger that fallback. Therefore malformed ids resolve to 404.
- **Identifier format**: the product's identity identifiers are UUIDs, so any identifier that is not a well-formed UUID cannot match a stored resource. Standard UUID textual formats are accepted; anything outside them is malformed.
- **Scope decided with the reporter**: coverage is the full identity surface (not only projects/users/roles), agreed in the issue comments.
- **Not-found message**: all identity by-id not-found responses (malformed and valid-but-unknown) use Keystone's verbatim message `"Could not find <resource>: <id>."` (FR-004), replacing lightstack's prior generic not-found text on the identity surface. The response *envelope* is a separate concern (see Out of Scope).

## Out of Scope

- Repairing error-masking (`if err != nil → NotFound`) on handlers this feature does **not** touch. Handlers whose not-found path #515 *does* rewrite for the verbatim message (domains, `UpdateUser`, `GET /users/:id/{projects,groups}`) get their masking fixed here (genuine error → 5xx; FR-010); a full audit/cleanup of masking on the remaining handlers is tracked independently.
- Changes to non-identity services (compute, network, block storage, image) — even if they share similar patterns.
- Any change to how resources are looked up by name (the `?name=` filter itself), which already works.
- **Query/filter-parameter ids** (e.g. `GET /v3/role_assignments?user.id=`, `?scope.project.id=`): a malformed id in a query *filter* can also currently 500, but it's a distinct surface — the path-param mechanism doesn't see query params, and Keystone's expected response for an unknown filter is typically an empty list (200), not 404. Fixing it needs per-endpoint, handler-level handling with its own Keystone verification; tracked as a separate follow-up.
- **Identity-v3 error envelope**: not-found responses keep lightstack's existing Nova-style envelope (`{"itemNotFound": {…}}`); only status and message are made Keystone-exact here. Converting keystone to Keystone's `{"error": {code, title, message}}` envelope is a surface-wide change tracked in #518.

## Dependencies

- Conformance target: OpenStack Identity API v3 (https://docs.openstack.org/api-ref/identity/v3/) — the authoritative contract for identity resource not-found behavior.
- Client resolvers that depend on this behavior: OpenStack CLI, Terraform OpenStack provider / gophercloud SDK, Horizon dashboard.
