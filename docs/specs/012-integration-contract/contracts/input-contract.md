# Input Contract — `values.schema.json`

The input half of the integration contract: what a consumer (sci-in-a-box or an operator) may set,
and what the chart guarantees to reject **before any workload is created**. Enforced by Helm's
built-in `values.schema.json` validation on `install`/`upgrade`/`lint`/`template`.

## Guarantees

- **C-IN-1 — Documented.** Every input has a `description` in the schema (FR-002, SC-001).
- **C-IN-2 — Typed/bounded.** Every input has a structural rule: `type`, and where applicable `enum`
  / `minimum` / `minLength` / `pattern` (see `data-model.md` §1).
- **C-IN-3 — Required structural inputs rejected when absent.** `image.repository` (non-empty).
- **C-IN-4 — Unknown keys rejected.** `additionalProperties: false` on the top level and typed
  objects; a misspelled key (e.g. `databse:`) fails validation. Free-form maps (`*.annotations`,
  `resources`, `nodeSelector`, `tolerations`, `affinity`) stay open (C-IN-8).
- **C-IN-5 — External DB refs modeled.** `database.{url,existingSecret,existingSecretKey}` are typed
  and described (FR-005).
- **C-IN-6 — `version` is `image.tag`.** Typed string (registry-defined, no enum), defaulting to
  `appVersion`. `multiNode.minProductVersion` is a separate advisory floor, documented as such.
- **C-IN-7 — Credentials presence-only.** The schema rejects empty inline `jwtSecret`/`adminPassword`
  only via the render-time guard (C-IN-9); it does **not** enforce length/complexity. Strength is
  documented guidance. `existingSecret`-provided values are never schema-validated.
- **C-IN-8 — k8s passthrough preserved.** Open maps validate as object/array without key restriction.
- **C-IN-9 — Conditional requirements (render-time, not schema).** Enforced as fail-fast template
  guards with helm-unittest coverage, NOT schema `if/then` (R1):
  - `jwtSecret`, `adminPassword`, `database.url` required unless the corresponding `existingSecret`.
  - `teardown.kubectlImage` required when `dataRetentionPolicy: delete`.
  - multi-node + real rejects `local` glance/cinder backends.
- **C-IN-10 — Cross-field classification (FR-007a).** Release-breaking/unsafe combos are **rejected**
  (C-IN-9); benign capacity/topology preferences are **advisory** — real mode auto-pins to 1 replica;
  multi-node + cache without a shared Redis warns in `NOTES.txt`. Neither blocks otherwise-valid installs.

## Rejection test matrix (RED → GREEN; R5)

Each invalid fixture MUST fail `helm template`; the valid fixture MUST render.

| Case | Fixture intent | Expected |
|---|---|---|
| unknown-key | `databse: ...` (typo) | reject (additionalProperties) |
| bad-enum | `service.type: Foo` | reject (enum) |
| bad-storagemode | `glance.storageMode: nfs` | reject (enum) |
| out-of-range | `replicaCount: 0` | reject (minimum) |
| missing-image | no `image.repository` | reject (required/minLength) |
| missing-required (render-time) | no `jwtSecret`, no `existingSecret` | reject (template guard, errorPattern) |
| malformed-db | `database: "postgres://..."` (string, not object) | reject (type) |
| valid-complete | all required + valid combos | render OK |

## Coverage invariant (drift gate)

`{top-level keys in values.yaml} ≡ {properties in values.schema.json}`. Any asymmetry fails CI —
prevents `additionalProperties: false` from rejecting a real key and prevents an undocumented input
(SC-001/SC-005).
