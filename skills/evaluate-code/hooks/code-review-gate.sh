#!/bin/bash
# code-review-gate.sh — non-blocking pre-commit advisory.
#
# Runs the evaluate-code simplicity + surgical detectors on staged files and prints
# warnings. NEVER blocks the commit (always exits 0) — the goal is awareness.
#
# This hook is OPT-IN. It is deliberately NOT wired into the generated settings.json
# (internal/settings/settings.tmpl.json). Enable it one of two ways:
#
#   1. Git pre-commit (per repo):
#        ln -s "$HOME/.claude/skills/evaluate-code/hooks/code-review-gate.sh" .git/hooks/pre-commit
#      (or call it from an existing husky/pre-commit chain)
#
#   2. Claude Code PreToolUse on git commit — add the hook to internal/settings/settings.tmpl.json:
#        { "matcher": "Bash",
#          "hooks": [ { "type": "command",
#            "command": "bash ~/.claude/skills/evaluate-code/hooks/code-review-gate.sh" } ] }
#      then re-render settings.json: claude-code install.
#
# Detectors live in skills/evaluate-code/scripts/ (diff_surgeon etc.), except the #2
# Simplicity detector complexity_checker.py, which is canonical in skills/simplicity/scripts/.
# Stdlib-only; no venv required.

set -uo pipefail

# Resolve the scripts dirs from the Claude config dir.
ROOT="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"
CR="$ROOT/skills/evaluate-code/scripts"
SIMP="$ROOT/skills/simplicity/scripts"

if [ ! -d "$CR" ]; then
	# Detectors not installed in this workspace — no-op, but say which path was tried.
	echo "code-review gate: detectors not found at $CR, skipping."
	exit 0
fi

# Nothing staged → nothing to check.
staged=$(git diff --cached --name-only 2>/dev/null || true)
[ -z "$staged" ] && exit 0

echo "--- code-review gate (advisory) ---"

# Read filenames into an array so names with spaces / leading dashes can't
# word-split into injected argparse flags (H1). NUL-delimited for total safety.
mapfile -d '' -t changed_files < <(
	git diff --cached --name-only -z --diff-filter=ACMR 2>/dev/null |
		grep -zE '\.(py|ts|tsx|js|jsx)$' || true
)

if [ "${#changed_files[@]}" -gt 0 ]; then
	echo "[simplicity] complexity_checker (medium):"
	if [ ! -d "$SIMP" ]; then
		echo "  skipped — simplicity scripts not found at $SIMP"
	else
		# Capture stdout+stderr and the exit code — a crash must not read as "clean".
		out=$(python3 "$SIMP/complexity_checker.py" "${changed_files[@]}" --threshold medium 2>&1)
		rc=$?
		if [ "$rc" -ne 0 ]; then
			echo "  detector failed (exit $rc):"
			printf '%s\n' "$out" | sed 's/^/    /'
		else
			printf '%s\n' "$out" | grep -E '^\s+\[WARN\]|^Verdict:' || echo "  clean"
		fi
	fi
fi

echo "[surgical] diff_surgeon (staged):"
out=$(python3 "$CR/diff_surgeon.py" 2>&1)
rc=$?
if [ "$rc" -ne 0 ]; then
	echo "  detector failed (exit $rc):"
	printf '%s\n' "$out" | sed 's/^/    /'
else
	printf '%s\n' "$out" | grep -E 'Noise ratio:|Verdict:' || echo "  clean"
fi

echo "--- /code-review gate ---"
exit 0
