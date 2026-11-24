#!/bin/sh
# Claude-aware `wl-paste` proxy for WSL2: routing lives in `claude-code clip paste` (internal/clipboard/wlproxy.go).
bin=$HOME/.claude/bin/claude-code
[ -x "$bin" ] || bin=$(command -v claude-code) || bin=
[ -n "$bin" ] && exec "$bin" clip paste "$@"
# No harness binary: the first PATH wl-paste that is not a proxy, matching findRealWlPaste.
IFS=:
set -f
for d in $PATH; do
	w=${d:-.}/wl-paste
	[ -f "$w" ] && [ -x "$w" ] && ! grep -qF 'Claude-aware `wl-paste` proxy for WSL2' "$w" && exec "$w" "$@"
done
echo "wl-paste: no real wl-paste behind the Claude clipboard proxy (is wl-clipboard installed?)" >&2
exit 127
