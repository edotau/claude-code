#!/usr/bin/env bash
# Run in a worktree: isolate.sh <copy-dir> [<owned-path>...] builds a git-worktree copy; --remove <copy-dir> deletes it.
set -euo pipefail

usage() { echo "usage: isolate.sh <copy-dir> [<owned-path>...] | isolate.sh --remove <copy-dir>" >&2; exit 2; }

if [ "${1:-}" = "--remove" ]; then
  C=$(realpath -m "${2:?$(usage)}")
  W=$(git rev-parse --show-toplevel)
  git -C "$W" worktree remove --force "$C"
  git -C "$W" worktree prune
  exit 0
fi

W=$(git rev-parse --show-toplevel)
C=$(realpath -m "${1:?$(usage)}"); shift; OWN=("${@%/}")
case "$C" in /|"$(realpath -m "$HOME")") echo "isolate: refusing $C" >&2; exit 2;; esac
case "$C/" in "$W"/*) echo "isolate: $C is inside $W" >&2; exit 2;; esac
case "$W/" in "$C"/*) echo "isolate: $C contains $W" >&2; exit 2;; esac
[ -e "$C" ] && { echo "isolate: $C already exists" >&2; exit 2; }

git -C "$W" worktree add --detach "$C" HEAD >/dev/null

# refuse if any existing ancestor of $1 (a path under $C) is a symlink: HEAD content can't make one unless the
# applied diff did, and writing through it would escape the copy.
no_symlink_parent() {
  local rel="${1#"$C"/}" cur="$C"
  local IFS=/
  # shellcheck disable=SC2206 # $rel is a repo-relative path (no globs); split on / is intentional
  local -a comps=($rel)
  unset IFS
  for ((i = 0; i < ${#comps[@]} - 1; i++)); do
    cur="$cur/${comps[i]}"
    if [ -L "$cur" ]; then echo "isolate: refusing, symlink parent at $cur" >&2; exit 2; fi
  done
}

if [ "${#OWN[@]}" -gt 0 ]; then
  diff=$(git -C "$W" diff --binary HEAD -- "${OWN[@]}")
  [ -n "$diff" ] && printf '%s\n' "$diff" | git -C "$C" apply --binary

  while IFS= read -r -d '' f; do
    no_symlink_parent "$C/$f"
    mkdir -p "$(dirname "$C/$f")"
    cp -P "$W/$f" "$C/$f"
  done < <(git -C "$W" ls-files --others --exclude-standard -z -- "${OWN[@]}")
fi
