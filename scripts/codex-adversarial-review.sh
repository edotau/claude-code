#!/usr/bin/env bash
# Scope flags (first word of $1, default --uncommitted): --uncommitted (staged+unstaged) |
#   --base <branch> (diff vs branch, PR-style) | --commit <sha> (single commit) | --pr (shortcut for
#   --base <origin default branch>). Optional leading "--session-id <id>" pins the session; default is
#   derived from the current branch + a UTC timestamp. Remaining text is extra review focus.
# Session correlation: handoff file is
#   "$(claude-code memory path)/reports/codex-sessions/<session-id>/review-<timestamp>.md"; a live-updated
#   status file sits beside it as "status.md". SESSION_ID is echoed to stdout as "SESSION_ID=<id>" so
#   callers (e.g. /workflow:orchestrate) can correlate or chain follow-ups.
set -eo pipefail

ARGS="${1:-}"
SESSION_ID=""
SCOPE_FLAGS="--uncommitted"
FOCUS=""

if [[ "$ARGS" == "--session-id "* ]]; then
  rest="${ARGS#--session-id }"
  SESSION_ID="${rest%% *}"
  ARGS="${rest#$SESSION_ID}"
  ARGS="${ARGS# }"
fi

case "$ARGS" in
  "--pr"*)
    base=$(git rev-parse --abbrev-ref origin/HEAD 2>/dev/null | sed "s#.*/##" || echo "main")
    SCOPE_FLAGS="--base $base"
    FOCUS="${ARGS#--pr}"
    ;;
  "--base "*)
    rest="${ARGS#--base }"
    base="${rest%% *}"
    SCOPE_FLAGS="--base $base"
    FOCUS="${rest#$base}"
    ;;
  "--commit "*)
    rest="${ARGS#--commit }"
    sha="${rest%% *}"
    SCOPE_FLAGS="--commit $sha"
    FOCUS="${rest#$sha}"
    ;;
  "--uncommitted"*)
    SCOPE_FLAGS="--uncommitted"
    FOCUS="${ARGS#--uncommitted}"
    ;;
  *)
    FOCUS="$ARGS"
    ;;
esac
FOCUS="${FOCUS# }"

if [[ -z "$SESSION_ID" ]]; then
  branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null | tr "/" "-" || echo "detached")
  ts=$(date -u +"%Y%m%d-%H%M")
  SESSION_ID="${branch}-${ts}"
fi

SESS_DIR="$(claude-code memory path)/reports/codex-sessions/$SESSION_ID"   # the repo's memory store, never docs/
mkdir -p "$SESS_DIR"

TS_ISO=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
HANDOFF="$SESS_DIR/review-${TS_ISO//:/}.md"
STATUS="$SESS_DIR/status.md"

cat > "$STATUS" <<EOF
# Codex Adversarial Review — Status

- Session ID: \`$SESSION_ID\`
- State: running
- Started: $TS_ISO
- Branch: $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "?")
- Scope: $SCOPE_FLAGS
- Focus: ${FOCUS:-<none>}
- Handoff: \`$HANDOFF\`
EOF

case "$SCOPE_FLAGS" in
  "--base "*)   DIFF=$(git diff "${SCOPE_FLAGS#--base }"...HEAD) ;;
  "--commit "*) DIFF=$(git show --no-color --format= "${SCOPE_FLAGS#--commit }") ;;
  *)            DIFF=$(git diff HEAD; git diff --cached) ;;
esac

if [[ -z "$DIFF" ]]; then
  echo "Nothing in scope ($SCOPE_FLAGS) — nothing to review." | tee "$HANDOFF"
  RC=1
  STATE="skipped (empty diff)"
else
  PROMPT="You are an adversarial reviewer. Your job is to find what the author missed — not to confirm what is correct.

Review the diff with these priorities, in order:
1. CRITICAL: security vulnerabilities, data loss, concurrency races, resource leaks
2. HIGH: bugs, broken error handling, incorrect async/await, API misuse
3. MEDIUM: missing tests for new behavior, brittle assumptions, unhandled edge cases
4. LOW: maintainability, naming, minor style drift

Rules:
- Skepticism default ON. Assume the author is wrong until proven right.
- Call out hidden assumptions (what must be true for this code to work?)
- Do NOT praise. Do NOT summarize the change. ONLY report findings.
- For each finding: <severity> <file:line> <one-line issue> <one-line fix>
- If scope is clean, say exactly: VERDICT: no actionable findings.

${FOCUS:+Additional focus: $FOCUS}

<diff>
${DIFF:0:400000}
</diff>"

  # --fallback vendors: an unavailable codex may hand off to another vendor family, never to Claude.
  if claude-code ask --agent codex --fallback vendors "$PROMPT" 2>&1 | tee "$HANDOFF"; then
    STATE="complete"
    RC=0
  else
    RC=$?
    STATE="failed"
  fi
fi

cat > "$STATUS" <<EOF
# Codex Adversarial Review — Status

- Session ID: \`$SESSION_ID\`
- State: $STATE
- Started: $TS_ISO
- Completed: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
- Branch: $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "?")
- Scope: $SCOPE_FLAGS
- Focus: ${FOCUS:-<none>}
- Handoff: \`$HANDOFF\`
- Exit code: $RC
EOF

echo
echo "=== SESSION_ID=$SESSION_ID ==="
echo "Status:  $STATUS"
echo "Handoff: $HANDOFF"
exit $RC
