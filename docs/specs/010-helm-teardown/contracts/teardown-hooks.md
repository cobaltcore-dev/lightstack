# Contract: rendered teardown hook resources

**Feature**: SPEC-010 | **Surface**: Helm chart (`charts/lightstack/templates/teardown-*.yaml`) | **Refs**: FR-001/005/007/011/016, R1/R4/R7

Defines what the chart MUST render so the teardown is blocking, ordered, least-privileged, and gated. **All resources render iff `dataRetentionPolicy == "delete"`.** Under `retain`, none render (SC-004).

## Common annotations/labels (every resource)

```yaml
metadata:
  name: {{ include "lightstack.fullname" . }}-teardown[-<role>]   # release-scoped (FR-007)
  labels: {{- include "lightstack.labels" . | nindent 4 }}
  annotations:
    "helm.sh/hook": pre-delete
    "helm.sh/hook-weight": "<weight>"
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
```

## Resources & ordering (R4)

### RBAC (weight `-20`)
- **ServiceAccount** `…-teardown`.
- **Role** `…-teardown` — least privilege (R7), exactly:
  - `apiGroups: ["apps"]  resources: ["deployments"]  verbs: ["get","list","patch"]`
  - `apiGroups: [""]      resources: ["pods"]         verbs: ["get","list"]`
  - `apiGroups: [""]      resources: ["persistentvolumeclaims"] verbs: ["get","list","delete"]`
- **RoleBinding** `…-teardown` — binds the SA to the Role. Namespaced (no ClusterRole).

### Quiesce + PVC Job (weight `-10`)
- Image: `{{ .Values.teardown.kubectlImage }}` (template **fail** if empty — FR-018).
- ServiceAccount: `…-teardown`. `backoffLimit: 0`, `restartPolicy: Never`.
- Steps (single-node): `kubectl scale deploy <fullname> --replicas=0` → `kubectl wait --for=delete pod -l app.kubernetes.io/instance=<release>` → if `teardown.deletePVCs`: `kubectl delete pvc -l app.kubernetes.io/instance=<release> --ignore-not-found`.
- Multi-node (DaemonSet): **not rendered** here — deferred to #52 (FR-014).

### Purge Job (weight `0`) — the blocking gate
- Image: `{{ include "lightstack.image" . }}` (the Lightstack image), command `["lightstack","cleanup"]`.
- Pod spec mirrors a **real-mode** instance: privileged + host libvirt socket + `hostStoragePath` mount (+ `hostNetwork` per real-mode values) so it can reach host state; mounts the config Secret at `--config`.
- `activeDeadlineSeconds: {{ .Values.teardown.timeoutSeconds }}` (FR-016), `backoffLimit: 0`, `restartPolicy: Never`.
- No K8s RBAC needed (talks to host + DB, not the API — R1).

## Behavioral guarantees the rendering must satisfy

| Guarantee | How |
|-----------|-----|
| **Blocking** (FR-001) | pre-delete hooks must all succeed before Helm deletes the release; the weight-0 purge Job is one of them. |
| **Bounded** (FR-016) | purge Job `activeDeadlineSeconds`; `backoffLimit: 0` (no silent retries). |
| **Fail-closed** | purge Job non-zero exit ⇒ hook fails ⇒ uninstall aborts. |
| **Ordered** | RBAC (-20) → quiesce+PVC (-10) → purge (0). |
| **Gated** | absent entirely under `retain`. |
| **Release-scoped** (FR-007) | every name `lightstack.fullname`-prefixed; kubectl selectors use `app.kubernetes.io/instance=<release>`. |
| **Escape hatch** (C5) | `helm uninstall --no-hooks` skips all of the above (documented in NOTES/README). |

## helm-unittest assertions (the test contract — R8)

- `dataRetentionPolicy: delete` → RBAC + both Jobs present with the weights/annotations above; `retain` → none present.
- Role rules equal exactly the three least-privilege entries (no extra verbs/resources).
- Purge Job has `command: [lightstack, cleanup]`, `activeDeadlineSeconds`, `backoffLimit: 0`, and the real-mode host mounts.
- `kubectlImage: ""` under `delete` → render fails with a clear message.
