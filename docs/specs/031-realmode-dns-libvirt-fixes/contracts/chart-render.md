# Contract: Chart render — cluster DNS and the libvirt socket

**Feature**: `031-realmode-dns-libvirt-fixes`

The chart's interface is what `helm template` produces and which values the schema refuses. Each clause is a helm-unittest assertion. "Real values" means `realMode.enabled: true` plus the image, secrets and a PostgreSQL `database.url`, as the existing suites set them.

## C-DNS-1 — Host-networked pods use the cluster DNS (FR-001)

Given real values with `realMode.hostNetwork: true`:

| Template | Path | Value |
|---|---|---|
| `templates/deployment.yaml` | `spec.template.spec.dnsPolicy` | `ClusterFirstWithHostNet` |
| `templates/daemonset.yaml` (`deploymentMode: multi-node`) | `spec.template.spec.dnsPolicy` | `ClusterFirstWithHostNet` |
| `templates/teardown-purge-job.yaml` (`dataRetentionPolicy: delete`) | `spec.template.spec.dnsPolicy` | `ClusterFirstWithHostNet` |

`hostNetwork: true` is rendered beside it in each, as today.

## C-DNS-2 — No policy without host networking (FR-002)

- Stub mode, with or without `realMode.hostNetwork: true`: no `dnsPolicy` on any of the three.
- Real values with `realMode.hostNetwork: false`: no `dnsPolicy` on any of the three. The pod spec still says `hostNetwork: false`, as today.
- The console proxy keeps its own `dnsPolicy: ClusterFirstWithHostNet` (unchanged, `console_proxy_test.yaml`).

## C-SOCK-1 — A non-default socket reaches the binary (FR-004)

Given real values with `realMode.libvirtSocketPath: /run/libvirt/virtqemud-sock`:

- `templates/secret.yaml` → `stringData["lightstack.yaml"]` matches `libvirt_uri: "/run/libvirt/virtqemud-sock"`.
- The Deployment and the purge Job mount `/run/libvirt` as the `libvirt-run` hostPath (`type: Directory`), as today (`libvirt_socket_mount_test.yaml`).
- Both read that one config Secret, so no second assertion per workload is needed beyond the existing `config` volume.

## C-SOCK-2 — The default render is unchanged (FR-004)

Given real values without `realMode.libvirtSocketPath`, or with it set to `/var/run/libvirt/libvirt-sock`: `stringData["lightstack.yaml"]` matches `libvirt_uri: "qemu:///system"`, and the mount is `/var/run/libvirt`.

## C-SOCK-3 — The schema refuses a non-absolute socket (FR-006)

`realMode.libvirtSocketPath: libvirt-sock` and `realMode.libvirtSocketPath: ""` both fail schema validation, as a relative `realMode.hostStoragePath` already does (`"pattern": "^/.+"`).

## C-DOC-1 — What the chart's own docs say (FR-003a, FR-005, FR-023)

Not helm-unittest; reviewed in the diff:

| Where | Says |
|---|---|
| `values.yaml` `realMode.hostNetwork` comment; `values.schema.json` description; README row | host networking renders `dnsPolicy: ClusterFirstWithHostNet`, so the Lightstack pod and the purge Job resolve in-cluster Service names |
| `values.yaml` `realMode.libvirtSocketPath` comment; schema description; README row | a non-default value is also the socket dialed; the default lets the binary choose `virtqemud-sock`, else `libvirt-sock`; with `secrets.existingSecret` the operator's own config must name the same socket |
| README health table; `CONTRACT.md` §2; `NOTES.txt`; `values.yaml` `probes` comments; schema probe descriptions | in real mode, `/readyz` also waits until the console socket directory has been prepared once; a host where that never succeeds is restarted by the startup probe |
