# Upgrading Lightstack

## Upgrading an install created under the previous product name

Lightstack was renamed (`cobaltcore-dev/cloud-storage#575`). The rename moved the
node-local state root, and that root is a compile-time constant in the binary
(`pkg/paths/paths.go`), so an upgraded node writes to the new root and stops seeing what
the old one holds: image bases, volume backing files, per-instance disk overlays,
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

`docs/CONFIGURATION.md` lists all twelve. Anything else — service modes, admin password,
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
