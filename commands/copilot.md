---
description: "Run GitHub Copilot: a verb (ask, review, status…) or a bare task as ask"
argument-hint: "[profile=<p>] [model=<m>] [ask|resume|review|explore|transfer|status|…] <task>"
disable-model-invocation: true
allowed-tools: Bash(claude-code copilot *), Bash(claude-code ask --agent copilot *)
---

# /copilot

Launcher only. Verbs and flags: `claude-code copilot -h`; the agent's role and report contract: `agents/copilot.md`.

1. Strip `profile=<p>` and `model=<m>` from `$ARGUMENTS`; they become `--profile <p>` / `--model <m>`.
2. First remaining word is a verb → run `claude-code copilot <verb> [flags] <rest>`.
   Verbs: `ask resume review explore transfer status tail result cancel setup catalog profile on off`.
3. Anything else is a task → run `claude-code ask --agent copilot [flags] "<task>"`.

Pass arguments as literal CLI arguments, never shell-evaluated. Long tasks: add `--background` and relay the job id
(`/copilot status`, `/copilot result <id>`). Relay the output as-is;
a verb that rejects a flag errors — relay that, don't retry.
