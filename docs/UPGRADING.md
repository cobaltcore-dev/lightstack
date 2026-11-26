# Upgrading Lightstack

One section per release, newest first. Each says what to do when upgrading to that release from
the one before it. A fresh install needs none of it.

## v0.7.0

### The migration squash

v0.7.0 restarts the database migrations. The migrations of earlier releases are replaced by one
baseline, `001`, for each backend. From v0.7.0 on, a release adds at most one migration, named
after the release (`002_v0.8.0`).

Nothing is upgraded in place. A database migrated by an earlier Lightstack is refused, untouched,
at start, by `lightstack-migrate up`, `down`, `goto` and `force`, and, on SQLite, by `check`:

```text
the database is at migration version 59, from before the migration squash. Recreate the database. See docs/UPGRADING.md, "The migration squash"
```

`lightstack-migrate status` prints `Status: PRE-SQUASH` with the same message and exits 1.

So install v0.7.0 on an empty database. Nothing in the old one carries over; back it up first only
if you may go back to the earlier release (`pg_dump` on PostgreSQL, `lightstack-migrate backup` on
SQLite).

In real mode, first remove what the earlier install created on each node: its guests, their
networks and its node-local storage. Do it while the old database still exists, because the
teardown reads it to find them. Run the earlier build's `cleanup` command with its configuration
(`lightstack cleanup -config …`), or uninstall its Helm release with `dataRetentionPolicy: delete`.

With Lightstack stopped, empty the database:

- **PostgreSQL**: drop the database and create it again, empty and with the same owner.
- **SQLite**: move the database file away, together with its `-wal` and `-shm` files. v0.7.0
  creates it at its first start.
- **Docker Compose** (`deployments/docker-compose.yml`): `docker compose -f deployments/docker-compose.yml down -v`
  removes the database volume.
- **The shared development and test database**: `make db-down && make db-up`.

Then start v0.7.0: `up -d` with Compose, or `helm upgrade` to the v0.7.0 chart and image on
Kubernetes. On bare metal, the checkout at `/opt/lightstack` that `scripts/deploy-single-node.sh`
creates, run as root:

```bash
cp /opt/lightstack/config/lightstack.yaml /root/lightstack.yaml.keep
git -C /opt/lightstack fetch origin --tags
git -C /opt/lightstack checkout -f v0.7.0
cp /root/lightstack.yaml.keep /opt/lightstack/config/lightstack.yaml
make -C /opt/lightstack build
systemctl start lightstack.service
```

The deploy script writes the node's configuration over the tracked `config/lightstack.yaml`, so
the copy keeps it across the forced checkout.

> **Never run an earlier Lightstack on a v0.7.0 database.** The old build reads the restarted
> numbering as its own. On PostgreSQL it applies its migrations from `002` over the baseline,
> inserts the default security group's two egress rules a second time and stops, dirty, at `003`.
> Going back to an earlier release therefore means recreating the database, or restoring a backup
> taken before the upgrade. The old build predates every check, so nothing stops it.
