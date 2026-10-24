# python-patterns/lib/common.sh
# Primitives sourced by skills/python-patterns/lib/check-style.sh (currently its sole consumer).
# Usage (at top of helper):
#   SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#   # shellcheck source=./common.sh
#   source "$SKILL_DIR/common.sh"

set -euo pipefail

# -----------------------------------------------------------------------------
# Output helpers — data to stdout, diagnostics to stderr
# -----------------------------------------------------------------------------
warn() { printf 'WARN  %s\n' "$*" >&2; }
info() { printf 'INFO  %s\n' "$*" >&2; }
die()  { printf 'ERROR %s\n' "$*" >&2; exit 1; }

# -----------------------------------------------------------------------------
# Env var guards
# -----------------------------------------------------------------------------
require_env() {
  local missing=()
  for var in "$@"; do
    [[ -z "${!var:-}" ]] && missing+=("$var")
  done
  if (( ${#missing[@]} > 0 )); then
    warn "Missing env vars: ${missing[*]}"
    warn "Source ~/.claude/env or set them in your shell."
    exit 1
  fi
}

require_cmd() {
  for cmd in "$@"; do
    command -v "$cmd" >/dev/null 2>&1 || die "Required command not found: $cmd"
  done
}

# -----------------------------------------------------------------------------
# Output formatting — JSON by default, pretty with PRETTY=1
# -----------------------------------------------------------------------------
json_or_pretty() {
  if [[ "${PRETTY:-0}" == "1" ]]; then
    jq -r '.'
  else
    jq -c '.'
  fi
}

# -----------------------------------------------------------------------------
# Destructive-operation gate — short-circuits with DRY_RUN=1
# -----------------------------------------------------------------------------
confirm_or_dry() {
  local description="$1"
  if [[ "${DRY_RUN:-0}" == "1" ]]; then
    info "DRY_RUN: would $description"
    return 1
  fi
  return 0
}
