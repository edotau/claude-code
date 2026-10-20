---
description: "Full-lifecycle coding via the claude agent — architect, implement, review, or simplify the current change."
argument-hint: "[review | simplify | architect <task>] [diff range / path / blank for current diff]"
allowed-tools: Bash, Read, Grep, Glob, Edit, Write, Agent
---

One entry point for the coding lifecycle. Dispatches the **claude** agent, which owns
architecture, quality review, and simplification. It drives the `evaluate-code` and
`simplify` skills as its review/cleanup procedures, and pairs with `/security` for the
OWASP pass.

## Mode (first arg, optional)

| Arg | Phase | What runs |
|-----|-------|-----------|
| *(blank)* or `review` | Review & Simplify | Detectors → quality review (discipline lens) → apply simplifications → verdict |
| `simplify` | Simplify only | Behavior-preserving cleanups in the diff (`simplify` skill); no bug hunt |
| `architect <task>` | Architect | Assess codebase, weigh 2-3 approaches, emit a phased `## Plan` |

The remaining args are the scope: an explicit diff range, a path, or blank for the current diff.

## 1. Determine diff scope

```bash
RANGE="$ARGUMENTS"                        # may also carry a leading mode word — strip it
case "$RANGE" in review|simplify|architect*) RANGE="${RANGE#* }";; esac
[ -z "$RANGE" ] && BASE=$(git merge-base HEAD main 2>/dev/null) && RANGE="${BASE}...HEAD"
git diff ${RANGE:---cached} --stat
git diff --name-only ${RANGE:---cached}
```
If there's no branch divergence and nothing staged, fall back to `git diff --cached` and say so.

## 2. Detector first pass (advisory)

```bash
mapfile -d '' -t CHANGED < <(git diff --name-only -z ${RANGE:---cached} | grep -zE '\.(py|ts|tsx|js|jsx|go)$' || true)
[ "${#CHANGED[@]}" -gt 0 ] && claude-code review complexity "${CHANGED[@]}" --threshold medium --json
claude-code review diff --diff "${RANGE:-HEAD~1..HEAD}" --json
```
Fold findings into the report by citing specific functions/lines, not raw JSON.

## 3. Dispatch claude

Launch the **claude** agent with the resolved mode, range, and changed-file list as
scoped context (NOT this session's history):

```
Phase: <review | simplify | architect>
Diff scope: <RANGE>
Files changed: <names>
Detector findings: <complexity / diff_surgeon highlights>
```

- **review** → quality + the four-principle discipline lens; report must-fix / should-fix / nit / question with `file:line` and a ship verdict.
- **simplify** → apply behavior-preserving cleanups (`--extended` follows duplication to its source past the diff); name each change and confirm behavior is preserved.
- **architect** → emit the phased `## Plan` (Approach / Changes / Risks / Verification).

For the **security** half of a review, run `/security` in parallel — it dispatches
`security-reviewer` for the OWASP / secrets / injection pass. `/claude review` + `/security`
together are the full pre-merge gate.

## 4. Synthesize

Present one verdict. For review: the summary table + Ship it / Ship after must-fixes / Needs rework.
For simplify: the ranked list of applied changes with `[extended]` tags where a fix landed
outside the diff. For architect: the plan, ready to feed implementation.
