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

A guest's VNC console is now a UNIX socket that libvirt creates per start in the domain's private
directory, with no TCP listener, so the token-checking console proxy is the one way in and a freed
console can never be handed to another guest (SPEC-021). That holds for guests **defined** after
the upgrade. The console is part of the libvirt domain definition, written once when the instance
is created, and nothing Lightstack does afterwards — reboot, stop/start, an upgrade — rewrites it.
Real mode only: stub mode defines no domains.

A guest created before this release therefore still has a TCP console on `0.0.0.0`: anything that
can reach the node reaches it, with no token and no password, as it always could. Lightstack does
not paper over it — a console request for such a guest fails with
`500 console misconfigured on this host`, and the log says
`guest console is not a unix socket; refusing to proxy`.

List the affected domains on each node:

```sh
for d in $(sudo virsh list --all --name); do sudo virsh dumpxml --inactive "$d" | grep -q "<listen type='address'" && echo "$d"; done
```

Then, per domain, either delete and recreate the instance, or fix the definition in place:

1. `sudo virsh edit <domain>` — replace the whole `<graphics type='vnc' …>` element with
   `<graphics type='vnc' sharePolicy='force-shared'><listen type='socket'/></graphics>`: no
   `port`, `autoport` or `listen` attribute, and no path of your own — libvirt picks one per
   start. `sharePolicy='force-shared'` keeps a second viewer from disconnecting the first.
2. Restart qemu: the edit takes effect only when the domain starts again.
   `openstack server reboot --hard <server>` does that — it power-cycles the domain
   (`sudo virsh domuuid <domain>` prints the server's id). A soft reboot restarts the guest inside
   the same qemu process and does not pick the edit up. Stopping and starting works too, but
   `openstack server stop` only asks the guest to shut down, so wait for
   `sudo virsh domstate <domain>` to report `shut off` before `openstack server start`.

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
