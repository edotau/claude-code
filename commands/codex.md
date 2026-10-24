---
description: "Adversarial code review via the codex agent runner — second-opinion review using a different model stack to catch blind spots the session model misses. Saves the review as a memory-store report."
argument-hint: "[ask|resume|review|transfer|status|result|cancel|setup|models] [args…] (bare = review --adversarial)"
allowed-tools: Bash
---

# /codex — Codex via `claude-code codex <verb>`

Launcher only. Verbs, procedure and the report contract live in `agents/codex.md`;
`claude-code codex -h` lists the surface.

1. First token of `$ARGUMENTS` picks the verb when it is one of `ask resume review transfer
   status result cancel setup models`; otherwise (including empty) the verb is
   `review --adversarial` and all of `$ARGUMENTS` are its args: scope flag `--uncommitted`
   (default) / `--base <b>` / `--commit <sha>` / `--pr`, remaining words = review focus.
2. `review` (default or explicit): tee to the reports path, `<scope>` = the scope word
   without dashes (`uncommitted`, `base-<b>`, `commit-<sha>`, `pr`):

   ```bash
   out="$(claude-code memory path)/reports/$(date +%F)-codex-review-<scope>.md"
   mkdir -p "${out%/*}"; claude-code codex review <args> | tee "$out"
   ```

   `nothing in scope` (exit 1) → say so and stop.
3. Any other verb: `claude-code codex <verb> <rest>` straight through, no tee.
4. Relay the verdict/findings and the report path. `--background` → relay the `job <id>`
   line and name `claude-code codex status|result|cancel`. Replays: `claude-code ask --log`
   / `ask --retry <id>` still work.
