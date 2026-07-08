# Quickstart — validate & use Ceph RGW with o3k

Two flows: **A** (Glance images on RGW) and **B** (tenant object storage). For a real RGW fixture, use `deployments/rook/setup-rook-rgw.sh` (Rook/minikube) or `ceph/demo` (docker-compose, CI); both print an S3 endpoint + keys.

## A. Glance image backend on RGW

1. Pre-create the bucket (o3k does not): `aws --endpoint-url $RGW s3 mb s3://images`.
2. Configure Glance (o3k.yaml):
   ```yaml
   glance:
     storage_mode: s3
     s3_endpoint: https://rgw.example:443     # or http://<ip>:<port> for dev
     s3_bucket: images
     s3_region: us-east-1
     s3_ca_bundle: /etc/o3k/rgw-ca.pem        # for internal-CA TLS (omit for HTTP/public CA)
   ```
   Credentials via env: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`.
3. Validate the round-trip:
   ```bash
   openstack image create --disk-format qcow2 --file cirros.img cirros
   aws --endpoint-url $RGW s3 ls s3://images/           # expect images/image-<id>.raw
   openstack image save cirros --file out.img && cmp cirros.img out.img   # or checksum match
   ```
4. Validate **boot** (real mode; the lazy-fetch path): on a node with no local base,
   `openstack server create --image cirros --flavor m1.small vm1` → the node fetches
   `image-<id>.raw` from RGW, normalizes, verifies checksum, and boots. Instance goes `ACTIVE`.
   A bad checksum / unreachable RGW → instance `ERROR` (fail-fast), never a corrupt boot.

## B. Tenant object storage via o3k identity

1. **Wire RGW to trust o3k** (`contracts/rgw-wireup.md`), ceph.conf:
   ```ini
   rgw s3 auth use keystone = true
   rgw keystone api version   = 3
   rgw keystone url           = https://o3k-keystone:35357
   rgw keystone implicit tenants = s3
   rgw keystone accepted roles   = admin,member,reader
   ```
   (No `rgw keystone admin token` needed — o3k's `/v3/s3tokens` self-validates the signature.)
2. **Tenant mints S3 credentials** (standard OpenStack CLI):
   ```bash
   openstack ec2 credentials create        # prints access + secret, scoped to the project
   ```
3. **Use them** with any S3 client against the RGW endpoint:
   ```bash
   AWS_ACCESS_KEY_ID=<access> AWS_SECRET_ACCESS_KEY=<secret> \
     aws --endpoint-url $RGW s3 mb s3://backups
   ... cp / ls / get ...
   ```
   RGW forwards the signature to o3k `POST /v3/s3tokens`; o3k validates and returns the
   project/roles; RGW namespaces the bucket under the project (per-project isolation).
4. **Isolation & revocation checks:**
   - A second project can create its own `s3://backups` (per-project namespace); neither sees the other's.
   - `openstack ec2 credentials delete <access>` → subsequent requests are rejected.

## CI (automated, no k8s)

`ceph/demo` + o3k + postgres in one docker-compose; a bash driver runs A (upload/download/checksum + boot-fetch) and B (mint creds → S3 client on the runner → RGW → o3k `/v3/s3tokens` + isolation + revocation). See `.github/workflows/ci.yml` (new job) and `test/*.sh`.

## Manual deployment-parity (Rook)

`deployments/rook/setup-rook-rgw.sh` brings up Rook+Ceph+RGW (HEALTH_OK) on minikube; wire o3k-on-k3s to its NodePort; boot a KVM guest and access RGW from inside over the tenant network (the one path CI can't do). Runbook: `docs/testing/rook-rgw-fixture.md`.
