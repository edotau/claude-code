#!/usr/bin/env bash
# lib/check-style.sh — ruff + black --check on given paths
# Usage: check-style.sh <path> [path ...]

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./common.sh
source "$SKILL_DIR/common.sh"

require_cmd ruff black

[[ $# -eq 0 ]] && die "Usage: check-style.sh <path> [path ...]"

info "Running ruff check..."
ruff check "$@" || warn "ruff found issues"

info "Running black --check..."
black --check "$@" || warn "black would reformat"
