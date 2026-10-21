# Claude Code Agents

Browse the claude-code harness's live agent tree (sessions, subagents, workflows) from the VS Code
activity bar, and dispatch or attach to remote work over SSH/tmux.

Runs `claude-code agents tree --format json` and `claude-code dispatch --list/--host` via
`child_process.execFile` — no shell, no npm dependencies. On Remote-SSH it runs on the remote host
(workspace extension kind), so the tree always reflects the box the binary is actually on.

## Settings

- `claudeCodeAgents.binary` — path or name of the `claude-code` binary (default `claude-code`).
- `claudeCodeAgents.refreshSeconds` — auto-refresh interval (default `5`).
- `claudeCodeAgents.days` — scope the tree to sessions touched in the last N days (default `1`).
- `claudeCodeAgents.hosts` — SSH hosts to list dispatched tmux sessions on (default `[]`).

## Commands

Refresh, Toggle Show All (`--all`), Dispatch… (quick-pick host/agent + task input), Attach, and
Copy Attach Command, all under the "Agents" view's title bar or a dispatch session's context menu.

## Tests

`node --test <dir>` fails to discover files on this box (mise node 24); use a glob instead:

```bash
node --test 'vscode/claude-code-agents/test/**/*.test.js'
```
