---
description: "Adversarial code review via the codex agent runner — second-opinion review using a different model stack to catch blind spots the session model misses. Writes a session-correlated handoff file."
argument-hint: "[--session-id <id>] [--uncommitted | --base <branch> | --commit <sha> | --pr] [extra review focus]"
allowed-tools: Bash
---

# /codex:adversarial-review — Second-Opinion Review via Codex

Runs `claude-code ask --agent codex` with an adversarial framing over a git diff. Uses a
**different model** than the current session, so it catches different blind spots — security
assumptions, edge cases the session model overlooked, missing tests, API misuse.

## Session correlation

Each invocation is tied to a session ID so results are traceable:

- **`--session-id <id>`** — explicit session ID (e.g., a task-id from `/workflow:orchestrate`)
- **omitted** — auto-derived from current git branch + short timestamp
- **handoff file**: `$(claude-code memory path)/reports/codex-sessions/<session-id>/review-<timestamp>.md`
- **status file**: `$(claude-code memory path)/reports/codex-sessions/<session-id>/status.md` (updated live)
- The session ID is printed back to stdout as `SESSION_ID=<id>` so callers can correlate or chain follow-ups.

## Scope (pick one; defaults to `--uncommitted`)

| Flag | What it reviews |
|------|-----------------|
| `--uncommitted` *(default)* | Staged + unstaged changes |
| `--base <branch>` | Diff between current branch and `<branch>` (PR-style) |
| `--commit <sha>` | A single commit |
| `--pr` | Shortcut for `--base <origin default branch>` |

Trailing free-text is passed as additional review focus (e.g., `--uncommitted "focus on concurrency"`).

## Execution

!bash -c '
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

  # --no-fallback: a second-model-family review that silently ran on Claude is worse than no review.
  if claude-code ask --agent codex --no-fallback "$PROMPT" 2>&1 | tee "$HANDOFF"; then
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
' _ "$ARGUMENTS"

## When to use

- **Before shipping a PR** — second-opinion after the `claude` agent's review phase passes
- **After architecture changes** — catch what a single model perspective missed
- **Security-sensitive code** — auth, user input, file uploads, injection surfaces
- **After `codex` (REVIEW mode) flags concerns** — corroborate findings with a fresh run

## Related

- `codex` agent, REVIEW mode — adversarial **pre**-review (before implementation)
- `claude` agent (review phase) — primary review stage (run this first)
- `/codex:dispatch` — generic task dispatch to the same runner (no review framing)

## Notes

- Runs non-interactively through `claude-code ask --agent codex`.
- Review is read-only — codex does not modify files in this mode.
- Session files accumulate under `<memory store>/reports/codex-sessions/` — clean up periodically.
