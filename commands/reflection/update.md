---
description: "Session end — save state to the memory bank: summary, active context, progress, decisions, conventions"
argument-hint: "[--transcript <path>] [--cwd <repo>] [--session <id>]"
allowed-tools: Bash, Read, Write, Edit
---

# /reflection:update

> Bank dir: `claude-code memory path`. The `session-harvest` Stop hook runs this in the background
> once a session has ≥3 file edits; run it by hand to save now or to recover a harvest the
> session-start note reports as failed.

In a live session (no `--transcript`): follow `skills/reflection/references/update.md` yourself — you
already hold the session — then settle with `claude-code memory index`.

To recover a failed background harvest, pass the finished session's transcript:

```bash
claude-code memory update $ARGUMENTS
```
