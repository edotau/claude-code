---
description: "Run Gemini (agy): a verb (ask, review…) or a bare task as ask"
argument-hint: "[profile=<p>] [model=<m>] [ask|resume|review|explore|transfer|…] <task>"
allowed-tools: Bash(claude-code gemini *), Bash(claude-code ask --agent gemini *)
---

# /gemini

Launcher only. Verbs and flags: `claude-code gemini -h`; the agent's role and report contract: `agents/gemini.md`.

1. Strip `profile=<p>` and `model=<m>` from `$ARGUMENTS`; they become `--profile <p>` / `--model <m>`.
2. First remaining word is a verb → run `claude-code gemini <verb> [flags] <rest>`.
   Verbs: `ask resume review explore transfer status tail result cancel setup catalog profile on off`.
3. Anything else is a task → run `claude-code ask --agent gemini [flags] "<task>"`.

Pass arguments as literal CLI arguments, never shell-evaluated. Long tasks: add `--background` and relay the job id
(`/code-worker:gemini status`, `/code-worker:gemini result <id>`). Relay the output as-is;
a verb that rejects a flag errors — relay that, don't retry.
