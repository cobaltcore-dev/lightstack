#!/usr/bin/env bash
# SPEC-014 T005 (FR-013, FR-017, SC-009/010/012): real-mode boot from an
# RGW(S3)-backed image.
#   1. lazy fetch — with NO local base, booting materializes the raw base from RGW
#      and the VM reaches ACTIVE (pure-S3, multi-node bootable).
#   2. integrity  — if the RGW object no longer matches the Glance checksum, the
#      boot is refused (server -> ERROR), never booted from corrupt data.
#
# Run ON the libvirt host, against a running real-mode Lightstack (nova libvirt_mode:real,
# glance storage_mode:s3 -> the RGW below). Source your OpenStack env first, and
# export the RGW S3 endpoint + credentials Lightstack's Glance uses:
#   OS_AUTH_URL=... RGW_S3_ENDPOINT=http://<rgw>:<port> \
#   AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... ./test/ws0_s3_boot_test.sh
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
: "${RGW_S3_ENDPOINT:?set RGW_S3_ENDPOINT (the RGW Lightstack Glance uses)}"
: "${AWS_ACCESS_KEY_ID:?set AWS_ACCESS_KEY_ID for the RGW}"
: "${AWS_SECRET_ACCESS_KEY:?set AWS_SECRET_ACCESS_KEY for the RGW}"

IMG="${LIGHTSTACK_TEST_IMAGE:-cirros}"          # a BOOTABLE image uploaded while glance=s3
NET="${LIGHTSTACK_TEST_NET:-net1}"
FLAVOR="${LIGHTSTACK_TEST_FLAVOR:-m1.tiny}"
BASE_DIR="${LIGHTSTACK_IMAGE_DIR:-/var/lib/lightstack/images}"
# Exported so the inline python (rgw_put) sees them via os.environ.
export RGW_BUCKET="${RGW_BUCKET:-images}"
export RGW_S3_ENDPOINT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"

command -v python3 >/dev/null || { echo "FATAL: python3 required"; exit 1; }
python3 -c 'import boto3' 2>/dev/null || { echo "FATAL: boto3 required (pip install boto3)"; exit 1; }

cleanup(){ openstack server delete t005-lazy t005-corrupt 2>/dev/null || true
           openstack image delete t005-corrupt-img 2>/dev/null || true
           rm -f /tmp/t005-corrupt.raw; }
trap cleanup EXIT

# rgw_put <key> <bytes> — overwrite an RGW object (used to corrupt the base).
rgw_put(){ python3 - "$1" "$2" <<'PY'
import os, sys, boto3
from botocore.client import Config
key, body = sys.argv[1], sys.argv[2].encode()
boto3.client("s3", endpoint_url=os.environ["RGW_S3_ENDPOINT"],
  aws_access_key_id=os.environ["AWS_ACCESS_KEY_ID"], aws_secret_access_key=os.environ["AWS_SECRET_ACCESS_KEY"],
  region_name=os.environ["AWS_DEFAULT_REGION"],
  config=Config(s3={"addressing_style":"path"}, signature_version="s3v4")
).put_object(Bucket=os.environ["RGW_BUCKET"], Key=key, Body=body)
print("overwrote", key)
PY
}

# ---------------------------------------------------------------------------
# 1) lazy fetch on boot: no local base -> materialized from RGW -> ACTIVE
# ---------------------------------------------------------------------------
echo "== T005.1 lazy fetch (RGW-backed image, no local base)"
imgid=$(openstack image show "$IMG" -f value -c id)
base="$BASE_DIR/image-${imgid}.raw"
sudo rm -f "$base"
sudo test -e "$base" && { echo "FAIL: base still present after rm ($base)"; exit 1; } || true
echo "  no local base present; booting $IMG ($imgid)"
openstack server create --image "$imgid" --flavor "$FLAVOR" --network "$NET" t005-lazy --wait
st=$(openstack server show t005-lazy -f value -c status)
[ "$st" = "ACTIVE" ] || { echo "FAIL: server not ACTIVE (got $st)"; exit 1; }
sudo test -f "$base" || { echo "FAIL: base was NOT fetched from RGW to $base"; exit 1; }
echo "PASS: base lazily fetched from RGW on boot; VM ACTIVE"

# ---------------------------------------------------------------------------
# 2) integrity: RGW object != Glance checksum -> boot refused (ERROR)
#    Uses a throwaway image so the shared bootable image stays intact.
# ---------------------------------------------------------------------------
echo "== T005.2 corrupt RGW object -> checksum verify fails -> ERROR"
head -c 1048576 /dev/urandom > /tmp/t005-corrupt.raw
openstack image create t005-corrupt-img --disk-format raw --container-format bare \
  --file /tmp/t005-corrupt.raw >/dev/null
cid=$(openstack image show t005-corrupt-img -f value -c id)
cbase="$BASE_DIR/image-${cid}.raw"
# replace the stored object with different bytes: fetched md5 will not match the
# checksum Glance recorded at upload.
rgw_put "images/image-${cid}.raw" "CORRUPT-not-the-real-image-data"
sudo rm -f "$cbase"
echo "  booting from the corrupted image (expect ERROR)"
openstack server create --image "$cid" --flavor "$FLAVOR" --network "$NET" t005-corrupt --wait 2>/dev/null || true
st=$(openstack server show t005-corrupt -f value -c status 2>/dev/null || echo MISSING)
if [ "$st" = "ERROR" ]; then
  echo "PASS: corrupt base refused (server ERROR, not booted)"
else
  echo "FAIL: expected ERROR from checksum mismatch, got '$st'"; exit 1
fi
sudo test -e "$cbase" && echo "  note: a base file remains at $cbase (fetched-but-rejected)" || true

echo
echo "[ws0-s3-boot] OK — lazy fetch + checksum-integrity both validated"
