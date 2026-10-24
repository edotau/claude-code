# Claude Code Agents Dashboard

Dispatch remote work over SSH/tmux and watch per-agent-CLI token usage from the VS Code activity bar.

Runs `claude-code dispatch`, `claude-code usage --format json` and `claude-code profile --format json`
via `child_process.execFile` — no shell, no npm dependencies. On Remote-SSH it runs on the remote host
(workspace extension kind), so usage always reflects the box the binary is actually on.

## Settings

- `claudeCodeAgents.binary` — path or name of the `claude-code` binary (default `claude-code`).
- `claudeCodeAgents.hosts` — SSH hosts offered by the Dispatch… quick-pick (default `[]`).

## Commands

Dispatch… (quick-pick host/agent + task input, "Enter custom host…" for an ad hoc SSH target; starts
the task and offers to copy the `ssh -t host tmux attach -t <session>` command), all under "Claude Code
Agents" in the command palette.

## Agents Dashboard

A status bar segment (right side) and an "Agents Dashboard" activity-bar view show
`claude-code usage` at a glance — most importantly, the Claude 5-hour window's used percentage and
time until it resets. Click the status bar segment (or run "Claude Code Agents: Show Agent
Dashboard") to open the view; "Claude Code Agents: Refresh Agents Dashboard" re-fetches on demand.
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

**Stats layer**: in Detailed mode, above the providers list, a tile row (Total tokens, Est. cost,
Sessions, Active days) summarizes `claude-code usage`'s `window_days` window, followed by a
daily-intensity heatmap (one square per day, 7 rows of weekdays × N week columns, 5 opacity steps,
best day called out) and a stacked token-mix bar (input/output/cache read/cache write). Est. cost
sums each agent's own `cost_usd` where the CLI reports it (OpenCode) and otherwise estimates from
Anthropic's published per-model rates (`lib/pricing.js`); a model it can't price (non-Anthropic, or
an unrecognized Anthropic tier) marks the total "(partial)" rather than silently underestimating it.
Older `claude-code` builds without `window_days`/`mix`/`daily`/`active_days` render the tile row with
whatever they can (sessions, turns, cost) and an empty heatmap/mix. Compact mode hides this layer.

**Agents**: when `usage`'s `agents[]` is present (newer `claude-code`), an "Agents" section renders
one card per agent — Claude/Codex/OpenCode/Copilot CLI — in a responsive grid (2-4 across in a wide
editor tab, stacked in the narrow sidebar): name, provider/model, a "tokens · sessions · turns · $cost"
line (omitting whatever the CLI didn't report), and a thin bar showing that agent's share of the
total tokens across all agents. An agent with no sessions in the window renders as a muted card. The
window is scoped by `claudeCodeAgents.usageDays` (default `30`, passed as `--days`). The status bar
also appends a compact `· codex 25k · opencode 321k · copilot 1.7M` summary (Claude is excluded — the
quota chips already cover it) when `claudeCodeAgents.usageStatusBarAgents` is `true` (default). Older
`claude-code` builds without `agents[]` render exactly as before.

**Editor tab**: `claudeCodeAgents.usageOpenIn` (`sidebar` default, or `editor`) picks what "Show
Agents Dashboard" opens — the sidebar view, or a single reusable editor-area tab rendering the same
dashboard (same data, same Refresh/Detailed/Compact/accounts handlers). "Claude Code Agents: Open
Agents Dashboard in Editor" always opens that tab regardless of the setting, and is also available as
a `$(pulse)` button in every editor's title bar (top-right) when `claudeCodeAgents.usageEditorTitleButton`
is `true` (default); the same button sits in the Agents Dashboard view's own title bar next to Refresh.

## Phone access

"Claude Code Agents: Serve for Phone (Tailscale)" runs `claude-code vscode serve` in a terminal
named "Agents Dashboard", publishing this VS Code's web UI to your tailnet (or the public internet
with `--funnel`) with a token-gated URL and a terminal QR code; "Stop Serving for Phone" runs
`claude-code vscode serve --stop`.

## Tests

`node --test <dir>` fails to discover files on this box (mise node 24); use a glob instead:

```bash
node --test 'vscode/claude-code-agents/test/**/*.test.js'
```
