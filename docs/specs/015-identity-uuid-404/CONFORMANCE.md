# OpenStack Keystone conformance — SPEC-015 (identity 404 for non-UUID ids)

How the [#515](https://github.com/cobaltcore-dev/cloud-storage/issues/515) fix
aligns with the real OpenStack Keystone Identity API v3, and where it
**intentionally** differs — with the design decision that authorized each
divergence. Conformance target: OpenStack Flamingo Keystone (Constitution Art. I).

## Summary matrix

| Behavior | Real Keystone | o3k after #515 | Conformant? | Basis |
|---|---|---|---|---|
| Non-UUID path id on a by-id lookup | 404 — ids are opaque strings, unknown → NotFound | 404 | ✅ match | FR-001, ticket |
| Valid-but-unknown id | 404, same response as any unknown id | 404, byte-identical to the malformed case | ✅ match | FR-004 (Q1) |
| Not-found message text | `Could not find <resource>: <id>.` (`exception.py`), incl. title-cased `Application Credential` | same, verbatim | ✅ match | FR-004; `project` DevStack-verified in #73 |
| 404 vs 400 for a malformed id | 404, not bad-request | 404 | ✅ match | Assumptions — "404, not 400" |
| Order authn → authz → lookup | 401, then 403, then 404 | 401 (authN mw) → 403 (in-handler authz) → 404 (id checked *after* authz) | ✅ match | FR-009 (Q4=B) |
| Genuine backend fault | 5xx (never a masked 404) | 5xx (`notFoundOr500` / `existsOr404` only 404 on `ErrNoRows`/`!exists`) | ✅ match | FR-007 / FR-010 |
| Error envelope | Identity v3 `{"error":{code,title,message}}` | o3k Nova-style `{"itemNotFound":{code,message}}` | ⚠️ **intentional divergence** | Q3 / research D5 — envelope tracked surface-wide in **#518** |
| Query/filter id (`?user.id=`, …) | usually 200 empty-list for an unknown filter | unchanged | ⚠️ **out of scope** | Q2 — separate follow-up ticket |
| EC2 `access` key not-found | 404 | 404 (o3k's existing message, unchanged) | ⚠️ **out of scope** | FR-005 / contract C8 — `access` is not a UUID identifier |

## Notes

- **Status is the load-bearing conformance point.** OpenStack clients
  (python-openstackclient, gophercloud, Terraform) key their name→id fallback on
  the 404 **status**, not the body — so the Nova-style envelope (deferred to
  #518) does not break name resolution. Verified end-to-end in `HOST_TESTING.md`
  (`openstack project show <name>` resolves without a 500).
- **Taxonomy check.** Keystone's own api-ref (via Context7 `/openstack/keystone`)
  lists 404 *"resource could not be found"* as a code distinct from the 5xx
  server/service errors — corroborating FR-007/FR-010.
- **Message verification.** Strings are matched to Keystone's `exception.py`
  `*NotFound` messages; `project` is DevStack-verified (#73). Task T025 recommends
  a DevStack spot-check of the remaining nouns before the PR.

## Deferred (tracked elsewhere)

- **#518** — convert the keystone error envelope to Identity v3 `{"error":{…}}`
  surface-wide (this fix keeps the existing Nova-style envelope).
- **Follow-up ticket** — malformed ids in query/filter parameters
  (`role_assignments?user.id=`, …), which Keystone answers with an empty list.
- **authZ policy layer** — centralize the ad-hoc in-handler authorization
  (oslo.policy-style); would let the malformed-id middleware cover every route
  uniformly instead of the current hybrid.
