# Contract: Compute Disk & Lifecycle (WS-0)

External interface = OpenStack Compute (Nova) v2.1 + Image (Glance) v2, exercised via `python-openstackclient`, gophercloud, and Terraform. All behavior must match upstream with no client modification (Constitution I). Tests are authored **RED-first** before implementation.

## Behavioral contracts

| ID | Given | When | Then |
|----|-------|------|------|
| C-WS0-1 | real mode, an uploaded image | `openstack server create --image IMG --flavor F vm1` | server → `ACTIVE`; a real libvirt domain exists; its disk is `/var/lib/o3k/instances/<id>/disk.qcow2`, a qcow2 overlay whose backing file is `/var/lib/o3k/images/image-<imgid>.raw`. No disk-path/"image not found" error. |
| C-WS0-2 | one image | create `vm1` and `vm2` from it | each has its own overlay; writing inside `vm1` is not visible in `vm2` or in the base image. |
| C-WS0-3 | `vm1` uses image `IMG` | `openstack image delete IMG` | rejected, HTTP 409 "image in use"; `IMG` still usable. |
| C-WS0-4 | running `vm1` | `openstack server delete vm1` | `instances/<id>/` removed; base image `IMG` intact. |
| C-WS0-5 | running `vm1` | `openstack server stop/start/reboot vm1` | libvirt domain state changes; `openstack server show` `power_state`/`status` matches (SHUTOFF/ACTIVE). |
| C-WS0-6 | booted `vm1` | `openstack console log show vm1` | returns the guest serial/boot output. |
| C-WS0-7 | a qcow2 image file | `openstack image create --disk-format qcow2 ... < file.qcow2` | stored as `image-<id>.raw` (raw bytes); `openstack image show` reports `disk_format raw`. |

## Contract tests (to author RED-first)

- `test/contract/ws0_disk_test.go` (gophercloud): C-WS0-1/2/4/5/7 — create/list/show/delete servers + images; assert states and `disk_format`.
- `test/ws0_disk_real_test.sh` (host, real mode): C-WS0-1/2/4 — `virsh list`/`virsh domblklist` + `qemu-img info` assert the overlay path and `backing file` + `-F raw`; isolation via writing a sentinel file in `vm1` and checking `vm2`.
- `test/ws0_image_inuse_test.sh`: C-WS0-3 — delete blocked while in use, allowed after the server is gone.
- `test/ws0_console_test.sh`: C-WS0-6 — `console log show` returns boot output.

## Notes
- `force_raw_images`-equivalent: conversion happens once on Glance ingest (research R2); VM-create only creates the thin overlay (keeps create < 10 s).
- Capacity failure (insufficient host RAM/CPU) → server `ERROR` with a clear message (research R12), not a hang.
