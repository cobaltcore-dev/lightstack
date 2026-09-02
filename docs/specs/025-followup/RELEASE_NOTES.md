# Release notes: SPEC-025 follow-ups

Two changes in this feature are **visible to users with no user action behind them**. They should be
read together, because a project can meet both at once and the second explains the first.

There is no configuration change. One data migration, `073`, puts the volumes of pending transfers in
the state described under "A volume transfer holds its volume"; everything else is behavioural.

---

## Block-storage quota begins being enforced

**What changes.** Volume, snapshot and backup creation now consult the project's block-storage
quota. Until now the `cinder_quotas` limits were reported by every quota view and enforced by
nothing, so a project could exceed any limit it was given.

**Who notices.** A project at or over a configured limit starts receiving `413 overLimit` naming the
exhausted resource, where it previously succeeded. The defaults are generous — 100 volumes, 10 TB,
100 snapshots, 100 backups — so a deployment that never configured quotas is unlikely to notice.

**What is NOT affected.** Nothing is deleted and nothing is reclaimed. A project already past its
allowance keeps everything it has; only further additions are refused.

**Enforced on every route that adds to a project's consumption**, not only the volume API: a server
booted from a new volume, a backup restored into a new volume, an imported managed volume, an
accepted volume transfer, a volume extension, a snapshot, a backup. A transfer refused for quota
stays pending and its volume stays with the sender — nothing is lost.

---

## Reported `gigabytes` usage rises for any project holding snapshots

**What changes.** The `gigabytes` allowance now counts **volume and snapshot size together**,
matching upstream, whose configuration reference describes it as "total gigabytes for volumes and
snapshots". It previously counted volume size only, while the snapshot *count* was tracked
separately.

**Who notices.** Anyone with snapshots. The number appears in `openstack quota show`, the
block-storage absolute-limits view, and the dashboard's usage bars. A project with a 100 GB volume
and two 100 GB snapshots reported 100 GB used and now reports 300 GB.

**A project can therefore read as over its limit the day this ships, without having created
anything.** Combined with the change above, that means such a project is refused its next volume.

**This is the correct number** — it is what the storage actually holds, and a snapshot occupies
backend storage exactly as a volume does. Counting only volumes left an unbounded path past a limit
that claimed to bound storage: fill the allowance with volumes, then keep snapshotting.

**What operators may want to do first.** Before upgrading, compare reported usage against the
configured `gigabytes` limit for projects that hold snapshots, and raise limits where the corrected
figure would put a project over. `openstack quota show <project>` after the upgrade reports the new
figure; the limit itself is unchanged.

---

## A volume transfer holds its volume until it is accepted or cancelled

**What changes.** Creating a transfer puts the volume in upstream's `awaiting-transfer` status, and
accepting or cancelling the transfer returns it to `available` — for the recipient, or for the sender.
Accept moves the volume only out of that status, and makes the accepting user its user, as upstream.
The volume's snapshots go with it, as upstream's do by default: they hold the volume's data, and the
recipient's quota is charged for their count and size. A transfer is refused while any snapshot of
the volume is not `available`, and an accept fails — moving nothing — if one stopped being available
in the meantime.

**Why.** Nothing held the volume before. Create left it `available`, so a sender could create two
transfers, hand one over, and later take the volume back — with whatever the recipient had written —
by accepting the other. Two accepts of one transfer into different projects both succeeded, and the
last one kept the volume.

**Who notices.** Anyone who transfers volumes. `openstack volume show` reports `awaiting-transfer`
while a transfer is pending. A second transfer of the same volume is refused with `400`, and so is any
operation that would change it — delete, extend, retype, unmanage, the read-only flag, re-image,
attach, a snapshot, a backup, or a restore onto it — until the transfer is accepted or cancelled.
Reading the volume, renaming it and its metadata stay allowed.

**On upgrade.** Migration `073` marks the volumes of transfers already pending as `awaiting-transfer`,
so those transfers can still be accepted.

---

## Smaller behaviour changes, unlikely to be noticed

- **Server listings honour their filters.** `openstack server list --name X` now narrows. It
  previously returned the whole project, so a script reading the first row was reading an arbitrary
  server. Filters on an attribute this deployment does not record (`host`, `hostname`,
  `availability_zone`, `tags`) are now refused with `400` naming the filter rather than ignored.
- **`openstack image set` writes what it reports.** The image patch applies `add` as well as
  `replace`; it previously discarded `add` and answered `200` with the unchanged image, which is the
  operation the CLI sends for a property that has no value yet.
- **The service catalog names the host you addressed.** A client that can reach the deployment is
  handed endpoints it can resolve, rather than whatever `LIGHTSTACK_ENDPOINT_HOST` names.
- **`restore` answers as upstream does.** Deletion is immediate — upstream's default reclaim
  interval — so a deleted server is `404` and any other is `409`, as before. Only the `409` message
  changed, to upstream's wording.
- **Booting onto an unshared external network is for administrators only**, as upstream
  (`network:attach_external_network`). Anyone else is refused up front with `403` and upstream's
  message — the network's own project included, which could previously boot onto it — while an
  administrator may now use any external network. A shared external network takes anyone's server,
  as before. Attaching an interface to a running server follows the same rule: it now also accepts
  a shared network, which it used to refuse, and answers an unshared external one with the same
  `403`.
- **An out-of-range compute microversion is refused** with `406`, naming the supported range, rather
  than silently answered at the maximum.
