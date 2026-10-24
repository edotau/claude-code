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

## Tasks

The Tasks page is one webview (rendered by `lib/tasks-view.js`) that reproduces Orca's Task page for three sources:
**Jira Data Center** (the default — "Assigned to me"), **GitHub** and **Bitbucket Data Center**. It is contributed in
two places backed by the same provider: a bottom **Panel** tab "Tasks" (`claudeCodeTasks.panel`, next to
Terminal/Problems) and an activity-bar container "Tasks" (`claudeCodeTasks.page`); hide whichever you don't use
(right-click its icon — VS Code remembers). "Claude Code Tasks: Show Tasks" and the status bar segment open the one
`claudeCodeAgents.tasksOpenIn` names (`panel`, default; `sidebar`; `editor`). "Claude Code Tasks: Open Tasks in Editor"
(palette only — or drag the Tasks tab from the Panel into the editor area) opens the page as an editor tab, and "Open
Tasks in New Window" (the one view-title button, `$(link-external)`) pops it out into a floating VS Code window (the
editor tab moved with "Move Editor into New Window"). The activity-bar view is a **launcher** (source list with status dots, "Open Tasks in new window",
"Open in Panel", the exit node): clicking the Tasks icon pops the page out into a window, or reveals it where it already
lives. VS Code decides the floating window's size and remembers the last size you gave it; the page fills whatever it
gets (board columns stretch to the width). Tasks is live in one place at a time: the Panel/sidebar copy
closes while the tab or window owns it and shows "Bring back here", and closing the tab/window returns it. No Go: the harness only supplies the Bitbucket and Jira
credentials (`<binary> secrets get BITBUCKET_TOKEN` / `JIRA_TOKEN` via `execFile`, then the `BITBUCKET_TOKEN` /
`JIRA_TOKEN` (or `ATLASSIAN_TOKEN`) env vars, then the token files), cached for the session and re-resolved once on a
401/403.

Chrome, top to bottom: a source bar (one square button per visible source), an availability banner
(not signed in / no token, with a Sign in / Set token action that runs `gh auth login` or
`claude-code secrets set BITBUCKET_TOKEN|JIRA_TOKEN` in the shared terminal), a repo multi-select (GitHub), preset
chips (click applies; **right-click makes it the default** for that source), type/state dropdowns, the
query input (Enter applies, X clears), "New issue" (GitHub) and Refresh. The list is a sticky-header grid
with sticky ID and Title columns under horizontal scroll — issues: ID · Title · Assignees · Status ·
Updated · Actions; pull requests (query scope `is:pr`, Bitbucket, or every row a PR): ID · Title ·
Reviewers · Checks · Merge · Updated · Actions; Jira: Key · Title · Assignee · Status · Priority · Updated ·
Actions. Loading shows 12 shimmer rows; errors show a banner
("N of M repos failed", a hard-error banner with Retry) plus per-repo error rows. A pagination bar
appears below the list only when there is more than one page; jumping to page *k* loads only page *k*
(36 rows per repo per page, 100 across repos — `claudeCodeAgents.tasksPageSize`). Clicking a row, the
Checks cell, the Merge pill or a Jira chip opens the URL in the browser; "Start" copies
`Work on PR #N "title" (url)` to the clipboard and opens the shared terminal running `<binary> claude`.
Mutations (merge, assign, review) stay in the GitHub Pull Requests extension / the browser.

The query language is Orca's (`lib/task-query.js`): `is:issue` / `is:pr`, `is:open|closed|merged`,
`state:…`, `is:draft`, `assignee:`, `author:`, `review-requested:`, `reviewed-by:`, `label:` (repeatable),
quoted phrases and unknown tokens pass through as free text.

- **GitHub** (`lib/tasks-github.js`) reads through the `gh` CLI (`gh pr list --search --json …` for PRs,
  `gh api search/issues` for issues, one count call per kind for the page total) with your `gh auth`
  login — no token is stored. Repos: every workspace folder's github.com remote plus
  `claudeCodeAgents.tasksGitHubRepos` (`owner/name`); the selection persists per workspace. Presets:
  Assigned to me, Created by me, Review requested, My PRs. Checks come from `statusCheckRollup`; Merge
  readiness from `mergeable` + `mergeStateStatus`.
- **Bitbucket Data Center** (`lib/tasks-bitbucket.js` over `lib/bitbucket-client.js`): `review-requested:@me`
  → the dashboard as REVIEWER, `author:@me` → as AUTHOR, anything else → the open PRs of the workspace
  folders' Data Center remotes plus `claudeCodeAgents.tasksBitbucketRepos` (`PROJECT/repo`). Only `is:pr`,
  `state:`, `author:`, `review-requested:@me` and free text apply (free text filters client-side). The
  Checks cell shows the folded Jenkins build status and links its URL; Merge is "Open in Bitbucket" (DC
  exposes no mergeability). A Jira key detected from the branch (then the title) renders as a chip linking
  to `claudeCodeAgents.tasksJiraBaseUrl/browse/<KEY>`.
- **Jira Data Center** (`lib/tasks-jira.js` over `lib/jira-client.js`, `/rest/api/2`, Bearer PAT): the query
  becomes JQL — `assignee:@me` → `assignee = currentUser()`, `author:@me` → `reporter = currentUser()`,
  `is:open` → `statusCategory != Done`, `is:closed` → `statusCategory = Done`, `label:` → `labels in (…)`,
  `project:KEY`, `status:Blocked`, a bare `KEY-123`, and other words → `text ~ "…"`; always
  `ORDER BY updated DESC`, paged with `startAt`. `claudeCodeAgents.tasksJiraProjects` scopes it to project
  keys (empty = every project the token can see). Presets: Assigned to me (default), Reported by me, Done by
  me, All open. The Status pill is toned by status category (To Do muted, In Progress primary, Done green);
  the issue-type and priority icons are Jira's own. Clicking a Jira row swaps the list for an **inline detail view**
  (Back returns, scroll position kept): key · project · updated, title, a status pill that opens the transition menu
  (transitions needing a resolution or comment show a small form; ones needing other fields say so and defer to
  Jira), a priority menu, an assignee button (QuickPick: Unassigned · Assign to me · live search), labels, the
  description (Jira's rendered HTML, sanitised), the comments with a composer (Cmd/Ctrl+Enter sends; the draft
  survives re-renders), and a rail: Open in Jira, Copy URL / key / branch name / prompt. Every write is optimistic
  through `lib/tasks-mutations.js` (patch now, writes serialised per issue, refetch after, rollback + an inline
  Retry banner on failure). Bitbucket rows open a header-only detail with "Open in Bitbucket" for now.
- **GitHub detail** (Orca's github-item-dialog): the row click swaps in the conversation — header, the same Status /
  Assignees (issues) or Reviewers / Checks / Merge (PRs) controls as the list, labels, base ← head and `+N −M`; PRs add
  a Conversation · Checks · Files tab strip (Files opens `<pr>/files` in the browser). The body and every comment come
  from GitHub already rendered (`Accept: application/vnd.github.html+json`, then sanitised — no markdown library);
  the activity is the issue timeline (comments, reviews with an approved / changes-requested pill, assigned, labeled,
  closed, merged, renamed, cross-referenced, review requested…; noise events dropped). The composer posts with
  ⌘/Ctrl+Enter through `gh api -X POST …/comments --input -` (the body on stdin, never argv) and appends GitHub's
  returned comment. The Checks tab lists `…/commits/<sha>/check-runs` (failed first) with run links.
- **GitHub cells are live.** The PR list asks `gh` only for cheap fields; Checks, Merge and Reviewers hydrate per row
  through `gh pr view` when the row scrolls near (160 px) or is hovered, four at a time, and the cells are patched in
  place (no page re-render). Status (issues) is a menu: Open · Close as completed · not planned · duplicate… (asks for
  the number); Assignees and Reviewers open a multi-select QuickPick over the repo's assignable users (reviewers capped
  at 15); Merge is a menu with Enable/Disable auto-merge, one item per method the repo allows, and "Open GitHub merge
  box" — merging asks "<Method> PR #N?" first. Writes run `gh issue close|reopen|edit`, `gh pr edit|merge|ready|close`
  through the same optimistic queue; a failure shows under the row's title with Retry. `gh` runs with
  `GH_PROMPT_DISABLED=1`, a 30 s timeout, two retries on 5xx / reset / 429-without-Retry-After, and a per-bucket
  breaker after "API rate limit exceeded" (search 70 s, core 5 min).
- **Board view.** The `List | Board` toggle (per source, remembered in the workspace; `claudeCodeAgents.tasksViewMode`
  sets the start) groups the same page of rows into 272 px columns: Jira by status (To Do · In Progress · Done order,
  "Done" always present), GitHub PRs Draft · Open · Changes requested · Approved · Merged · Closed, issues Open ·
  Closed, Bitbucket Open · Needs work · Approved · Merged · Declined. Dropping a card (native drag and drop, or "Move
  to…" in the card's ⋮ menu) performs the matching action: Jira runs the transition that lands in that column (one →
  apply, several → pick, required resolution/comment → asked first, none → an inline note and the card stays), GitHub
  issues reopen/close, PRs ready/draft/close/reopen/merge (with the confirm); review columns and Bitbucket refuse with a
  note.

The **Task sources** section at the bottom of the page shows one card per source, plus a **Tailscale** card (exit-node
pill; "Exit node…" lists the tailnet's exit nodes from `<binary> tailscale status --format json` in a QuickPick and
switches with `tailscale set --exit-node=<ip> --exit-node-allow-lan-access=true`; "Direct" clears it — VPN-style routing
without any API key; `claudeCodeAgents.tailscaleBinary` names the CLI). It shows one card per source — a Connected /
Disconnected pill (`gh api user` / the DC doctor / Jira `/myself`; the login is in its tooltip), "Show in Tasks" (the
last visible source cannot be hidden), the default source, the repo list — with "Edit repos and defaults in Settings"
and "Re-check connections". Settings: `claudeCodeAgents.tasksDefaultSource` (default `jira`), `tasksVisibleSources`,
`tasksDefaultQuery.jira` / `.github` / `.bitbucket`, `tasksGitHubRepos`, `tasksBitbucketRepos`, `tasksJiraProjects`,
`tasksPageSize`, `bitbucketUrl` (default `https://bitbucket.biscrum.com`), `tasksJiraBaseUrl` (default
`https://jira.biscrum.com`), `tasksRefreshSeconds` (default `120`, polls while the page is visible).

On activation, the extension installs the "GitHub Pull Requests and Issues" extension
(`github.vscode-pull-request-github`, also listed as an `extensionPack` dependency for a
marketplace install) if it isn't already present — it stays the write path for GitHub;
`claudeCodeAgents.tasksInstallGitHubExtension` (default `true`) turns that off. A failed install
(offline, marketplace unreachable) is silent — the Tasks page works without it.

A status bar segment (same side/priority pattern as the usage segment, one slot to its right) shows
the current branch's PR — from Bitbucket when the first workspace folder's remote is on the Data Center
host, from GitHub when it is on github.com — as `#123 ✓ · 1/2` (checks · reviewers approved/total), or
hides itself when the branch has no PR; `claudeCodeAgents.tasksStatusBar` (default `true`) turns it off.
Clicking it runs "Show Tasks".

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
