#!/usr/bin/env bash
set -euo pipefail

root="$(git -C "$(dirname "$0")" rev-parse --show-toplevel)"
command -v podman >/dev/null || { echo "$0: podman not found (brew install podman; podman machine start)" >&2; exit 1; }
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
  podman build ${GO_JOBS:+--build-arg GO_JOBS="${GO_JOBS}"} \
    --target "${target}" -f "${ctx}/container/Dockerfile" -t "${tag}" "${ctx}"
}

run() {
  [ $# -gt 0 ] || set -- make audit
  local tty=()
  [ -t 0 ] && tty=(-it)
  # Both entrypoints end in claude-code; replace them so `run make audit` runs make, not `claude-code make audit`.
  if [ "${target}" = base ]; then
    tty+=(--entrypoint "")
  else
    tty+=(--entrypoint /usr/bin/catatonit)
    set -- -- /usr/local/bin/nested-podman-entrypoint.sh "$@"
  fi
  # Nesting needs /dev/fuse, no SELinux label and an unmasked /proc (else crun can't mount proc); no --privileged.
  podman run --rm ${tty[@]+"${tty[@]}"} \
    --security-opt label=disable --security-opt seccomp=unconfined --security-opt "unmask=/proc/*" \
    --device /dev/fuse \
    "${tag}" "$@"
}

case "${1:-}" in
  build) build ;;
  run) shift; run "$@" ;;
  *) echo "usage: $0 build | run [cmd ...]" >&2; exit 2 ;;
esac
