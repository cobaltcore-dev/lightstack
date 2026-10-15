#!/bin/bash
# workflows.sh — SPEC-028 tier A/B conformance cases for cross-service scenarios (tier C).
# WAVE-2 PLACEHOLDER: the owning agent fills this file; the manifest section
# (manifest/workflows.json) must list every command these cases cover.

# shellcheck source=../lib/common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/../lib" && pwd)/common.sh"

suite_begin workflows

# Wave 2: cases go here. Every case id must exist in manifest/workflows.json.

suite_end
