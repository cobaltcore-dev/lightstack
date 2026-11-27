# Upgrading Lightstack

## Upgrading an install created under the previous product name

Lightstack was renamed (`cobaltcore-dev/cloud-storage#575`). The rename moved the
default node-local state root (`pkg/paths/paths.go`), so an upgraded node writes to the
new root and stops seeing what the old one holds: image bases, volume backing files, per-instance disk overlays,
cloud-init ISOs and dnsmasq state.

Nothing has to be rewritten to fix this. No database column stores a path — every
location is derived from the resource id at runtime — so the data only has to **move
once**, per node, in real mode. Stub-mode and API-only deployments hold no node-local
state and need none of this.

The old data is not deleted by an upgrade; it is left where it is. If you upgrade first
and migrate afterwards, existing volumes and instances become visible again once the
state has moved.

Four things moved with the rename: the environment variables the binary reads (rename them
before upgrading), the service catalog (fixed once per database, automatically), each node's
local state (moved once per node) and — in real mode — that node's iptables chains.

### The service catalog — automatic

Nothing to do. Migration 052 seeded `endpoints.url` with the
product's Docker hostname, and the rename edited 052 in place, which reaches fresh
databases only: `golang-migrate` is version-tracked, so an install already past version
52 keeps its original rows. Migration **069** rewrites them on the next start.

Until it runs, the catalog advertises the old hostname and clients that discover services
through it (the OpenStack CLI, Terraform, Horizon) cannot reach anything but identity.
`LIGHTSTACK_ENDPOINT_HOST` does not help — the substitution in
`internal/keystone/auth.go` matches only the current host token.

069 also fixes endpoint rows created by hand through the identity API, and is a no-op on a
fresh database.

### Environment variables — rename them first

Every variable the binary reads was renamed with the product, and the old spellings are not
read. An old variable is not reported as an error either: it is simply absent under the new
name, so the mounted config file's value stands in its place. Rename them **before**
upgrading.

Two of those file defaults deserve attention, because nothing will draw it:

- `keystone.jwt_secret` in the reference config is the placeholder
  `change-me-in-production`. A deployment that supplied the secret only through the
  environment will still start — it logs one warning to stderr and then signs tokens with a
  published key. Tokens issued before the upgrade also stop validating.
- `glance.s3_insecure_skip_verify` is a setting an operator may have turned *off* from the
  environment against a config file that shipped it on. Losing that override re-enables
  skipped TLS verification.

`docs/CONFIGURATION.md` lists them all. Anything else — service modes, admin password,
ports, logging — only ever came from the config file, so it is unaffected.

### Stale firewall chains (real mode only)

The iptables chain prefixes changed with the rename. Chain names live in the kernel, not on
disk, so a node that has **not rebooted** since the upgrade still carries the pre-rename
chains — and the jumps that reference them.

That is not merely untidy. The per-fixed-IP north-south ingress chains are jumped from
`FORWARD` and end in `DROP`, and the reconciler *appends* its own jump, which therefore sits
behind the stale one. Until the old jump is gone, the stale chain decides what reaches a VM,
while every security-group ingress rule written since the upgrade lands in a chain traffic
never reaches — so a rule you remove to close a port appears to apply and does not.

Two equivalent fixes, per node:

```bash
sudo ./scripts/migrate-legacy-state.sh --apply    # removes the jumps, then the chains
```

or reboot the node — iptables rules are not persisted by default, and the reconciler rebuilds
the current chains on startup.

The dry run lists what it would remove. It only ever touches chains carrying the pre-rename
prefixes: the current chains, and anything else on the host, are left alone.

If any legacy rule or chain survives the cleanup, the run names it and exits non-zero rather
than reporting success — checked by re-reading the ruleset afterwards, since a command can
return 0 and still leave a rule behind. That matters because a surviving ingress chain sits
ahead of the current one in `FORWARD` and ends in `DROP`, so a host in that state is still
filtering on the pre-rename rules.

> Kubernetes nodes need this too. In real mode the pod uses host networking, so these chains
> are in the node's namespace, not the pod's — draining and rebooting the node works just as
> well as running the script on it.

### Bare metal / systemd

Run the migration on each node, with Lightstack stopped. It reports what it would do and
exits; `--apply` performs the move:

```bash
sudo systemctl stop lightstack.service
sudo ./scripts/migrate-legacy-state.sh            # dry run — shows the plan
sudo ./scripts/migrate-legacy-state.sh --apply
```

It merges file by file and never overwrites anything already under the new root, so an
interrupted migration can simply be run again — including one that left a half-copied
instance directory behind, which is the normal outcome of an interruption because the two
roots are separate mounts and every move is therefore a copy rather than a rename.

A path that exists under **both** roots is the one thing it will not decide for you: those
are listed and the run exits non-zero, so automation does not carry on believing the
migration finished. Everything else still moves first, so one conflict does not block the
rest. Compare the two copies, keep one, remove the other, and re-run.

`scripts/upgrade-lightstack.sh` calls it for you, with the service already stopped.

**Service-account ownership.** Moving state is a rename, so the files keep the UID and GID
of the account that wrote them, while the unit installed today runs as `lightstack`. The
migration hands the moved state to that account once the move is done — reading the account
from the installed unit's `User=`/`Group=` when there is one. Nothing in this repository
creates that account, so on a host where it does not exist yet the script prints the `chown`
to run later rather than failing a migration that otherwise succeeded.

One thing it deliberately does not reason about: qemu may run under its own account, so if a
guest fails to start after the migration, check that the overlay under
`/var/lib/lightstack/instances` is still reachable by it.

The migration moves **state only**. Install-layout paths — the checkout or package
directory, the config directory, the environment file, the binary, the systemd unit and
the service account — are reported but never touched: renaming a unit or a system
account mid-upgrade is not safely reversible from a script. The dry run prints the exact
commands for whichever of those paths still exist on that host; run them, then re-run
the upgrade.

> A checkout install under the old directory has to be renamed **before**
> `scripts/upgrade-lightstack.sh` will run at all — it looks for `/opt/lightstack`.

### Docker Compose

PostgreSQL honours `POSTGRES_DB` and `POSTGRES_USER` only while initialising an **empty**
data directory. A stack that keeps its `postgres` volume across the upgrade therefore
keeps the database and role it was first created with, and the healthcheck (`pg_isready`,
which does not authenticate) still reports healthy — so the symptom is the application
failing to connect, not the database looking broken.

Set the identity that volume already has, in `.env` next to the compose file or in the
environment:

```bash
POSTGRES_DB=<previous-name> POSTGRES_USER=<previous-name> \
  docker compose -f deployments/docker-compose.yml up -d
```

All four stacks that keep a volume read these (`deployments/docker-compose.yml`,
`deployments/docker/docker-compose.yaml`, `deployments/docker-compose-horizon.yml`,
`deployments/horizon/docker-compose.yaml`) and default to the current names, so a fresh
install needs nothing set. Verify with `docker compose -f <file> config` before starting.

#### Optional: rename the role and database instead

Only if you would rather not carry the old identity. A session cannot rename the role it
is connected as, so this needs a temporary superuser, and `ALTER ROLE … RENAME`
invalidates the stored password (it is salted with the role name) — so the password must
be set again afterwards. With the stack's Lightstack container stopped and only PostgreSQL
running:

```bash
psql "$OLD_DSN" -c "CREATE ROLE ls_rename_tmp SUPERUSER LOGIN PASSWORD 'tmp';"
psql "postgres://ls_rename_tmp:tmp@<host>:5432/postgres" \
  -c "ALTER ROLE <previous-name> RENAME TO lightstack;" \
  -c "ALTER DATABASE <previous-name> RENAME TO lightstack;" \
  -c "ALTER ROLE lightstack WITH PASSWORD 'secret';"
psql "postgres://lightstack:secret@<host>:5432/postgres" -c "DROP ROLE ls_rename_tmp;"
```

`ALTER DATABASE … RENAME` needs no session connected to that database, which is why the
temporary superuser connects to `postgres`. Afterwards the stack runs with no overrides.

### Kubernetes / Helm

Set `realMode.legacyStatePath` to the old root for one release, on a real-mode
deployment:

```bash
helm upgrade lightstack charts/lightstack \
  --set realMode.enabled=true \
  --set realMode.legacyStatePath=/var/lib/<previous-name>
```

That adds a `migrate-legacy-state` initContainer which runs before
`init-host-storage` on every node the pod lands on, moving that node's state into
`realMode.hostStoragePath`. It is idempotent and skips anything already present under
the new root.

Leave the value out (the default) once every node has been migrated — the initContainer
and its `hostPath` volume then disappear from the manifest. Fresh installs never need
it.

Both roots are mounted `DirectoryOrCreate`, so setting `legacyStatePath` on a node that
has no old state creates an empty directory and moves nothing.

#### Rename the key in a self-managed config Secret

Only if you supply the config yourself through `secrets.existingSecret` instead of letting
the chart render it. The key inside that Secret **is** the filename the container reads, and
it has to be `lightstack.yaml`.

The whole Secret is mounted as a directory at `/app/config`, so each key becomes a file
there, and the container starts with `--config /app/config/lightstack.yaml`. A Secret still
keyed under the previous product name therefore presents a file at a path the binary is not
looking at: the config load fails and the pod crash-loops. `helm upgrade` cannot repair it —
that Secret is yours, not the release's.

Rename the key before upgrading, preserving any other keys in the Secret:

```bash
SECRET='my-config-secret'      # the Secret you named in secrets.existingSecret
OLD_KEY='previous-name.yaml'   # the key it currently carries

VALUE="$(kubectl get secret "$SECRET" -o go-template="{{ index .data \"$OLD_KEY\" }}")"
kubectl patch secret "$SECRET" --type=json -p="[
  {\"op\": \"add\",    \"path\": \"/data/lightstack.yaml\", \"value\": \"$VALUE\"},
  {\"op\": \"remove\", \"path\": \"/data/$OLD_KEY\"}
]"
```

Then roll the Deployment or DaemonSet yourself. The chart cannot checksum a Secret it does
not render, so nothing restarts on its own.

## Redis cache keys are now scoped to the caller

`GET /v2.0/networks/{id}` and `GET /v2/images/{id}` read the cache **before** applying their
visibility predicate, so an entry keyed on the resource id alone was served to any
authenticated caller that knew the id. The keys now carry the caller's project:

| | before | after |
|---|---|---|
| Neutron network | `network:<id>` | `network:<project_id>:<id>` |
| Glance image | `image:<id>` | `image:<project_id>:<id>` |

This only matters where caching is enabled. It is off by default in both
`config/lightstack.yaml` and the chart, so a deployment with no Redis needs nothing here.

Entries written under the old keys are neither read nor evicted: the new lookup cannot match
them, and the invalidation patterns (`network:*:<id>`, `image:*:<id>`) do not either — the
old key has no project segment for the `*` to stand in for. Nothing serves them, so they
simply expire: 30 minutes for a network, one hour for an image. There is no stale-read
window and no action to take.

If you would rather not leave them sitting in Redis, drop them before starting the new
version — the new entries repopulate on the next read:

```bash
redis-cli --scan --pattern 'lightstack:network:*' | xargs -r redis-cli del
redis-cli --scan --pattern 'lightstack:image:*'   | xargs -r redis-cli del
```

Adjust the prefix if `cache.key_prefix` is not the default.

## Quota, limits, security-group, image and volume API changes

Several quota, limits and security-group answers changed, and so did server create and rebuild,
volume create from an image, how volume attachments read back, and which images Glance shows and
lists, most of them to match upstream OpenStack. A client that relied on an old answer now gets a
different status code or value. Nothing is migrated, and there is no switch to restore the old
behaviour. Three changes can need action on your side — eBPF mode, a self-maintained Horizon
configuration, and scripts that boot from an image id that names no image, or from an image with
no data, which used to be accepted — and the rest is listed per service below. Three rows
deliberately differ from upstream: `default` is Lightstack's own alias for the caller's project, an
unscoped token gets `404` on `/limits` where upstream answers `200`, and `os-attach` takes a
`mountpoint` of at most 50 characters where upstream takes 255.

### eBPF security groups fail closed (eBPF mode only)

With `networking_mode: ebpf`, four cases admitted more than the rules said:

- A rule whose only source is `remote_group_id` was translated to `0.0.0.0/0`, admitting every
  source. It now admits nothing — the filter is programmed once, at port creation, and cannot
  follow a group's members.
- An IPv6 rule without a prefix became IPv4 `0.0.0.0/0`, and one with an IPv6 prefix made the
  port's whole rule set fail to program. IPv6 rules are now left out of the filter, which parses
  IPv4 only.
- When programming a port's rules failed, the filter was not attached and the port stayed
  unfiltered. It is now attached either way.
- When attaching the filter failed, the port was still created, unfiltered. Port creation now
  fails with `500` instead, and nothing is left behind.

Traffic one of these cases admitted before the upgrade is dropped after it. Before upgrading an
eBPF deployment, replace rules whose only source is a remote group with `remote_ip_prefix` CIDRs.

This applies only to a build that loads the eBPF object. A stock build does not — the object path is
not wired through configuration — so there eBPF mode adds no east-west filtering, before or after
the upgrade; see [`EBPF_STATUS.md`](EBPF_STATUS.md).

### Horizon: `CSRF_COOKIE_HTTPONLY` must be `False`

Lightstack's Horizon `local_settings` set `CSRF_COOKIE_HTTPONLY = True`. Horizon's AngularJS reads
the CSRF token from `document.cookie`, so with the flag set some dashboard actions fail without
an error — Delete Key Pair does nothing. Both shipped copies,
`deployments/horizon-config/local_settings` and the chart's
`charts/horizon/files/local_settings`, now set `False`. If you deploy Horizon from your
own copy of that file, change the flag there too.

### Quota and limits

`{id}` is the project in the quota path: Nova `/os-quota-sets/{id}`, Cinder `/v3/quota-sets/{id}`,
Neutron `/v2.0/quotas/{id}`.

| | before | after |
|---|---|---|
| Cinder quota `PUT` / `DELETE` | any authenticated caller | admin only (`403` otherwise) |
| Nova quota `PUT` by a non-admin | `400` when the body was malformed | `403` before the body is read |
| `{id}` of `default`, or empty | Cinder: the project named `default`; Nova `PUT`: `500` | the caller's project, on every verb |
| a non-admin's `{id}` naming another project | that project's quotas | `403`. Nova `defaults` is still `200`, as upstream allows anyone to read it |
| an admin's `{id}` naming no project | `200` with default quotas | `404` |
| a malformed `{id}` | Nova: `500` | `404` |
| an unscoped token, on the quota routes and Nova/Cinder `/limits` | `500` (Neutron quotas: `200`) | `404` |
| a usage count that fails | `200`, reporting zero usage | `500` |
| Nova `/limits` for a project without quota rows | `maxTotalInstances` 100, `maxTotalCores` 200, `maxTotalRAMSize` 512000, `maxSecurityGroups` 50 | 10, 20, 51200 and 10 — the quota-set defaults |
| Nova `/limits` usage | `DELETED` instances left out; floating IPs, security groups and server groups always `0` | every instance row counted; the others counted |
| Cinder `/limits` | `maxTotalVolumes` 1000, `maxTotalSnapshots` 1000, `maxTotalBackupGigabytes` 5000; quota overrides ignored | 100, 100 and 10000; overrides applied |
| Cinder quota set with `?usage=true` | the parameter was ignored | `{in_use, limit, reserved}` per resource |

### Neutron

| | before | after |
|---|---|---|
| network and subnet `tenant_id` / `project_id` | the caller's project | the owning project |
| subnet listing | the caller's own and shared subnets | also subnets other projects created on the caller's networks |
| listings without `limit` (networks, subnets, ports, routers, floating IPs) | the first 1000 rows, with no `next` link | every row; an explicit `limit` still pages |
| network list/show when the subnet lookup fails | a truncated `subnets` list | `500` |
| unset `remote_ip_prefix` / `remote_group_id` in `GET /v2.0/security-groups/{id}` | `""` | `null` |
| creating a rule in another project's group | allowed | `404` |
| reading or deleting another project's rule | allowed | `404` |
| deleting an unknown rule id | `204` | `404` |
| `remote_group_id` | stored unchecked; a malformed id gave `500` | `400` when malformed, `404` when not the caller's group |
| deleting a network that still has subnets | `409` | `204`; the subnets are deleted with it, as upstream |
| deleting a network with floating IPs allocated from it, or that is a router's external gateway | `409` if it had subnets, else `204`, deleting its floating IPs with it | `409` (`NetworkInUse`) |
| deleting the auto-allocated topology while a port is on its network | `204`, deleting the port with it | `409` (`NetworkInUse`) |
| `gateway_ip` on subnet create | stored unchecked | `400` unless an address of the subnet's IP version inside it, other than its network and broadcast addresses |
| `gateway_ip` on subnet update | ignored | applied, checked as on create; `409` (`GatewayIpInUse`) while a router interface holds the current gateway |
| updating, deleting, or removing an interface from another project's router | the gateway's egress SNAT installed before a `404` (update), the namespace deleted before a `204` (delete), the interface detached (remove) | `404`, and nothing changes |
| clearing a router's gateway with `external_gateway_info: {}` or `null` | `400`, or ignored | cleared, and the egress SNAT it gave withdrawn |
| a router's egress SNAT once its gateway is cleared, `enable_snat` is turned off, or an interface is removed | left in place until teardown | withdrawn, unless another router still needs the same range |
| `enable_snat` that is not a boolean | stored | `400` |
| deleting a router that still has interfaces | `409` (`Conflict`) | `409` (`RouterInUse`) |
| `remove_router_interface` with neither `subnet_id` nor `port_id` | `500` | `400` |
| `marker` on the network, subnet and port listings | read whatever its visibility; one that named nothing was ignored | used only with a `limit`, as upstream, and only if the caller may read it; otherwise `404` (`NetworkNotFound`, `SubnetNotFound`, `PortNotFound`) |

### Nova

| | before | after |
|---|---|---|
| `addSecurityGroup` with a group the server already has | `409` | `400`, as upstream, where Neutron rejects the duplicate id |
| server create, `security_groups[].name` | a group name | a group name or id |
| server create with a root `source_type: image`, `destination_type: volume` mapping — the dashboard's *Create New Volume* | boots the image directly; `volume_size` and `delete_on_termination` ignored, no volume created | creates an unnamed volume from the image and boots from it, as upstream; `delete_on_termination` decides whether it is deleted with the server |
| … where no volume can be created | the image boot above | `400` when there is no block-storage service, `503` when creating the volume fails |
| … with an unknown image, a malformed `volume_size`, or one below the image's floor | the image boot above | `400` |
| server create with an image the caller cannot read (another project's private image), or an id that names no image | accepted: the image booted, or the server went to `ERROR` (stub mode: a server that never booted anything) | `400 Can not find requested image`, before anything is created. Scripts that booted from a made-up image id must create a real image first |
| rebuild with such an image | accepted | `400 Cannot find image for rebuild` |
| server create or rebuild with an image that is not active (no data uploaded yet, or deactivated) | accepted | `400 Image <id> is not active.` |
| server show and detail | no `os-extended-volumes:volumes_attached` | `[{id, delete_on_termination}]` for every attached volume, the boot volume included; `[]` when there is none |

### Cinder

| | before | after |
|---|---|---|
| `volume create --image` on a `local` or `local,rbd` backend, for an image whose base is not on the node yet (an RGW/S3-backed image no server has booted there) | `503 failed to create volume from image` | the base is fetched and checked against the image checksum first, as an image boot does |
| `volume create --image` with an image the caller cannot read, or an id that names no image | accepted (stub mode); `503` (local backends) | `400 Invalid image identifier or unable to access requested image.` |
| `os-reimage` with such an image | `202` | `404 Image <id> could not be found.` |
| `volume create --image` or `os-reimage` with an image that is not active | accepted | `400 Invalid input received: Image <id> is not active.` |
| a volume's `attachments` | `{server_id, device}`, the device always `/dev/vdb` | upstream's `{id, attachment_id, volume_id, server_id, host_name, device, attached_at}` with the recorded device (`/dev/vda` for a boot volume); `id` is the volume's id, as upstream |
| `os-attach` without `mountpoint`, or naming a server that is not one of the caller's | accepted | `400`. A `mountpoint` longer than 50 characters is also a `400` (upstream allows 255) |
| `os-attach` on a volume that is not `available` | accepted | `400`; a `mountpoint` the server already uses is a `409` |
| `os-attach`, `os-detach`, `os-force_detach` on a volume that does not exist | `202` | `404` |

### Glance

Reads follow upstream's visibility rules, so sharing an image now has an effect.

| | before | after |
|---|---|---|
| image show and download | public images and the caller's own | also community images, images with no owner, images shared with the caller (any membership status), and every image for an administrator |
| image list, default | public images and the caller's own | the caller's own, public ones, and shared ones whose membership is accepted; an administrator lists every image. Either way other projects' community images are left out unless `?visibility=community` or `all` asks for them |
| `?visibility=`, `?member_status=`, `?status=` on the list | ignored | honoured as upstream; an invalid `visibility` or `member_status` is a `400` |
| `marker` on the list | read whatever its visibility; one that named nothing was ignored | must name an image the caller may read, as upstream; otherwise `400` |

## The console origin is now read — and must be an origin

`nova.console_proxy_base_url` used to be ignored: the binary had no such setting, although four
deployment guides told operators to set it, with example values ending in `/vnc_auto.html`. It is
now honoured (SPEC-021), and it takes an **origin only** — `scheme://host[:port]`. Lightstack
appends the client page and the token query itself, so a path would produce a console link that
404s.

A value with anything beyond the origin — a path, query or fragment — now **stops startup**, with
an error that names the corrected value:

```text
Failed to load config: nova.console_proxy_base_url "http://203.0.113.10:6080/vnc_auto.html" must be an origin only; Lightstack appends the client page and token itself (unlike upstream Nova's novncproxy_base_url, it names no page): use "http://203.0.113.10:6080"
```

Drop the path. Three sources carry one:

- the old guides' example values;
- a config written by `scripts/deploy-turnkey.sh` or `scripts/deploy-single-node.sh` before this
  release — both wrote `http://<host>:6080/vnc_auto.html`. Edit the value. A console on those
  installs also needs the updated scripts' `novnc` service: the old one could not resolve a token
  at all;
- a value carried over from upstream Nova, whose `novncproxy_base_url` **does** name the page.

With Helm, `nova.consoleProxyBaseUrl` is pattern-checked, so the same value fails `helm install` /
`helm upgrade` instead of starting a pod that exits. An empty value is still valid: the origin is
then derived from the request.

## Guests created before this release still expose their console

A guest's VNC console is now a UNIX socket at the instance's own path,
`<state_path>/console-sockets/<server-id>.sock`, with no TCP listener. The token-checking console
proxy is the one way in, and a console can never be handed to another guest (SPEC-021). The
socket directory holds nothing else, so the proxy never sees libvirt's per-domain directories,
which also hold each guest's QMP monitor socket. That holds for guests **defined** after the
upgrade. The console is part of the libvirt domain definition, written once when the instance
is created, and nothing Lightstack does afterwards — reboot, stop/start, an upgrade — rewrites it.
Real mode only: stub mode defines no domains.

A guest created before this release therefore has one of two consoles:

- **A TCP console on `0.0.0.0`.** Anything that can reach the node reaches it, with no token and
  no password, as it always could. The log says `guest console is not a unix socket; refusing to
  proxy`.
- **A path-less `<listen type='socket'/>`** (test builds of this release). Its socket is in
  libvirt's per-domain directory, where the proxy cannot see it. The log says
  `guest console socket is outside the console socket directory; refusing to proxy`.

Lightstack does not paper over either. A console request for such a guest fails with
`500 console misconfigured on this host`.

List the affected domains on each node, meaning every domain whose definition does not name the
console socket directory (adjust the path if `state_path` is not the default):

```sh
for d in $(sudo virsh list --all --name); do sudo virsh dumpxml --inactive "$d" | grep -q "socket='/var/lib/lightstack/console-sockets/" || echo "$d"; done
```

Then, per domain, either delete and recreate the instance, or fix the definition in place:

1. `sudo virsh edit <domain>` — replace the whole `<graphics type='vnc' …>` element with
   `<graphics type='vnc' sharePolicy='force-shared'><listen type='socket' socket='/var/lib/lightstack/console-sockets/<server-id>.sock'/></graphics>`,
   where `<server-id>` is `sudo virsh domuuid <domain>` and the root is your `state_path`. Leave
   out any `port`, `autoport` or `listen` attribute. `sharePolicy='force-shared'` keeps a second
   viewer from disconnecting the first.
2. Restart qemu: the edit takes effect only when the domain starts again.
   `openstack server reboot --hard <server>` does that — it power-cycles the domain
   (`sudo virsh domuuid <domain>` prints the server's id). A soft reboot restarts the guest inside
   the same qemu process and does not pick the edit up. Stopping and starting works too, but
   `openstack server stop` only asks the guest to shut down, so wait for
   `sudo virsh domstate <domain>` to report `shut off` before `openstack server start`.

## Console sockets moved out of libvirt's directories

The guest console socket now lives in `<state_path>/console-sockets/`, owned by QEMU's user with
mode 0750. Lightstack prepares the directory at startup in real mode, before any guest can start.
Four consequences:

- **`state_path` is at most 50 characters in real mode.** A longer root would push a guest's
  socket path past QEMU's 108-byte limit, so no guest could start. Startup refuses it instead, and
  the error names the limit. Stub mode is not limited.
- **`consoleProxy.libvirtDomainDir` is gone from the chart.** The proxy no longer mounts libvirt's
  per-domain directories. A values file that still sets it fails `helm install` / `helm upgrade`.
  Delete the key.
- **Every real-mode deployment mounts `console-sockets` into Lightstack**, the KVM compose stack
  included, even though it runs no proxy. Without the mount the host's QEMU cannot create the
  socket and no guest starts. If Lightstack cannot make the directory QEMU's, it keeps serving and
  refuses guest starts with an error naming the directory.
- **Migration `065` changed in place** (`console_auth_tokens.domain_id`). Recreate a test database
  that ran an earlier `065`. Nothing outside test hosts ran it.

## `realMode.hostStoragePath` now takes effect

The chart always accepted this value, but the binary ignored it: its node-local root was a
compile-time constant, `/var/lib/lightstack`. A custom value therefore mounted a host directory
Lightstack never wrote to. Its state went into the container's own filesystem instead, guests did
not boot (libvirtd on the host looks for their disks where the domain XML says), and console
tokens never reached the proxy.

The root is now the `state_path` setting in `lightstack.yaml` — absolute, default
`/var/lib/lightstack` — and the chart renders it from `realMode.hostStoragePath`. On the default
there is nothing to do. An install that set a custom value never worked in real mode and now uses
that directory as intended; anything created while the value was ignored — instances left in
`ERROR`, images whose data was lost with a container restart — has to be recreated.

Outside Helm, set `state_path` directly. The directory must be the same path on the host and to
Lightstack, since libvirtd opens the paths Lightstack writes into the domain XML.

Both deploy scripts had the same defect. `scripts/deploy-turnkey.sh`'s `LIGHTSTACK_STORAGE_DIR`
moved three bind mounts and wrote per-service `*_storage_path` keys the binary never read; it now
writes `state_path` and mounts the whole root at the same path — which also brings `cloud-init` and
`dhcp` onto the host, where they were missing even on the default root. `scripts/deploy-single-node.sh`
asked for three per-service paths that went into the same unread keys; it now asks for one state
root. Re-running either script regenerates its files.

## The EC2 metadata `instance-id` is now EC2-shaped

`http://169.254.169.254/<version>/meta-data/instance-id` used to answer with the bare instance
UUID. It now answers `i-` and the UUID's first 17 hex digits (`i-755a18e9eb3940f68` for
`755a18e9-eb39-40f6-8208-267410268522`), the shape guests expect: cirros's EC2 datasource
retried 20 times, two seconds apart, before settling for the UUID, so every cirros boot waited
about 40 seconds. The OpenStack tree is unchanged; `meta_data.json` still carries the full UUID
as `uuid`.

A running guest that reads the EC2 id sees it change once. On its next boot, cloud-init's EC2
datasource compares the new id with the one it cached at first boot and treats the guest as a new
instance: it runs first-boot setup again, which regenerates SSH host keys (clients see a
host-key-changed warning) and runs user-data scripts a second time. Guests that use the OpenStack
datasource are not affected. There is no switch to keep the old id, so plan the next reboot of
such a guest accordingly.

## The metadata service identifies guests by their address only

A caller used to be able to choose which instance it was, in two ways: an `X-Instance-ID` header
naming the instance, or an `X-Forwarded-For` header naming one of its fixed IPs. Guests reach the
service through a DNAT that passes headers through untouched, so any guest, and anything else that
could reach port 8775, could read any instance's metadata and user-data. The caller is now its TCP
peer address: `X-Forwarded-For` is ignored, and `X-Instance-ID` is believed only from loopback.

Nothing changes for guests. A test setup that names instances with `X-Instance-ID` from somewhere
other than loopback, such as Docker Compose with port mapping, where requests arrive from the bridge
gateway, sets `metadata.trust_instance_header: true` or `LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER=true`.
`deployments/docker-compose.yml` does, since it runs in stub mode. Never set it where real guests
reach the metadata service.

## Placement is listed in the service catalog

The binary always served placement on `:8778`, but the catalog never listed it, so clients that look
it up there, such as Horizon's Hypervisors → *Resource Provider* tab, reported it as unavailable.
Migration 071 adds it on the next start; nothing to do. Where the catalog is cached in Redis
(`cache.enabled`), a cached copy lives up to 24 hours, so placement appears once it expires.

On a single node placement schedules nothing: Lightstack's placement is a stub that reports no
resource providers, so Horizon's tab shows an empty table rather than an error.

## SQLite as a second database backend

Lightstack can now run a single node on one embedded SQLite file instead of a PostgreSQL server
(`database.url: sqlite:///absolute/path/lightstack.db`; [CONFIGURATION.md](CONFIGURATION.md#sqlite)).
An existing PostgreSQL deployment keeps its database and changes nothing; there is no conversion
between the two. What does change for every deployment:

### Migrations

- **`make migrate` runs `lightstack-migrate`.** golang-migrate's `migrate` CLI no longer reads the
  folder: SQLite's files share version numbers with PostgreSQL's, so reading the folder through iofs
  it refuses the collision (`duplicate migration file: 073_…`). `make install-tools` no longer
  installs it. Scripts that called `migrate -path migrations -database … up` call
  `lightstack-migrate up -config <lightstack.yaml> -migrations migrations` instead — it always reads
  a config file (default `config/lightstack.yaml`, and it exits 1 when that is missing) even when
  `LIGHTSTACK_DB_URL` supplies the URL; `status`, `down`, `reset`, `goto` and `force` work as before,
  on either backend.
- **Migration 074** adds a nullable `reserved_by` column to `volumes` and `snapshots`. It runs on the
  next start, like every migration.
- PostgreSQL can now migrate **down** through 001. The down files of `010`, `019`, `028` and `037`
  dropped tables and columns an earlier migration owned and stopped the way down; they are
  corrected. Nothing changes going up.

### API answers

| | before | after |
|---|---|---|
| server create over the instances, cores or RAM quota | `413 overLimit`, `Quota exceeded for resource: instances` | `403 forbidden`, `Quota exceeded for instances: Requested 1, but already used 10 of 10 instances`, as upstream |
| server create whose new boot volume exceeds the block-storage quota | `413` naming the resource | `403`, `Quota exceeded for gigabytes: Requested …, but already used … of … gigabytes` |
| two server creates racing for a project's last instance | both could be accepted | exactly one is accepted, the other gets the `403` |
| `DELETE` of a volume still `creating`, or `os-extend` of one | not reachable — volumes were recorded `available` after their backing existed, never `creating` | `400` with upstream's message (the reservation now makes `creating` a real, reachable state) |
| `DELETE` of a snapshot still `creating` | accepted (there was no status check); its backing already existed | `400` with upstream's message |
| snapshot create | `202 creating`; `available` about 100 ms later | `202 creating`, unchanged; `available` already when the answer arrives |

Block storage's own endpoints keep answering `413` for their quotas, as upstream's do. Horizon shows
the compute quota refusal as an error message either way.

### Volumes interrupted by a restart

A volume or snapshot create now reserves its row (`creating`) before the backing store is touched
and finalises it afterwards. A node that stops mid-create sets its own interrupted reservations to
`error` when it runs again: at its next start, before it serves requests, or — if it restarted
within about a minute of stopping, while the stopped process still looked alive — by a periodic
pass about a minute after the start. They keep whatever backing they had: deleting the `error`
volume or snapshot removes it, as upstream Cinder does, and is the clean-up. The node finds its own
by name — `LIGHTSTACK_NODE_NAME`, or
the host name when that is unset. The Helm chart sets `LIGHTSTACK_NODE_NAME` to the pod's node
name, so a recreated pod finds them too. Elsewhere, where the host name changes across restarts (a
container without host networking), set it yourself, or interrupted reservations stay `creating`
until an administrator resets them with `os-reset_status`, as upstream operators do for a resource
stuck in a transitional state: `POST /v3/{project}/volumes/{id}/action` for a volume and
`POST /v3/{project}/snapshots/{id}/action` for a snapshot (the snapshot action is admin-only and
new in this release — it is the only way to free a snapshot, and the volume holding it, when the
reserving node never returns under the same name). Rows from before 074 carry no name and are never
touched.

### Helm chart: the purge reads the database from `database.existingSecret`

With the database URL in `database.existingSecret`, the purge Job of `dataRetentionPolicy: delete`
now receives it as `LIGHTSTACK_DB_URL`, as the Lightstack pods do. It used to run without a URL, so
it could not read the database and purged only what it found on the node: the release's VMs and its
host networking stayed behind. Now it removes them, as it does for a release with `database.url`.
Without the URL, `lightstack cleanup` — which now refuses an empty one — would fail the hook and
stop the uninstall.

## Real mode: the libvirt socket, readiness and cluster DNS

Five things an existing real-mode release meets:

- **A `libvirt_uri` Lightstack cannot honour stops startup.** Lightstack only ever dials the libvirt
  on its own node: `qemu:///system` (the default) or `qemu+unix:///system`, either with
  `?socket=<absolute path>`, or an absolute socket path. Anything else — `qemu:///session`,
  `qemu+tcp://`, `qemu+ssh://`, a relative path — used to be dialed as a file name and failed as a
  missing socket. The server, `lightstack cleanup` and `lightstack console-tokens` now refuse it at
  configuration load, naming the value and the accepted forms. With the default, Lightstack chooses
  the socket on every connection attempt: `virtqemud-sock` where the modular daemons run, else
  `libvirt-sock` ([CONFIGURATION](CONFIGURATION.md)).
- **A pod that cannot prepare the console socket directory is no longer Ready.** Before, it was
  Ready and refused every guest start. Lightstack now connects to libvirt in the background and
  prepares the directory after every connection; until the first preparation succeeds, `/readyz`
  answers `503 hypervisor unavailable`, and on a single node the Service has no endpoint. A host
  where libvirt never answers is restarted by the startup probe (30 × 5 s by default) and ends in
  `CrashLoopBackOff`, with the cause at the top of each start's log, so `helm install --wait` fails.
  Once prepared, later libvirt outages do not affect readiness. Docker Compose and bare metal are
  not gated: their healthchecks probe `GET /v3`.
- **Host-networked pods resolve through the cluster DNS.** With `realMode.hostNetwork: true` the
  chart now renders `dnsPolicy: ClusterFirstWithHostNet` on the Lightstack pod and the purge Job,
  so in-cluster Service names (`<service>.<namespace>.svc`) resolve. A name only the node's own
  resolver answers — an `/etc/hosts` entry, a zone only the node's resolver serves — must now be
  reachable through the cluster DNS, for example through a CoreDNS forward or stub domain.
- **The host workarounds can go.** A `libvirt-sock` symlink to `virtqemud-sock`, enabling
  `virtproxyd.socket` so that `libvirt-sock` exists, and warming libvirt before Lightstack starts
  (`virsh capabilities`) are no longer needed: Lightstack finds `virtqemud-sock` itself, bounds each
  attempt at 1 second, and retries the preparation until libvirt answers.
- **The uninstall purge waits one second for libvirt, once.** With `dataRetentionPolicy: delete`,
  `lightstack cleanup` used to wait on libvirt's handshake without a bound. It now makes one attempt
  bounded at 1 second and fails closed. A socket-activated daemon that had exited is started by that
  attempt and may miss it; `helm uninstall` then aborts. Run it again: the daemon is up by then.

## A server create must name its boot source

A server create that names neither `imageRef` nor `block_device_mapping_v2` is now refused at once
with `400` "Missing imageRef attribute", in stub and real mode, as upstream Nova refuses it. It used
to be accepted with `202`, and the server then ended `ERROR` with the fault "the image reference is
not a valid image ID". Lightstack reads `block_device_mapping_v2` only, so a request that boots from
the legacy `block_device_mapping` alone was never honoured either, and is now refused the same way.

## A shelve libvirt cannot perform ends `ERROR`

Suspend, resume and shelve answer `202` and only then ask libvirt. A failed suspend or resume already
turned the server `ERROR`; a failed shelve was dropped, and the server read `SHELVED` while its guest
ran on. It now ends `ERROR` too, and all three log the cause (`shelve failed in libvirt`, and so on).

## Security-group membership reaches the ports

A server's security groups now live on its ports, the list the firewall is built from. Adding or
removing a group through the compute API changes the ports, and with them what reaches the server.
It used to change a separate record that the firewall never read, so a group added to a running
server opened nothing and a group removed closed nothing.

| | before | after |
|---|---|---|
| `addSecurityGroup` / `removeSecurityGroup` | changed a record of the server's; the ports and the firewall stayed as they were | change every port of the server, and its chains on its node, before the answer |
| the group in either action | by name only | by name or by id, as server create, resolved in the server's project |
| either action on a server with no port | `202` | `404`, "instance_id `<id>` could not be found as device id on any ports", as upstream |
| removing a server's last group | `400 cannot remove last security group from instance` | `202`, as upstream: its ports carry no group and admit replies only |
| a server's `security_groups` | its own record, with `default` invented when it held none | its ports' groups, one entry per port and group; no key when they carry none. Servers created by v0.6.1 or earlier report none, since their ports carry none |
| an interface attached with `net_id` | a port with no group | a port in the project's `default` group, created if missing; the server's other groups are not copied, as upstream |
| `PUT /v2.0/ports/{id}` with `security_groups` | ignored, or `400 no updates provided` when it was the only field | replaces the port's groups and converges its chains before the answer |
| `POST /v2.0/ports` with `security_groups: []` or `null` | the port got `default` | the port gets no group; absent still gives `default` |
| `GET /v2.0/security-groups?id=…&name=…` | every group of the project | the groups matching `id` (repeatable) and `name` |
| deleting a group a port carries | `204`, and the group left every port | `409 SecurityGroupInUse`, as upstream |
| deleting a group that does not exist or is another project's | `204` | `404` |
| deleting a port or a server whose port has a floating IP | the floating IP kept its fixed IP, router and `ACTIVE`, and its NAT stayed | the floating IP loses its port, fixed IP and router and goes `DOWN` in the same step, and the network node stops forwarding it before the answer |
| the `forceDelete` server action | deleted the instance row and its domain; the server's ports and volume attachments stayed | deletes the server as `DELETE` does: its boot ports go, ports attached by id are detached, its volumes are released |
| any real-mode change — a port's groups, a rule, a floating IP, a router, a network, a subnet, a port, a port forwarding — that the node holding its state cannot apply, or that waits more than one second | answered success | `500`, and the change is undone; a deletion keeps its record |
| a server's `security_groups` naming a group of another project that an administrator put on its port by id | named to the tenant | named only to an administrator, as upstream, which skips a group its caller cannot list |
| a server deletion whose boot port is a port forwarding's internal port | the forwarding went, its floating IP kept its router and `ACTIVE` | the floating IP is freed with it, as a port's deletion frees it: no router, `DOWN` |
| a deletion that loses a race with another deletion of the same server | `404`, and the tenant's detached port was bound back to its old node | `404`; the port stays as the winning deletion left it |

Each server keeps the groups it has. A server created by v0.6.1 or earlier, whose ports carry none,
now reports none: that is what its firewall was enforcing. Add its groups again with
`openstack server add security group` to have them enforced.

### The Networking API

| | before | after |
|---|---|---|
| `GET /v2.0/security-groups` with `description`, `project_id`, `tenant_id`, `stateful`, `revision_number`, `shared` or the tag filters | ignored: every group of the project | honoured, as upstream; an administrator's `project_id` names any project; a boolean filter that is not one answers `400` |
| a group's representation | no `stateful`, `revision_number`, `shared` or `tags` | all four: `stateful` as created, `revision_number` from `0`, one more on every update and on every rule added or deleted; `shared: false`, `tags: []` |
| creating a group with `"stateful": false` | ignored | a stateless group: its ports' traffic is not connection-tracked. A port carrying stateful and stateless groups at once answers `409 SecurityGroupConflict`, and changing `stateful` on a group a port carries `409 SecurityGroupInUse` |
| `PUT` or `DELETE` of a group with `If-Match: revision_number=<n>` | ignored | `412 RevisionNumberConstraintFailed` when the revision differs; `400 Bad if-match request` for two `revision_number=` tags or a number that does not parse. Other entity tags, `*` among them, are still ignored |
| a new group's rules | none | two egress rules, IPv4 and IPv6, as upstream |
| creating a rule upstream refuses: a `direction`, `ethertype` or `protocol` it does not know; ports without a protocol, on one without ports, or out of range; a `remote_ip_prefix` that is no address or network, or of the other family; a remote prefix and a remote group together | `201`, stored as sent | `400` under upstream's name (`SecurityGroupRuleInvalidProtocol`, `InvalidCIDR`, `SecurityGroupRuleParameterConflict`, `SecurityGroupProtocolRequiredWithPorts`, …), storing nothing |
| a rule whose `security_group_id` is not a UUID | `404` | `400`, as upstream |
| a boolean query filter — a group listing's `shared` and `stateful`, a network listing's `shared` and `router:external`, a subnet listing's `enable_dhcp`, Cinder's `?usage=` | ignored by the group listing; the network and subnet listings and Cinder took Go's `strconv.ParseBool` words (`1`/`0`, `t`/`f`, `true`/`false`, some capitalisations) and ignored any other | upstream's words, in any case: `yes`/`no`, `y`/`n` and `on`/`off` too |
| a rule's `remote_ip_prefix`, `protocol`, `ethertype` and ports | stored as sent | stored as upstream stores them: an address as its host prefix (`192.0.2.10` → `192.0.2.10/32`), a netmask as a prefix length, a protocol name lowercased and `any` as none, an IPv6 rule's `icmp` as `ipv6-icmp`, ports `1`–`65535` as none, `ipv4` as `IPv4` |
| a `default` group created from now on, and the seeded project's | egress only | egress, and ingress from its own members, IPv4 and IPv6; still nothing open to every address. Every other existing group keeps its rules: see below |
| listing groups or rules, or creating a group, in a project that has no `default` yet (no port so far) | no `default` listed | the project's `default` is created first, as upstream: `?name=default` finds it |
| `GET /v2.0/security-group-rules` with `security_group_id`, `direction`, `protocol` and the other attribute filters | ignored: every rule you can see, for an administrator every project's | honoured, each repeatable: `openstack security group rule list <group>` lists that group's rules |
| deleting an unused `default` as a non-administrator | `204` | `409 SecurityGroupCannotRemoveDefault` |
| renaming `default` | `200` | `409 SecurityGroupCannotUpdateDefault`; its description still changes |
| creating a group named `default` (any case), or renaming one to it | `500` for `default` exactly, which the unique name refused a second time; another case, `Default` say, accepted | `409 SecurityGroupDefaultAlreadyExists` |
| a group name used twice in a project | `500` | `201`: names repeat, as upstream |
| a name several of your groups share, in server create, `addSecurityGroup` or `removeSecurityGroup` | — | `409 NoUniqueMatch`; `GET /v2.0/security-groups/{name}` answers `404`, list with `?name=` instead |
| floating-IP creates sent at the same time | could answer `500` | each `201`, with an address of its own |
| a floating IP on a port no router joins to its network, a port on a subnet without a gateway included | create `201`, update `400` | `404 ExternalGatewayForFloatingIPNotFound`, storing nothing |
| a second floating IP of one external network on a fixed IP | accepted | `409 FloatingIPPortAlreadyAssociated` |
| a floating IP on a network with no IPv4 subnet | an address of `192.0.2.0/24` | `400` |
| a floating IP's address | from `.100` up in the network's first subnet | the lowest free address of the pools of the network's IPv4 subnets; `400 ExternalIpAddressExhausted` when they are taken |
| a floating IP's `fixed_ip_address` that is not the port's; a taken `floating_ip_address` | accepted | `400`; `409 IpAddressAlreadyAllocated`. `subnet_id` and `floating_ip_address` are honoured |
| a floating IP's `floating_ip_address` chosen by a caller holding neither `admin` nor `manager` (`openstack floating ip create --floating-ip-address`) | honoured | `403 PolicyNotAuthorized`, as upstream: the address is an administrator's or the project manager's to choose. Lightstack seeds no `manager` role; create it and assign it on a project to give its holders the choice |
| a subnet's `allocation_pools` | ignored on create and update; reported as computed | stored, validated as upstream (`400 InvalidAllocationPool`, `400 OutOfBoundsAllocationPool`, `409 OverlappingAllocationPools`, `409 GatewayConflictWithAllocationPools`) and reported. Ports, DHCP ports, router gateways and floating IPs take their addresses from them. Existing subnets get the default pool, so an external network's floating range is set through its pools |
| a subnet's `dns_nameservers` on create | stored as sent | each an IP address of the subnet's IP version, an IPv4 one without leading zeros, none twice, at most 5, as upstream; `400` otherwise, storing nothing. An update still ignores them |
| a requested fixed IP that is its subnet's network or broadcast address | a port ignored any requested address; an interface attach took it | `400 InvalidIpForSubnet`, on a port and an interface attach |
| `POST /v2.0/ports` with `fixed_ips` | ignored | honoured, as upstream |
| deleting a subnet a port or a floating IP has an address on | `204`, the port or floating IP keeping a dangling address | `409 SubnetInUse`; the network's DHCP port does not count |
| a router interface on a subnet the router already has, or on one without a gateway | `500` | `400`, storing nothing |
| a router interface on another project's subnet, a shared network's included, as a non-administrator; on the subnet of the router's gateway; or on a subnet overlapping one of its interfaces' | `200` | `400`, as upstream, storing nothing |
| a port, a router gateway's `external_fixed_ips` or a floating IP requesting its subnet's gateway address, held by nothing | ignored: none of the three allocated a requested address | allocated, as upstream's IPAM does; `409 IpAddressAlreadyAllocated` once something holds it. The pools still leave the gateway out |
| a router interface on a subnet whose gateway address a port holds, another router's interface say | `200`, two ports holding one address | `409 IpAddressAlreadyAllocated` |
| changing the `gateway_ip` of a subnet a router's gateway is on | the routers kept the previous default route | every such router's default route moves before the answer; when one cannot take it, `500` and the update is undone |
| a router's gateway | `external_gateway_info` as the request gave it; no address allocated | a port with `device_owner` `network:router_gateway` holding an address from the pools, or the one requested, reported in `external_gateway_info.external_fixed_ips`; requesting one is administrator-only (`403`), as upstream |
| `enable_snat` in a router's `external_gateway_info`, from a non-administrator, on create or update (`openstack router create`/`set` with `--enable-snat` or `--disable-snat`) | stored and applied, `false` sending the router's subnets out un-NATed | `403 PolicyNotAuthorized`, whatever the value, as upstream: an administrator's to set. A gateway without it masquerades, as before |
| a port forwarding on a floating IP no port uses | `400` ("floating IP must be associated with a router") | `201`, as upstream: the router is found through the internal port and recorded on the floating IP, which the last forwarding frees |
| a port forwarding on a floating IP a port uses; to an internal port with a floating IP of its own | `201` | `409 FloatingIPPortAlreadyAssociated`; `409 PortHasBindingFloatingIP`, as upstream. Free the floating IP first: `openstack floating ip unset --port` |
| giving a floating IP its port forwardings use a port | accepted | `409 FipInUseByPortForwarding`, as upstream |
| removing an interface, or clearing or changing a gateway, that a floating IP or one of its port forwardings goes through | accepted | `409 RouterInterfaceInUseByFloatingIP`; `409 RouterExternalGatewayInUseByFloatingIp` |
| deleting a router that a member's own port names as its device, with owner `network:router_gateway` | the member's port deleted with the router | the member's port stays: only the router's own gateway port goes |
| deleting a router's interface port with `DELETE /v2.0/ports/{id}`; an administrator deleting its gateway port | `204`, the router losing the interface and its floating IPs their path | `409 ServicePortInUse`, administrators included, as upstream. Remove them through the router: `openstack router remove subnet`, `openstack router unset --external-gateway` |
| ports | — | report `binding:host_id` to administrators; each network with DHCP has a port with `device_owner` `network:dhcp`, as upstream |
| another project's port naming a server in `device_id` | changed by the server's group actions, or vetoing them; listed in its groups; detached or deleted with it | left alone, as the server's addresses already left it: a server's ports are its project's |
| a `device_owner` starting `network:` on a port of another project's network, set by a non-administrator on create or update | stored | `403 PolicyNotAuthorized`, as upstream: only an administrator or the network's project gives a port a network's role |
| another project's ports on a shared network, for a non-administrator | listed and shown, with their addresses and servers | not visible, as upstream: a port is its project's and its network owner's to read; a marker naming one answers `404` |
| an administrator's Networking API listings | the administrator's own project's resources | every project's, narrowed by `project_id`/`tenant_id`, as upstream; a show by id reaches any project's resource |
| `project_id`/`tenant_id` in a create of a port, security group, subnet, router, floating IP, QoS policy, RBAC policy, trunk, metering label, subnet pool or address scope | ignored: the caller's project owned it | an administrator's sets the owner, as upstream; another project named by a member, the two spellings disagreeing, or a project that does not exist answer `400` |
| an administrator's update, delete or action on another project's resource by id | `404` | allowed, as upstream; a create may reference another project's resource by id. Names still resolve in your own project only |
| the message of a `409` `NetworkInUse`, `RouterInUse`, `GatewayIpInUse` or `SecurityGroupInUse` | `Cannot <operation>`, the reason in `detail` | upstream's: `Unable to complete operation on network <id>. There are one or more ports still in use on the network, id for these ports is: <ids>.`, `Router <id> has ports still attached: <ports>`, `Current gateway ip <ip> already in use by port <port>. Unable to update.`, `Security Group <id> in use.`; `detail` empty. A network's floating IPs and port-less gateways are named the same way |
| a port's create response | written from the request: no `project_id`, no `binding:host_id` for an administrator, `status` always `ACTIVE`, the timestamps the handler's | the port as its show reports it, read back after the create |
| a subnet's create and update responses | create: written from the request, with the handler's timestamps; update: no `created_at` or `updated_at` | the subnet as its show reports it, read back after the change |
| `addSecurityGroup`/`removeSecurityGroup` with a `name` of blanks only | `404` | `400`, as upstream ("Security group name cannot be empty"); a missing `name`, `400` "Security group not specified" |
| `POST /v2.0/subnets` with an IPv6 CIDR and `ip_version: 4`, or `ip_version: 5` | `201`: stored as given, an IPv6 range as an IPv4 DHCP subnet that crashed the network node's reconciliation | `400`, as upstream's `_validate_ip_version`; an absent `ip_version` is the CIDR's |
| one address requested twice in a port's `fixed_ips` or a gateway's `external_fixed_ips` | `201`/`200`, the address stored twice | `400` "Duplicate IP address"; two entries naming only a subnet get two addresses |
| a gateway update with `external_fixed_ips: []`, or naming only the subnet it has an address on | `200`, the address moved | `200`, the address kept, as upstream |
| a port forwarding's update to another internal port | checked the port's project and address only | checked as a create: `409 PortHasBindingFloatingIP`, `400` for a port the floating IP's router does not reach; a create to such a port `400` too |
| `PUT /v2.0/networks/{id}` unsetting `router:external` while a router's gateway is on it | `200` | `409 ExternalNetworkInUse`, as upstream |
| a trunk or a subport on another project's port, or a malformed subport id | `201`/`200`; the owner's own trunk on it then `500` | `404`; a malformed id `400` |
| a port update that answers `500` (its firewall change could not be applied) | its groups went back, its other fields stayed | all of it goes back, compare-and-set |
| `device_owner` changed into or out of `network:*` on a bound port | the port's chains changed at the next reconciliation pass | its node is converged before the answer, or `500` and undone |
| `POST /v2.0/ports` with a `network:*` `device_owner` | the port got `default`, or the groups given | no group, as upstream: a trusted port carries none |
| a rule with `remote_group_id: ""` | `404` (SQLite) or `500` (PostgreSQL) | `201`, no remote group, as upstream |
| the rule listing's `ethertype`, `protocol`, `remote_ip_prefix`, `port_range_min`/`_max` filters | compared as sent: `ipv4`, `TCP` or a bare address listed nothing | converted as upstream: any case, a prefix as netaddr prints it; a port that is not one `400`; `protocol=any` lists rules without one |
| a router interface on a subnet whose gateway a floating IP holds | `200`: the address held twice | `409 IpAddressAlreadyAllocated` |
| `add_router_interface` with `port_id` | `404 subnet` | adds the port as the interface, as upstream; `409 PortInUse`, `400` for no fixed IP, a second IPv4 subnet, a subnet the router has, or an IPv6 address on a network where the router has an IPv6 port already; a subnet and a port together `400`. Every fixed IP of the port is on the router's leg, and a removal by one of its subnets takes only that subnet off the port |
| an IPv6 subnet added by subnet to a router with an IPv6 interface port on its network | a second interface port | the subnet's gateway joins that port, as upstream |
| a floating IP's `fixed_ip_address` without a `port_id`; an update's non-string `fixed_ip_address` or `description` | ignored | `400`, as upstream |
| an allocation pool address with an IPv6 zone (`%eth0`) | stored and handed out | `400 InvalidAllocationPool` |
| the auto-allocated topology's network where the node coordinates VXLAN | `flat`, MTU 1500 | `vxlan`, MTU 1450, as a network create makes |
| deleting a rule while its group is deleted (PostgreSQL) | one of the two `500`, deadlocked | both complete: the rule's delete takes the group's lock first |

#### Existing security groups and the port firewall

Every port is now firewalled on its node, and its chains end in a drop: egress and traffic between VMs are
filtered where the earlier release filtered neither. Existing groups keep the rules they have, so two kinds
of group stop admitting traffic they let through before:

- A group created through the API before this release has no rule at all — the earlier release gave a new
  group none, where this one gives it two egress rules, as upstream. A port carrying only such groups cannot
  open a connection. Give each of them the two egress rules, with `GROUP` set to its name or id:

  ```sh
  openstack security group rule create --egress --ethertype IPv4 "$GROUP"
  openstack security group rule create --egress --ethertype IPv6 "$GROUP"
  ```

- A project's `default` created before this release, other than the seeded project's, has egress only: no
  rule admits its own members, so the VMs in it no longer reach each other. Give each such `default` the
  member rules this release gives a new one, with `PROJECT` set to its project:

  ```sh
  openstack security group rule create --ingress --ethertype IPv4 --remote-group default --project "$PROJECT" default
  openstack security group rule create --ingress --ethertype IPv6 --remote-group default --project "$PROJECT" default
  ```

`openstack security group rule list --egress "$GROUP"` shows a group's egress rules, and
`openstack security group rule list --ingress --project "$PROJECT" default` a `default`'s ingress.

### The real-mode layout: a clean break

Real mode moves to upstream's layout for a network node:

- One node, the **network node**, holds every router, each in a namespace `qrouter-<router id>`, and
  every network's DHCP and metadata, each in a namespace `qdhcp-<network id>`. Floating IPs, port
  forwardings and the routers' SNAT live in the routers' namespaces; the network node answers ARP for
  the routers' and floating addresses on its uplink, and no other node holds them. With VXLAN on,
  `neutron.network_node` (the chart's `neutron.networkNode`) names the network node and Lightstack
  refuses to start without it; without VXLAN the node is its own.
- Every port is firewalled on the node its guest runs on, both ways, as upstream's iptables driver:
  chains `ls-i`, `ls-o` and `ls-s` per port, built from its groups, remote groups resolved to their
  members' addresses. This needs the `br_netfilter` module and
  `net.bridge.bridge-nf-call-iptables` and `-ip6tables` at `1`: Lightstack sets them at start, and its
  readiness fails, naming `bridge-netfilter`, while they do not hold. The router namespaces' NAT rules
  need iptables' `comment` match (`xt_comment` with the legacy backend), the ports' chains its
  `physdev` match (`xt_physdev`).
- A guest's TAP whose port is gone — deleted while the guest runs — has its frames dropped both ways before
  any port's chains see them, those addressed to the node itself included, by two fixed chains, `ls-bound-in` and
  `ls-bound-out`, in both families; teardown removes them. Such a TAP used to reach every bound port on its bridge,
  checked only by the receiver's ingress.
- A rule with protocol `vrrp` (112) no longer fails the node's whole firewall commit: protocols are written by
  name exactly where iptables prints one, otherwise by number, and `gre`, `108`, `135` and ICMP type `255` alone
  are no longer rewritten on every pass.
- Requests waiting together on a node's firewall, a router or a network's DHCP share one convergence: many guests
  booting at once on one node no longer run out of the one-second wait and end in `ERROR`.
- The node-to-node call goes straight to the node's tunnel address, never through an `HTTP_PROXY` the
  environment names. `neutron.network_node` is read trimmed, and a node that is not the network node removes
  the routers and DHCP it holds only while the named node is active.
- `lightstack cleanup` also removes the earlier layout's two `FORWARD` accepts per recorded subnet, and its
  removal of an older deployment's project namespaces is bounded at one second, a failure failing the purge.
- Guests reach metadata in their network's namespace, named by network and address. The host's own
  listener (`metadata.bind_address`) names no caller by its address any more; only `X-Instance-ID`, from
  loopback or with `metadata.trust_instance_header`, names an instance there.
- No bridge holds an address, no dnsmasq runs on the host, and the host carries no floating-IP NAT,
  floating addresses or filter chains of its own.

Nothing converts a host running the earlier layout: each node is reset, and what it held is created
again. A port has firewall chains only once it is bound to a node, which happens when a server boots
or an interface is attached, so servers are recreated, not kept.

`lightstack cleanup` resets a node, and it removes more than the guests and the host networking:
everything Lightstack keeps on the node goes with them — images, volumes, instance disks, cloud-init
and DHCP files, the console directories — and, with a SQLite `database.url`, the database file. A
PostgreSQL database is left as it is. Before running it:

1. Back up what you keep. A SQLite database: `lightstack-migrate backup`, whose default target,
   `<state_path>/backups/`, the reset leaves alone. PostgreSQL: `pg_dump`. Image and volume data has
   no backup command; copy what you need off the node first.
2. On PostgreSQL, delete through the earlier release's API the servers, volumes and images you will
   recreate, so that no record names data the reset removes, or start this release on an empty
   database.
3. See what the reset would remove: `lightstack cleanup --dry-run` with the node's `--config`.

Then run `lightstack cleanup` on each node (the chart's `helm uninstall` runs it as its purge hook,
one Job on one node the workload's node selection allows, when the release sets
`dataRetentionPolicy: delete`; with the default `retain`, and on every other node, run it yourself),
and remove the earlier layout's router namespaces, named `qrouter-` and the first
11 characters of the router's id: the cleanup and the reconciliation recognise this release's
`qrouter-<router id>` only.

```sh
ip netns list | awk '/^qrouter-/ && length($1) == 19 {print $1}' | xargs -r -n1 ip netns del
```

Start this release, and create the images, volumes and servers again. `lightstack cleanup` removed
what a record names of the earlier layout — the unmarked NAT and `/32` uplink addresses of floating
IPs that exist, and the egress `MASQUERADE` of existing subnets — and, as this release does once on its
own when it starts, what a mark or a name makes Lightstack's: marked floating-IP NAT, the `LS-NSI-*`
and `LS-SG-*` chains, bridge addresses, the per-bridge metadata DNAT, host dnsmasq and Neutron's
`tap-*` devices. Neither touches an unmarked rule no record names, which it cannot tell from an
operator's own: those of floating IPs and subnets deleted before the upgrade stay. List them on each
node with

```sh
iptables -t nat -S PREROUTING | grep -- '-j DNAT' | grep -v lightstack-fip
iptables -t nat -S OUTPUT | grep -- '-j DNAT' | grep -v lightstack-fip
iptables -t nat -S POSTROUTING | grep -- '-j MASQUERADE'
```

and remove the ones that are not yours.

Floating addresses must reach the network node's uplink: on a LAN the node's ARP answers do it; where
an underlay delivers only the addresses assigned to a node, allow the floating range for the network
node there.

### Kubernetes and Compose

- `realMode.privileged` is gone: real mode always runs the pod, and the purge Job, privileged, since Kubernetes
  allows the bidirectional `/run/netns` mount to privileged containers only. Remove the key from your values;
  the schema refuses it (`additional properties 'privileged' not allowed`).
  The pod, and the teardown purge Job, mount the host's `/run/netns` with bidirectional propagation,
  so the routers' and networks' DHCP namespaces are the host's and outlive the pod. The pod also mounts
  `/lib/modules` read-only for `modprobe br_netfilter`; the purge Job does not.
- A multi-node release with `multiNode.vxlanEnabled: false` no longer renders: without VXLAN every node was its
  own network node and every change touching another node answered `500`. The chart's `neutron.networkNode`
  and VXLAN checks are skipped under `secrets.existingSecret`, whose configuration the chart never renders. On
  PostgreSQL, real mode no longer creates an empty `<hostStoragePath>/db`; on SQLite it creates the directory
  `database.url` names.
- The turnkey Compose (`scripts/deploy-turnkey.sh`) mounts the host's `/run/netns` and `/lib/modules` as the
  KVM file does, and creates `/run/netns` first: its routers' and networks' DHCP namespaces outlive the
  container.
- The KVM Compose file mounts `/run/netns:/run/netns:rshared` and `/lib/modules:/lib/modules:ro`. The
  host's `/run` must be a shared mount, as systemd makes it. Podman, unlike Docker, does not create a
  missing bind source: create `/run/netns` on the host first.
- Multi-node: name the network node. In the chart, `neutron.networkNode`, its Kubernetes node name;
  with VXLAN on, the chart refuses to render without it. In Lightstack's own configuration,
  `neutron.network_node`, its `LIGHTSTACK_NODE_NAME`, else its host name.
