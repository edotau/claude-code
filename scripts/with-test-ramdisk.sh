#!/usr/bin/env bash
# Runs "$@" with TMPDIR on a throwaway APFS RAM disk; test temp-dir churn queues on the macOS Data volume (67s -> 27s).
# TEST_RAMDISK=0 skips it; off macOS, or if the disk cannot be made, the command runs on the normal TMPDIR.
set -uo pipefail

if [ "$(uname -s)" != Darwin ] || [ "${TEST_RAMDISK:-1}" = 0 ]; then
	exec "$@"
fi

volume="claude-code-test-$$"
mount="/Volumes/$volume"
sectors=$((4 * 1024 * 1024 * 2)) # 4 GiB in 512-byte sectors; RAM is only used as files are written

dev="$(hdiutil attach -nomount "ram://$sectors" 2>/dev/null | awk 'NR == 1 {print $1}')"
fallback() {
	echo "with-test-ramdisk: no RAM disk, using the normal TMPDIR (expect a slower run)" >&2
	exec "$@"
}
[ -n "$dev" ] || fallback "$@"
if ! diskutil erasevolume APFS "$volume" "$dev" >/dev/null 2>&1; then
	hdiutil detach "$dev" -force >/dev/null 2>&1
	fallback "$@"
fi

# -force: a helper a test leaked must not keep the volume, or its RAM, alive.
trap 'hdiutil detach "$dev" -force >/dev/null 2>&1' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$mount/tmp"
TMPDIR="$mount/tmp" "$@"
