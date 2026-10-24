#!/usr/bin/env bash
# lib/check-style.sh — ruff check + ruff format --check on given paths
# Usage: check-style.sh <path> [path ...]

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "$SKILL_DIR/common.sh"

require_cmd ruff

[[ $# -eq 0 ]] && die "Usage: check-style.sh <path> [path ...]"

status=0

info "Running ruff check..."
ruff check "$@" || { warn "ruff found issues"; status=1; }

info "Running ruff format --check..."
ruff format --check "$@" || { warn "ruff format would reformat"; status=1; }

exit "$status"
