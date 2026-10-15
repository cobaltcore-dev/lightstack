#!/bin/bash
# compute.sh — SPEC-028 tier A/B conformance cases for the service.
# WAVE-2 PLACEHOLDER: the owning agent fills this file; the manifest section
# (manifest/compute.json) must list every command these cases cover.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin compute

# Wave 2: cases go here. Every case id must exist in manifest/compute.json.

suite_end
