#!/usr/bin/env bash

# Claude-aware `wl-paste` proxy for WSL2.
#
# WSLg bridges a Windows clipboard image to Wayland as image/bmp ONLY, in 32bpp BI_BITFIELDS, which
# Claude's BMP decoder rejects (anthropics/claude-code#50552). The CLI's save chain (2.1.241) is:
#
#   xclip -t image/png || wl-paste --type image/png || xclip -t image/bmp
#     || wl-paste --type image/bmp || powershell GetImage->PNG
#
# The proxy answers BOTH wl-paste legs by TRANSCODING the real BMP through `claude-code bmp-transcode`:
# the bmp leg gets 24bpp BI_RGB, the png leg (inserted in 2.1.241) gets a PNG. Paste then succeeds on
# the native path — no PowerShell round trip, and the earlier the leg, the less of the chain has to
# hold its shape across upstream refactors.
#
# If transcoding is unavailable or fails, it falls back to FAILING the leg, which reaches the next leg
# and ultimately the PowerShell PNG fallback. Everything else delegates untouched.
#
# It identifies the CLI's own read by its PARENT's command line, not by walking ancestors for a process
# named "claude". Ancestor-walking matched ANY descendant of a Claude session — every Bash tool call,
# build, and test — so a user's own `wl-paste --type image/bmp` broke too, and the delegate-when-outside
# test could never pass when run from inside a session. The CLI spawns its chain through `sh -c`, so the
# parent's cmdline carries the whole `xclip ... || wl-paste ...` pipeline; nothing else looks like that.
#
# CLAUDE_WL_PASTE_PROXY: 0 = never interfere; reject = skip transcoding and fail the leg (the old
# behaviour, kept for comparison); 1 = apply the fix regardless of the parent; unset/auto = detect.
set -u

# A shell without WSLg's env (VS Code Remote-WSL terminals) gets the WSLg socket by absolute path.
wslg_socket=/mnt/wslg/runtime-dir/wayland-0
if [[ -z ${SSH_CONNECTION:-} && -z ${WAYLAND_DISPLAY:-} && -S $wslg_socket ]]; then
	export WAYLAND_DISPLAY=$wslg_socket
fi

self="$(readlink -f -- "$0")"

# Locate the real wl-paste: first PATH entry that is not this proxy.
real_wl_paste=
IFS=: read -ra path_dirs <<<"${PATH:-}"
for dir in "${path_dirs[@]}"; do
	[[ -n $dir ]] || dir=.
	candidate="$dir/wl-paste"
	if [[ -x $candidate && $(readlink -f -- "$candidate") != "$self" ]]; then
		real_wl_paste="$candidate"
		break
	fi
done
if [[ -z $real_wl_paste ]]; then
	echo "wl-paste: no real wl-paste behind the Claude clipboard proxy (is wl-clipboard installed?)" >&2
	exit 127
fi

# requests_type reports whether argv asks for mime, in --type=V, "--type V", or "-t V" form.
requests_type() {
	local want=$1 i n
	local -a argv
	shift
	argv=("$@")
	n=${#argv[@]}
	for ((i = 0; i < n; i++)); do
		case ${argv[i]} in
		--type="$want") return 0 ;;
		--type | -t)
			((i += 1))
			[[ $i -lt $n && ${argv[i]} == "$want" ]] && return 0
			;;
		esac
	done
	return 1
}

# is_cli_clipboard_read matches the CLI's own save chain in the parent's cmdline. Requiring the xclip
# leg alongside the wl-paste leg is what separates it from a user typing wl-paste themselves.
is_cli_clipboard_read() {
	local parent_cmd
	parent_cmd=$(parent_cmdline) || return 1
	[[ $parent_cmd == *"wl-paste"*"image/bmp"* ]] || return 1
	[[ $parent_cmd == *"xclip"*"clipboard"* || $parent_cmd == *"powershell"* ]]
}

# is_vendor_clipboard_read matches the agent TUIs that spawn wl-paste DIRECTLY — no `sh -c`, so no
# chain in the parent cmdline for is_cli_clipboard_read to see. Both ask for image/png and have
# nothing behind it on Linux, so on WSLg (BMP-only offer) that one shot always failed and their
# image paste never worked here at all:
#
#   gemini   `wl-paste --no-newline --type image/png`      (no bmp leg, no PowerShell fallback)
#   opencode `wl-paste -t image/png` then xclip            (PowerShell is confined to its win32 branch)
#
# NOT listed, deliberately: codex reads the clipboard natively and carries its OWN WSL detection plus
# a PowerShell `Get-Clipboard -Format Image` fallback, so it never shells out here; copilot has no
# image-paste path on Linux at all.
#
# An ALLOWLIST, not "intercept every direct caller": a user's own wl-paste keeps delegating untouched.
is_vendor_clipboard_read() {
	local parent_cmd
	parent_cmd=$(parent_cmdline) || return 1
	case $parent_cmd in
	# gemini ships as a node script, so the parent is node running the bundle; requiring both keeps
	# `claude-code gemini` (an in-process Go bridge that never spawns wl-paste) out of it.
	*node*gemini* | *gemini*node*) return 0 ;;
	*opencode*) return 0 ;;
	esac
	return 1
}

# parent_cmdline reads the spawning process's argv as a space-joined string; non-zero when /proc is
# unreadable (a non-Linux host, or a parent that exited between spawn and read).
parent_cmdline() {
	tr '\0' ' ' <"/proc/$PPID/cmdline" 2>/dev/null
}

# harness_bin locates the claude-code binary that owns the transcoder; empty when unavailable.
harness_bin() {
	local bin=$HOME/.claude/bin/claude-code
	[[ -x $bin ]] || bin=$(command -v claude-code 2>/dev/null) || return 1
	[[ -x $bin ]] && printf '%s' "$bin"
}

# fix_bmp transcodes the clipboard BMP to stdout. Returns non-zero when it cannot, so the caller can
# fall back to failing the leg (which reaches the CLI's PowerShell fallback).
fix_bmp() {
	local bin
	bin=$(harness_bin) || return 1
	[[ -n $bin ]] || return 1
	# Pipeline status is the transcoder's: an empty clipboard yields no bytes and it rejects them, so a
	# missing image still ends up non-zero rather than writing a truncated file.
	"$real_wl_paste" "$@" | "$bin" bmp-transcode
}

apply_fix() {
	if [[ ${CLAUDE_WL_PASTE_PROXY:-auto} != reject ]] && fix_bmp "$@"; then
		exit 0
	fi
	exit 1 # transcode skipped or failed: fail the leg so the CLI reaches its PowerShell fallback
}

# apply_png answers the image/png leg 2.1.241 inserted ahead of the BMP one. Buffered through a temp
# file on purpose: PNG bytes contain NULs, which command substitution would silently drop.
apply_png() {
	local tmp bin
	tmp=$(mktemp "${TMPDIR:-/tmp}/wl-paste-claude.XXXXXX") || exit 1
	trap 'rm -f -- "$tmp"' EXIT
	# Try the real thing first: a compositor that genuinely offers PNG needs no conversion. Asking
	# beats reading the offer list, which on WSLg can advertise a type it cannot serve.
	if "$real_wl_paste" "$@" >"$tmp" 2>/dev/null && [[ -s $tmp ]]; then
		cat -- "$tmp"
		exit 0
	fi
	# WSLg offers BMP only, so synthesize the PNG from that. Pipeline status is the transcoder's.
	bin=$(harness_bin)
	if [[ -n $bin ]] && "$real_wl_paste" --type image/bmp 2>/dev/null | "$bin" bmp-transcode --png >"$tmp" && [[ -s $tmp ]]; then
		cat -- "$tmp"
		exit 0
	fi
	exit 1 # fail the leg: the chain still has its BMP legs and the PowerShell fallback behind it
}

# apply_ssh answers an SSH session's image request from the client's clipboard bridge, never WSLg:
# png comes from `clip --ssh-stdout`, bmp fails the leg (the bridge carries PNG only).
apply_ssh() {
	local bin
	requests_type image/bmp "$@" && exit 1
	requests_type image/png "$@" || return 0
	bin=$(harness_bin) || exit 1
	"$bin" clip --ssh-stdout
	exit $?
}

# intercept routes the two image legs; anything else falls through to plain delegation.
intercept() {
	[[ -z ${SSH_CONNECTION:-} ]] || apply_ssh "$@"
	requests_type image/bmp "$@" && apply_fix "$@"
	# reject mode is the pre-transcoder comparison, so it leaves the new PNG leg untouched.
	[[ ${CLAUDE_WL_PASTE_PROXY:-auto} == reject ]] || { requests_type image/png "$@" && apply_png "$@"; }
}

case ${CLAUDE_WL_PASTE_PROXY:-auto} in
0) exec "$real_wl_paste" "$@" ;;              # forced off: never interfere
1 | reject) intercept "$@" ;;                 # forced on, parent ignored
# The vendor TUIs only ever ask for image/png: neither has a bmp request to answer, and reject mode
# is a Claude-chain comparison knob, so it does not apply here.
*)
	if is_cli_clipboard_read; then
		intercept "$@"
	elif is_vendor_clipboard_read; then
		[[ -z ${SSH_CONNECTION:-} ]] || apply_ssh "$@"
		requests_type image/png "$@" && apply_png "$@"
	fi
	;;
esac

exec "$real_wl_paste" "$@"
