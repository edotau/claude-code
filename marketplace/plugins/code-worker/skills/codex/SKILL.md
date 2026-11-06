---
description: "Run Codex: bare = adversarial diff review saved as a report; a verb (ask, review, status…) passes through"
argument-hint: "[profile=<p>] [model=<m>] [ask|resume|review|explore|transfer|status|…] [args…]"
allowed-tools: Bash(claude-code codex *), Bash(claude-code ask --agent codex *), Bash(claude-code memory path), Bash(date +%F), Bash(mkdir -p *), Bash(tee *)
disable-model-invocation: true
---

# /code-worker:codex

Launcher only. Verbs and flags: `claude-code codex -h`; usage, workflows and report contract: `~/.claude/agents/codex.md`.

1. Strip `profile=<p>` and `model=<m>` from `$ARGUMENTS`; they become `--profile <p>` / `--model <m>`.
2. First remaining word is a verb → run `claude-code codex <verb> [flags] <rest>`.
   Verbs: `ask resume review explore transfer status tail result cancel setup catalog profile on off`.
3. Empty, or anything else → `review --adversarial` over the rest (scope `--uncommitted` default, `--base <b>`,
   `--commit <sha>`, `--pr`; other words = focus), teed to a report; `<scope>` = `uncommitted`, `base-<b>`,
   `commit-<sha>` or `pr`:

   ```bash
   out="$(claude-code memory path)/reports/$(date +%F)-codex-review-<scope>.md"
   mkdir -p "${out%/*}"; claude-code codex review --adversarial <args> | tee "$out"
   ```

   `nothing in scope` (exit 1) → say so and stop.

Pass arguments as literal CLI arguments, never shell-evaluated. Long tasks: add `--background` and relay the job id
(`/code-worker:codex status`, `/code-worker:codex tail <id>`,
`/code-worker:codex result <id>`). Relay the output and any report path as-is;
a verb that rejects a flag errors — relay that, don't retry.
