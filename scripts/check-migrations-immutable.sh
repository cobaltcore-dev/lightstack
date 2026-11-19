#!/bin/bash
#
# SPEC-032 FR-025: the baseline and every release migration are immutable once on main; a schema
# change goes into a fragment in migrations/next/. Compares BASE with HEAD and fails, with one
# GitHub annotation per file, when a numbered migration was changed or removed — a rename counts as
# a removal. New numbered files (a release's own) and fragments pass.
#
# Usage: scripts/check-migrations-immutable.sh <base-revision>
#
set -euo pipefail

base=${1:?usage: $0 <base-revision>}

# A digit after _v: every release migration (002_v0.7.0…), but none of the migrations the squash
# moved away (044_volume_groups, 073_volume_transfer_awaiting), so the squash change itself passes.
changed=$(git diff --no-renames --name-status --diff-filter=DM "$base" HEAD -- \
    'migrations/001_baseline.*' 'migrations/[0-9][0-9][0-9]_v[0-9]*' | cut -f2)

for path in $changed; do
    echo "::error file=$path::numbered migrations are immutable once on main; put the change in a fragment (migrations/next/)"
done
[ -z "$changed" ]
