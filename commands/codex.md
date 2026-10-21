---
description: "Adversarial code review via the codex agent runner — second-opinion review using a different model stack to catch blind spots the session model misses. Saves the review as a memory-store report."
argument-hint: "[--uncommitted | --base <branch> | --commit <sha> | --pr] [extra review focus]"
allowed-tools: Bash
---

# /codex — Adversarial Review via Codex

Launcher only. The review contract lives in `agents/codex.md` → "Adversarial diff review (`/codex`)".

1. Scope from the first word of `$ARGUMENTS` (default `--uncommitted`); the remaining words are extra focus.
   Write the diff to `d=$(mktemp)`:
   - `--uncommitted`: `git diff HEAD > "$d"`
   - `--base <b>`: `git diff "<b>...HEAD" > "$d"`
   - `--commit <sha>`: `git show --no-color --format= <sha> > "$d"`
   - `--pr`: `--base` with `$(git rev-parse --abbrev-ref origin/HEAD | sed 's#.*/##')`
2. Empty diff: say "nothing in scope" and stop.
3. Run, with the prompt from `agents/codex.md` → "Adversarial diff review" plus any focus:
   `out="$(claude-code memory path)/reports/$(date +%F)-codex-review-<scope>.md"; mkdir -p "${out%/*}"`
   `claude-code ask --agent codex --fallback vendors --attach "$d" "<prompt>" | tee "$out"; rm -f "$d"`
4. Relay the findings and the report path. `claude-code ask --log` / `--retry <id>` replays the dispatch.
