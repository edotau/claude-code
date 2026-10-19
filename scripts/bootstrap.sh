#!/bin/sh
# Curl-able fresh-box bootstrap: clone to a staging dir, build it, let the built binary adopt the
# live directory (it can't adopt itself before it exists). See README.md → Setup modes.
set -eu

REPO="${REPO:-https://github.com/edotau/claude-code}"
INTO="${INTO:-$HOME/.claude}"

if [ -e "$INTO/.git" ]; then
  echo "already a checkout"
  exit 0
fi

STAGE="$INTO.stage"
git clone "$REPO" "$STAGE"
make -C "$STAGE" compile
"$STAGE/bin/claude-code" install --setup adopt --from "$STAGE" --into "$INTO" "$@"
make -C "$INTO" install
