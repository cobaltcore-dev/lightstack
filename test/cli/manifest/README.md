# Command Manifests — the definition of "all cases" (SPEC-028)

Each service owns one JSON file here (`keystone.json`, `compute.json`, `network.json`,
`volume.json`, `image.json`, plus `smoke.json` and `workflows.json`). `bin/coverage.sh`
globs every `*.json` and cross-references the entries with the recorded case results —
the gate fails on any tier A/B command that is `untested`, `fail`, or `skip`.

## Schema

```json
{
  "service": "keystone",
  "description": "optional, one line",
  "commands": [
    {
      "id": "keystone.project.list",
      "cmd": "openstack project list",
      "endpoints": ["GET /v3/projects"],
      "tiers": ["A", "B"]
    }
  ]
}
```

| Field      | Meaning |
|------------|---------|
| `id`       | Globally unique, `<service>.<noun>.<verb>`. Doubles as the tier A case id in the suite. |
| `cmd`      | Documentation for humans — the command shape this entry covers. |
| `endpoints`| The lightstack API endpoint(s) the command exercises (inventory reference). |
| `tiers`    | `A` happy path + output shape; `B` negative (case id: `<id>.neg`); `C` workflow scenario (workflows.json only); `S` smoke (not gated). |

## Tier → case-id mapping

- Tier A: the manifest `id` is the case id.
- Tier B: `<id>.neg` is the case id.
- Tier C: the workflow `id` is the case id, used verbatim.

## Statuses

`pass`, `fail`, `known-failing`, `skip`, `untested` — derived from the latest results
(`test/cli/results/results.json`). The gate allows `known-failing` (tracked, must
shrink) and fails everything else in the required tiers.

## Rules

1. Only add commands lightstack's implemented endpoints can actually serve — the
   repo inventory is `docs/API_COVERAGE_REPORT.md` and `docs/testing/BLUEPRINT_TEST_COVERAGE.md`.
   Not-implemented-by-design surfaces (federation/SAML, DVR) stay out.
2. Every tier A/B entry must have a matching case in the owning suite; CI's coverage
   gate enforces it.
3. One file per service, owned by that service's suite — no cross-editing, so
   parallel work cannot conflict.
4. `known-failing` is recorded in the *suite* (via `t_known_failure "reason"`), not
   in the manifest — the manifest states the contract, results state reality.
