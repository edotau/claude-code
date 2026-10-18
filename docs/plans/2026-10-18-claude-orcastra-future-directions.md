# claude-orcastra — future directions: VS Code extension vs Go rewrite into the harness (2026-10-18)

Draft for discussion. Effort is a rough estimate in focused days (you + agents), not a commitment.

## What you actually use from Orca (the parity target)

From this week's keep/strip decisions:

| Feature | Notes |
| --- | --- |
| Terminals with splits, surviving app restarts | Orca's daemon keeps PTYs alive across relaunch |
| Worktrees (git + folder workspaces), one per task | Core of the Orca model |
| Agent launch + live status (working / blocked / idle) | Hook server → one status store → sidebar |
| Agents: claude, codex, opencode, gemini, copilot (+ grok, hermes, cursor, kimi, antigravity, qwen-code, pi, muse) | First five are already harness runners |
| Editor: Monokai, files open left of terminal, explorer in sidebar | Every fork commit here re-creates VS Code behaviour |
| Native chat GUI, agent session history/resume | |
| Embedded browser | |
| SSH remotes, WSL | |
| GitHub PR/issue UI, Jira, Bitbucket | |
| Mobile pairing | Phone sees/drives sessions |
| Computer use, dictation | "Review later" |

Orca today: 702 → 661 MB app, ~1.9M lines of TS upstream, deeply entangled (deleting 12 agents broke ~800 types).

## Option A — VS Code extension

VS Code already covers most of the list natively or via extensions you have installed:

| Need | Covered by |
| --- | --- |
| Editor, explorer, split layout, themes | VS Code itself (your fork commits become settings) |
| Terminals + splits | Integrated terminal (persistence across reload: `terminal.integrated.enablePersistentSessions`, weaker than Orca's daemon; tmux closes the gap) |
| SSH / WSL | Remote-SSH, Remote-WSL (better than Orca's) |
| GitHub PRs, Jira, Bitbucket | GitHub Pull Requests, Atlassian extensions; GitLens already installed |
| Claude / Codex / Copilot chat GUI | anthropic.claude-code, openai codex, Copilot (installed) |
| Browser | Simple Browser / Live Preview (weaker than Orca's) |
| Phone access | Remote Tunnels / vscode.dev (no Orca-style agent dashboard) |
| Dictation | VS Code Speech |

**You'd build (the actual Orca value):**
1. Worktree manager view: create worktree (or folder) → open terminal(s) → launch chosen agent in it.
2. Agent status view: per-worktree working/blocked/idle badges, fed by agent hooks.
3. Session history search/resume across agents.

| | |
| --- | --- |
| Effort | MVP (1+2) **~5–8 days**, ~2–4K lines TS; 3 adds ~3–5 days |
| Benefits | No 661 MB app to build/sign/update; VS Code updates are free; Remote-SSH/WSL for free; your layout prefs are just settings; tiny codebase |
| Costs / risks | Lose the terminal-first single-window layout and Orca's restart-proof terminals; no mobile agent dashboard; extension API limits (no custom terminal chrome, webviews for anything rich); still TypeScript |

## Option B — Go rewrite into the claude-code harness

The harness already has: provider registry + router, agent runners for claude/codex/gemini/opencode/copilot, 17
hooks, transcript + memory packages, a `tui` package, status line, tailscale, VS Code settings sync (~31K lines Go).

**You'd build:**
1. `claude-code worktree new|list|rm|open` (git + folder), JSON output.
2. Agent status store fed by the existing hooks (`working|blocked|idle`, per worktree) + `claude-code agents status`.
3. PTY daemon so sessions survive UI restarts (creack/pty; ConPTY on Windows), or delegate to tmux/zellij.
4. A UI, one of:
   - **TUI** (bubbletea) over tmux panes: **~10–15 days**.
   - **Web UI** served by the harness (xterm.js + small JS) over tailscale, which also replaces mobile pairing:
     **~15–25 days**.
   - **Desktop** (Wails): Electron-class effort again; not recommended.
5. Session history search/resume on top of `internal/transcript`: ~3–5 days.

| | |
| --- | --- |
| Effort | Engine (1–3, 5) **~10–15 days**; full replacement with a UI **~25–40 days** |
| Benefits | One small Go binary, cross-platform, same codebase as your harness; SSH/tailnet-native; phone access via the web UI; no upstream to fight |
| Costs / risks | Rebuilding GUI pieces (native chat, PR UI, browser) mostly means dropping them; terminal UI is the hard part; you own all of it |

## Recommendation (revised twice) — plain VS Code + settings first; build only the two missing pieces

What the user actually needs from Orca (2026-10-18, with screenshot of the tab `+` menu):
1. One `+` menu per panel to open a terminal or any agent TUI (Claude, Codex, OpenCode, Gemini, Copilot) as a tab,
   split beside a VS Code-style editor, with an explorer to open/close files.
2. Seeing all subagents of one workflow, especially across multiple repos.
3. Remote dispatch of new agents (mostly solved by tailscale; worth extending).
4. Mobile: the Claude app covers it except a shell — waiting on Anthropic; Termius over tailscale meanwhile.
Worktrees, browser, PR UI, native chat etc. are not needed.

| Need | Solution | Effort |
| --- | --- | --- |
| 1 | VS Code terminal profiles per agent (icon + keybinding, appear in the terminal `+` dropdown) and `terminal.integrated.defaultLocation: "editor"` so terminals/agent TUIs are editor tabs splittable beside files. Ship via the harness's tracked VS Code settings (`claude-code vscode`). | 0.5 day, settings only |
| 2 | Harness: `claude-code agents tree [--format json]` — session → subagents → status/tokens/last tool across repos, from hooks + `internal/transcript`. View: small VS Code tree-view extension, or a page the harness serves (phone-reachable). | 4–7 days (Go + ~500 lines TS) |
| 3 | Harness: `claude-code dispatch --host <tailnet machine> [--repo] <agent> "<task>"` → launches in tmux on that box, returns an attach command; listed by #2. | 3–5 days (Go) |

Plan: (A) configure VS Code as the Orca replacement and use it for a week, keeping slim Orca as fallback;
(B) build #2; (C) build #3. Go `agentd` + its own web UI (previous revision) is the fallback only if A falls short.
