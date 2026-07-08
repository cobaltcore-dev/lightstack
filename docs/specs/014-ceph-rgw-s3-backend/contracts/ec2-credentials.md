# Contract — EC2 credentials API (o3k Keystone)

OpenStack-compatible EC2/S3 credential management. Matches `openstack ec2 credentials …`.
Routes on the **authenticated** `mgmt` group (user token via `X-Auth-Token`).

## Endpoints

### POST /v3/users/{user_id}/credentials/OS-EC2
Create a long-lived, project-scoped S3 access/secret pair.
Request:
```json
{ "tenant_id": "<project_id>" }
```
201:
```json
{ "credential": {
  "user_id": "<user_id>",
  "tenant_id": "<project_id>",
  "access": "<access_key>",
  "secret": "<secret_key>",
  "trust_id": null,
  "links": { "self": ".../v3/users/<user_id>/credentials/OS-EC2/<access>" }
} }
```
- Access/secret generated with `crypto/rand`; stored in `credentials` (`type=ec2`, `access_key`, `blob`).
- Caller may create for their own `user_id`; admin may create for others (role check).

### GET /v3/users/{user_id}/credentials/OS-EC2
200 → `{ "credentials": [ {credential…, secret omitted or included per OpenStack behavior} ] }`.

### GET /v3/users/{user_id}/credentials/OS-EC2/{access}
200 → `{ "credential": { … } }`; 404 if not found / not owned.

### DELETE /v3/users/{user_id}/credentials/OS-EC2/{access}
204 → row deleted (this is revocation). Subsequent `s3tokens` lookups for that access fail.

## Rules
- `access` unique across all credentials.
- Scoping: credential bound to `user_id` + `tenant_id`(project); returned in `s3tokens` as the RGW tenant.
- No expiry required (long-lived); optional `expires_at` may be added later.

## Test (contract, Test-First)
- `openstack ec2 credentials create/list/show/delete` round-trip (openstackclient).
- Created secret can subsequently sign an S3 request that RGW→`s3tokens` accepts.
- Deleted credential → S3 request rejected.
