# Quickstart: verifying SPEC-031

**Feature**: `031-realmode-dns-libvirt-fixes` (cloud-storage#630) | **Date**: 2026-11-06

An outline of how the result is checked: the gates, then the host validation of FR-021. The step-by-step host runbook (paste-safe, with teardown) is generated separately into `HOST_TESTING.md`.

## 0. Gates

```bash
make build && make lint
make db-up
LIGHTSTACK_TEST_DB_URL='postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable' \
  go test ./pkg/hypervisor/... ./internal/nova/... ./internal/health/... ./internal/teardown/... ./cmd/lightstack/...
LIGHTSTACK_TEST_DB_URL='sqlite:///tmp/ls-test/t.db' \
  go test ./internal/nova/... ./cmd/lightstack/...
helm unittest charts/lightstack
# contract suite: against a running stub Lightstack, once on PostgreSQL and once on SQLite
make test-contract
```

Expected:
- the URI table, the socket-choice tests, the background-connection and preparation-retry tests, the per-launch ownership tests, the readiness check, the startup refusals, the cleanup fail-fast tests and the chart suites pass on both backends;
- the existing suites, the contract suite included, stay green (SC-008).

The no-hypervisor contract case runs separately, against the §2 server: `LIGHTSTACK_CONTRACT_NO_HYPERVISOR=1 go test ./test/contract/nova/ -run NoHypervisor`.

## 1. Render checks (no cluster needed)

```bash
helm template t charts/lightstack --set image.repository=x --set secrets.jwtSecret=j --set secrets.adminPassword=p \
  --set database.url=postgres://u@db.ns.svc/l --set realMode.enabled=true --set realMode.hostNetwork=true \
  | grep -n 'dnsPolicy\|hostNetwork'
```

Expected: `hostNetwork: true` followed by `dnsPolicy: ClusterFirstWithHostNet` on the Deployment.

Add `--set dataRetentionPolicy=delete` to see the purge Job carry the same pair. Add `--set realMode.libvirtSocketPath=/run/libvirt/virtqemud-sock` and look at the config Secret's `libvirt_uri` (C-SOCK-1). Without it, `libvirt_uri: "qemu:///system"` (C-SOCK-2).

## 2. Binary checks (no libvirt needed)

- Real mode with `nova.libvirt_uri: "qemu+tcp://h/system"` → the server exits 1 and `lightstack cleanup` exits 2, each naming the value and the accepted forms (C-URI-2). With `libvirt_mode: stub` the same config starts.
- Real mode with libvirt absent → the server starts, logs failed connection attempts naming both sockets, and `/readyz` answers `503 hypervisor unavailable` (C-READY-3).
- A `server create` against it ends `ERROR` with a `fault` (C-GUEST-1). The message names the step that failed first: an image boot on a host without the image base fails at disk staging, a boot from an existing volume reaches the hypervisor ("the hypervisor is not ready to start guests"). This is the server the gated contract case (§0) runs against.

## 3. Host validation (FR-021, research R12)

Single-node k3s on the openSUSE host: `virtqemud` socket-activated, no `libvirtd`, no `virtproxyd`, no symlink. Chart in real mode with `realMode.privileged` and `realMode.hostNetwork`.

| Step | Do | Expect |
|---|---|---|
| (a) DNS | `database.url` naming the PostgreSQL Service as `<svc>.<ns>.svc`; `dataRetentionPolicy: delete` | the pod becomes Ready; `helm uninstall` completes (the purge Job resolves the name) |
| (b) modular host, defaults | install with default `realMode.libvirtSocketPath`; create a server | the log names `/var/run/libvirt/virtqemud-sock`; the server is `ACTIVE` with a guest in `virsh list`; the delete uninstall destroys it |
| (c) cold path | before installing: `systemctl stop virtqemud`, empty `/var/cache/libvirt/qemu/capabilities/` | the log shows the failed first capabilities read, then the success line (directory, uid:gid); Ready; the first server boots `ACTIVE`; no restart. A fast host that answers within 1 s anyway is recorded as "retry not exercised" |
| (d) unreachable path | stop the `virtqemud` socket units (their `RemoveOnStop=yes` removes the socket), then install | not Ready, `503 hypervisor unavailable`; a create on the node's own address (`localhost:8774`) ends `ERROR`. Start the socket units again: the log shows the connection and the socket; Ready; a server boots `ACTIVE`; no restart |
| (e) monolithic | stop the modular socket units, start `libvirtd.socket`, reinstall | the log names `/var/run/libvirt/libvirt-sock`; a server boots `ACTIVE` |

Afterwards, restore the host's daemon setup and remove any leftover workaround: the symlink, `virtproxyd.socket`, the warm-up.
