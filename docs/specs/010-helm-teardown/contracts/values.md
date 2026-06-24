# Contract: teardown configuration values

**Feature**: SPEC-010 | **Surface**: `charts/o3k/values.yaml` + `values.schema.json` | **Refs**: FR-002/005/016/018, R1

The operator-facing config contract this feature adds/finalizes. Backward compatible: defaults reproduce SPEC-008 behavior exactly.

## `values.yaml` (additions / finalized docs)

```yaml
# -- Uninstall behavior. retain (default) = remove only the release's Kubernetes
#    workloads, leave ALL persisted/runtime state (SPEC-008 SC-006, unchanged).
#    delete = run the blocking pre-delete teardown: quiesce, purge libvirt domains +
#    network namespaces/routes + node-local hostPath storage + release PVCs, then
#    RESET the configured database schema. IRREVERSIBLE. Single-node only today;
#    multi-node host purge is tracked in issue #52. (FR-002/013/014; issue #53)
dataRetentionPolicy: retain   # retain | delete

# -- Teardown hook settings. Effective ONLY when dataRetentionPolicy: delete.
teardown:
  # -- REQUIRED when delete. kubectl image for the quiesce/PVC-delete hook Job
  #    (scale Deployment to 0, delete release PVCs). No implicit pull — install/
  #    uninstall fails fast if empty under delete. (R1, FR-018)
  kubectlImage: ""            # e.g. registry.example.com/kubectl:1.31

  # -- Blocking-hook bound: purge Job activeDeadlineSeconds. (FR-016)
  timeoutSeconds: 600

  # -- Delete release-owned PersistentVolumeClaims during teardown. (FR-005)
  #    PVCs are deleted only if present (the chart uses hostPath today — C3).
  deletePVCs: true
```

## `values.schema.json` (additions)

```json
"dataRetentionPolicy": { "type": "string", "enum": ["retain", "delete"] },
"teardown": {
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "kubectlImage":   { "type": "string" },
    "timeoutSeconds": { "type": "integer", "minimum": 1 },
    "deletePVCs":     { "type": "boolean" }
  }
}
```

## Conditional-required gate (template `fail`, not schema)

```text
if dataRetentionPolicy == "delete" and teardown.kubectlImage == "":
    fail: "dataRetentionPolicy=delete requires teardown.kubectlImage (the chart pulls no implicit image; see issue #53 / FR-018)."
```

(JSON Schema can't express "required only when another field equals X"; this mirrors the chart's existing image/secret fail-fast gates.)

## Compatibility & documentation (FR-018)

- **Default unchanged**: `dataRetentionPolicy: retain` + the `teardown` block defaulted ⇒ no teardown resources render ⇒ SPEC-008 behavior bit-for-bit (SC-004).
- **README reconciliation**: the "clean node-local data manually when `delete`" line is **superseded** (now auto-purged); the values table gains the `teardown.*` rows; the Uninstall section documents the irreversibility, the DB reset, what stays operator-owned (external DB beyond this release's schema is not multi-tenant; networked RBD/S3 not purged — C4), and the `--no-hooks` escape hatch.
