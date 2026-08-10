# Contract: Authentication scope vocabulary

**Feature**: `023-tenant-scoping-authz` | Covers FR-009 – FR-017, User Story 2

All requests go to `POST /v3/auth/tokens`. Credentials are `admin` / `secret` in domain `Default` unless stated.

---

## S1 — A domain scope is refused (FR-009)

```json
{"auth": {"identity": {...}, "scope": {"domain": {"name": "Default"}}}}
```

```
→ 400
   body names the domain scope as unsupported
```

MUST NOT be `201`. The current behaviour — `201` with `has_project=false`, `has_domain_scope=false`, `roles=0` — is the defect, and is the exact shape FR-010 forbids: a success that omits both the scope asked for and any roles.

Reproduction from the ticket, which MUST change:

```bash
curl -s -X POST localhost:35357/v3/auth/tokens -H 'Content-Type: application/json' \
  -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"domain":{"name":"Default"}}}}' \
  | jq '{has_project:(.token|has("project")), has_domain_scope:(.token|has("domain")), roles:(.token.roles|length)}'
```

Now: `{"has_project":false,"has_domain_scope":false,"roles":0}` with `201`.  
Expected: `400`.

## S2 — The refusal is uniform (FR-014)

Every one of these MUST produce the same status and the same body:

```json
{"scope": {"domain": {"name": "Default"}}}                     ← exists
{"scope": {"domain": {"name": "no-such-domain"}}}              ← does not exist
{"scope": {"domain": {"id": "default"}}}                       ← well-known alias
{"scope": {"domain": {"id": "00000000-0000-0000-0000-000000000009"}}}  ← unknown UUID
{"scope": {"domain": {"id": "not-a-uuid"}}}                    ← malformed
```

A differing answer for any of them discloses which domains exist. Uniformity follows from refusing **before** resolving the reference — the implementation MUST NOT query the domain table on this path.

A malformed identifier MUST produce the same `400`, never a `500`.

## S3 — Distinguishable from a credential failure (FR-013)

```
domain scope, valid credentials    → 400
wrong password, no scope           → 401
```

## S4 — The nested domain qualifier still works (FR-015) ⚠ REGRESSION GUARD

This is the form Horizon, Terraform and `python-openstackclient` send on **every** authentication. Catching it would break every client.

```json
{"auth": {"identity": {...},
          "scope": {"project": {"name": "default",
                                "domain": {"name": "Default"}}}}}
```

```
→ 201   token is project-scoped, carries roles, body identical to today's
```

The check MUST test for a **top-level** `scope.domain` only, never `scope.project.domain`.

## S5 — Existing scopes are byte-identical (FR-016)

```json
{"scope": {"project": {"name": "default", "domain": {"name": "Default"}}}}   → 201, unchanged
{"scope": {"project": {"id": "<uuid>"}}}                                     → 201, unchanged
{"scope": "unscoped"}                                                        → 201, unchanged
(no scope key at all)                                                        → 201, unchanged
```

**S5b — no domain discovery endpoint appears.** With no domain scope to grant, there is nothing for it to report:

```
GET /v3/auth/domains   → 404 (unchanged; still absent)
```

## S6 — Both authentication paths agree (FR-016)

The password path and the token re-scoping path MUST answer identically. Obtain a token, then re-scope it:

```json
{"auth": {"identity": {"methods": ["token"], "token": {"id": "<token>"}},
          "scope": {"domain": {"name": "Default"}}}}
```

```
→ 400   same status and body as S1
```

## S7 — Contradictory scope (FR-012)

```json
{"scope": {"project": {"name": "default"}, "domain": {"name": "Default"}}}
```

```
→ 400   refused as contradictory, not resolved by precedence
```

## S8 — Pre-existing tokens keep working (FR-017)

A token issued before the change validates afterwards and carries exactly the authority it carried when issued.
