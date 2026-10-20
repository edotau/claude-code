#!/usr/bin/env bash
# Run in a worktree: isolate.sh <copy-dir> <owned-path>... — copy it, revert changes outside owned paths to HEAD.
set -euo pipefail
W=$(git rev-parse --show-toplevel)
C=$(realpath -m "${1:?usage: isolate.sh <copy-dir> <owned-path>...}"); shift; OWN=("${@%/}")
case "$C/" in "$W"/*) echo "isolate: $C is inside $W" >&2; exit 2;; esac
# Delete only a copy this script made (marker) or an empty dir: a wrong <copy-dir> must never wipe a real tree.
if [ -e "$C" ]; then
  [ -e "$C/.isolate-copy" ] || [ -z "$(ls -A "$C")" ] || { echo "isolate: $C is not an isolate copy" >&2; exit 2; }
  chmod -R u+w "$C"; find "$C" -delete
fi
mkdir -p "$C"; touch "$C/.isolate-copy"
(cd "$W" && tar --exclude=.git --exclude="./.*-state" --exclude="./.scratch*" --exclude="./.tmpbin" -cf - .) |
  tar -xf - -C "$C"
own(){ local o; for o in "${OWN[@]}"; do case "$1" in "$o"/*|"$o") return 0;; esac; done; return 1; }
cd "$W"
git diff --no-renames --name-only HEAD | while read -r f; do
  own "$f" && continue
  if git cat-file -e HEAD:"$f" 2>/dev/null; then
    mkdir -p "$(dirname "$C/$f")"; git show HEAD:"$f" > "$C/$f"
  else rm -f "$C/$f"; fi
done
git ls-files --others --exclude-standard | while read -r f; do own "$f" || rm -f "$C/$f"; done
