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

## Agent Dashboard

A status bar segment (right side) and an "Agent Dashboard" view above "Agents" show
`claude-code usage` at a glance — most importantly, the Claude 5-hour window's used percentage and
time until it resets. Click the status bar segment (or run "Claude Code Agents: Show Agent
Dashboard") to open the view; "Claude Code Agents: Refresh Agent Dashboard" re-fetches on demand.
Data comes from `claude-code usage --format json` and `claude-code profile --format json`,
refreshed every `claudeCodeAgents.usageRefreshSeconds` (default `60`, floored to `15`); reset
countdowns tick every minute between fetches by re-rendering the cached JSON, no refetch needed.
`claudeCodeAgents.usageShow` picks `used` (default) or `left` percentages.
`claudeCodeAgents.usageStatusBarAlignment` picks `left` (default) or `right`; `claudeCodeAgents.usageStatusBarPriority`
(default `1000000`, placing it second from the left, right after VS Code's remote/host indicator — a smaller
number moves it further right) sets its sort order within that side; both take effect immediately via
`onDidChangeConfiguration`, no reload needed.

**Billing flip**: when `usage`'s `billing.mode` is `"subscription"` (or absent), the status bar
and view show quota percentages with reset countdowns per provider/window (Claude 5h/weekly/Fable,
Antigravity's Gemini/Claude-and-GPT groups, …). When it is `"api"`, they instead show the API-key
token ledger (`api[]`: provider, model, tokens today/last 30 days/all time) with "today" as the
headline — no percentages.

**Grouped windows**: a provider whose windows carry two or more distinct `group` values (Antigravity's
Gemini/Claude-and-GPT/… groups) renders as an aligned grid in Detailed mode instead of a zig-zag row
list — one row per group, one column per distinct window (`5h`, `wk`, …) in order of first
appearance, bars and percentages right-aligned in fixed-width cells so every bar and number lines up
vertically; a group missing a window renders a muted "—" in that cell. Providers with at most one
group (Claude, GitHub Copilot) keep one row per window but share the same fixed-width bar/pct cells.
Compact mode is unchanged either way.

**Agents**: when `usage`'s `agents[]` is present (newer `claude-code`), an "Agents" section lists
Claude/Codex/OpenCode/Copilot CLI token totals, sessions, turns and cost, scoped by
`claudeCodeAgents.usageDays` (default `1`, passed as `--days`). The status bar also appends a
compact `· codex 25k · opencode 321k · copilot 1.7M` summary (Claude is excluded — the quota chips
already cover it) when `claudeCodeAgents.usageStatusBarAgents` is `true` (default). Older
`claude-code` builds without `agents[]` render exactly as before.

**Editor tab**: `claudeCodeAgents.usageOpenIn` (`sidebar` default, or `editor`) picks what "Show
Agent Dashboard" opens — the sidebar view, or a single reusable editor-area tab rendering the same
dashboard (same data, same Refresh/Detailed/Compact/accounts handlers). "Claude Code Agents: Open
Agent Dashboard in Editor" always opens that tab regardless of the setting, and is also available as
a `$(pulse)` button in every editor's title bar (top-right) when `claudeCodeAgents.usageEditorTitleButton`
is `true` (default); the same button sits in the Agent Dashboard view's own title bar next to Refresh.

## Tests

`node --test <dir>` fails to discover files on this box (mise node 24); use a glob instead:

```bash
node --test 'vscode/claude-code-agents/test/**/*.test.js'
```
