#!/usr/bin/env bash
# Build or run the Ubuntu harness image. Usage: docker/image.sh build | run [cmd ...] (default: make audit).
# TARGET=base (claude entrypoint) or nested-podman (default); IMAGE overrides the tag.
set -euo pipefail

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
engine="${ENGINE:-$(command -v podman || command -v docker)}"
target="${TARGET:-nested-podman}"
tag="${IMAGE:-claude-code-harness:${target}}"

build() {
  local ctx
  ctx="$(mktemp -d)"
  trap 'rm -rf "${ctx}"' RETURN
  # Tracked + untracked-but-not-ignored files that exist: never the vault, memory, sessions or history.
  git -C "${root}" ls-files -z --cached --others --exclude-standard \
    | (cd "${root}" && while IFS= read -r -d '' f; do [ -e "$f" ] && printf '%s\0' "$f"; done) \
    | tar -C "${root}" --null -T - -cf - \
    | tar -C "${ctx}" -xf -
  "${engine}" build ${CLAUDE_CHANNEL:+--build-arg CLAUDE_CHANNEL="${CLAUDE_CHANNEL}"} \
    ${GO_JOBS:+--build-arg GO_JOBS="${GO_JOBS}"} \
    --target "${target}" -f "${ctx}/docker/Dockerfile" -t "${tag}" "${ctx}"
}

run() {
  [ $# -gt 0 ] || set -- make audit
  local tty=()
  [ -t 0 ] && tty=(-it)
  # base's entrypoint is claude; clear it so `run make audit` runs make, not `claude make audit`.
  [ "${target}" = base ] && tty+=(--entrypoint "")
  # Nesting needs /dev/fuse, no SELinux label and an unmasked /proc (else crun can't mount proc); no --privileged.
  local unmask=(--security-opt "unmask=/proc/*")
  [ "$(basename "${engine}")" = docker ] && unmask=(--security-opt systempaths=unconfined)
  "${engine}" run --rm ${tty[@]+"${tty[@]}"} \
    --security-opt label=disable --security-opt seccomp=unconfined "${unmask[@]}" \
    --device /dev/fuse \
    "${tag}" "$@"
}

case "${1:-}" in
  build) build ;;
  run) shift; run "$@" ;;
  *) echo "usage: $0 build | run [cmd ...]" >&2; exit 2 ;;
esac
