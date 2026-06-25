# Contract: Block Storage (Cinder) Real Mode (WS-5)

External interface = OpenStack Block Storage (Cinder) v3 + Nova volume attachments and boot-from-volume, via `openstack` CLI / gophercloud / Terraform. Attach is made **synchronous** (fixing the current async goroutine).

## Behavioral contracts

| ID | Given | When | Then |
|----|-------|------|------|
| C-WS5-1 | real mode | `openstack volume create --size N v1` | real backing storage is provisioned (node-local default at `/var/lib/o3k/volumes`, or RBD); volume → `available`. |
| C-WS5-2 | a running VM and `available` volume | `openstack server add volume vm1 v1` | the call returns only after libvirt `attach-device` succeeds (synchronous, fail-fast); the guest sees a new block device; DB and hypervisor agree. `server remove volume` detaches and returns the volume to `available`. |
| C-WS5-3 | a volume attached, written, detached | re-attach (same or other VM) | previously written data is present (persisted on the node hostPath). |
| C-WS5-4 | an image | `openstack server create --volume ...` / `--block-device ... boot_index=0` | a bootable volume is created from the image and the VM boots from it; `delete_on_termination` governs volume cleanup on server delete. |
| C-WS5-5 | `openstack volume create --image IMG --size N v1` | — | `v1` is `bootable=true`, `source_image_id=IMG`; on RBD it is a CoW clone that **pins** `IMG` (image delete → 409, ties to WS-0 C-WS0-3). |
| C-WS5-TF | Terraform `openstack_blockstorage_volume_v3` + attach (+ boot-from-volume) | `terraform apply` | behaves as upstream, unmodified. |

## Contract tests (RED-first)

- `test/contract/ws5_volume_test.go` (gophercloud): C-WS5-1/2/3 — create/attach/detach/delete; assert synchronous attach (no "in-use" before the device is really attached) and status transitions.
- `test/ws5_attach_real_test.sh` (host): C-WS5-2/3 — `virsh domblklist` shows the attached disk; write/read a sentinel across detach/re-attach.
- `test/ws5_boot_from_volume_test.sh`: C-WS5-4 — boot-from-volume; `virsh domblklist` root is the volume; verify `delete_on_termination`.
- `test/ws5_volume_from_image_test.sh`: C-WS5-5 — `source_image_id` set; image delete blocked while the volume exists.
- `test/terraform/ws5/`: C-WS5-TF.

## Notes
- Attach correctness: the existing flow updates the DB then calls libvirt in a background goroutine that never reverts on failure (`internal/nova/volume_attachment.go:126-167`). This contract requires the synchronous + fail-fast behavior (research R11; Constitution II).
- Local volume path moves to `/var/lib/o3k/volumes` (research R6) so volumes persist across pod restarts.
- `volumes.source_image_id` is the new column (migration `060`); it distinguishes CoW clones (pin the image) from independent copies (do not).
