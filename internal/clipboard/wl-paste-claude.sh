#!/bin/sh
# Claude-aware `wl-paste` proxy for WSL2: routing lives in `claude-code clip paste` (internal/clipboard/wlproxy.go).
bin=$HOME/.claude/bin/claude-code
[ -x "$bin" ] || bin=$(command -v claude-code) || bin=
[ -n "$bin" ] && exec "$bin" clip paste "$@"
exec /usr/bin/wl-paste "$@" # no harness binary: plain wl-paste
