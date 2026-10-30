# CLAUDE.md

Injected into **every** session's system prompt — a map of the harness, not an exposition.
Depth lives elsewhere: `README.md` (infrastructure), `rules/` (gotchas and corrected behavior), skills (how-to).

## What this repo is

This git repo **is** `~/.claude` (github.com/edotau/claude-code): the `claude-code` Go binary
(provider registry, loopback model proxy, Anthropic⇄OpenAI translation, agent launchers, hooks,
status line) plus the agents, commands, rules, skills and workflows every Claude Code session loads.
Files under `agents/`, `commands/`, `rules/`, `skills/` and this file go live the moment they are saved.

## Where things live

| Path | Holds |
| --- | --- |
| `cmd/claude-code/`, `internal/` | The binary, stdlib-first; external SDKs boxed one package each. `internal/cli` is the verb table only; each verb's engine is its own package |
| `internal/hookspec/hookspec.go` · `internal/hooks/` | Hook declarations (the one authoring surface) · their handlers |
| `internal/providers/defaults.json` | Built-in providers; the vault's provider overlay replaces them by name |
| `defaults/claude/settings.json` | Source of `settings.json` (generated — hard rule 1) |
| `state/vault.db` | The vault (untracked): pins, custom env, provider/agent overlays (`vault list\|edit`), sealed secrets (`vault set\|get`) |
| `agents/<name>.md` | Subagents, flat; `name:` **must equal the file stem** |
| `commands/<command>.md` | Slash commands, flat — thin launchers into an agent or skill |
| `workflows/*.js` | Workflow tool scripts (`skills/subagent-workflows/references/saved-workflows.md`) |
| `rules/` | `standards/` (always-on) · `workflow/` · `harness/` · `software/<lang>/`; scoping contract → `rules/README.md` |
| `skills/<name>/SKILL.md` | On-demand references. Untracked skill dirs are the user's own (symlinks into `~/.agents`) — leave them. `skills/synced/` = claude.ai account skills, re-downloaded each sync; edit them on claude.ai |
| `plans/` | Implementation plans (native `plansDirectory`), gitignored: this repo's at the top, others' under `plans/<repo-slug>/` |
| `memory/` | The one memory store, a local-only git repo. `memory/index.md` = generated catalog (never hand-edit); `memory/<repo>/` = native auto-memory + six typed bank files (harness = `memory/claude-code/`, home = `memory/home/`); `projects/<slug>/memory` symlinks into it. Engine `internal/memory` |
| `~/worktrees/<repo>/<slug>` | Linked worktrees (shared with Orca) |

## Hard rules (always on)

1. **`settings.json` is generated — never hand-edit it.** Edit `defaults/claude/settings.json`
   (hooks: `internal/hookspec/hookspec.go`), then `make install`. Only `settings.local.json` is hand-edited.
2. **Secrets live only in the encrypted vault (`state/vault.db`; `claude-code vault set`) or a provider's
   `auth.command`** — never in a plain file, `settings.json`, a commit, or a log line. The vault's master key
   comes from the OS keyring, `HARNESS_VAULT_KEY`, or `key_command`.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never `git stash` / `checkout` / `reset` / `restore` in the live checkout (`~/.claude`).** It is
   one checkout shared by parallel subagents and every live session. Default is no worktree; when to ask, the
   commands and their gotchas: `rules/standards/worktree-git.md`.
5. **Done means pushed to `origin/main`** — merge, `make install`, push (the pre-push gate runs `make audit`).

## Behavioral guidelines

1. **Clarify assumptions.** State assumptions; surface multiple interpretations instead of picking
   silently; name a simpler approach if one exists.
2. **Surgical changes.** Touch only what the request requires; match existing style; don't refactor
   adjacent code. Remove only orphans your change created; mention pre-existing dead code.
3. **Goal-driven execution.** Turn tasks into verifiable goals ("fix the bug" → "reproduce it, fix,
   re-run"); loop until verified. Depth: `rules/standards/principles.md`.
4. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
5. **Parallel by default; keep the main thread free.** Plan and brief, then dispatch `code-worker` ‖ `copilot`
   in one message in the background (`copilot` only when the change needs a test agent); inline only for a
   one-line edit; never re-steer a running agent. A leaf does the work or requests one agent via `Agent:` →
   `skills/subagent-workflows/SKILL.md` → Leaf types; many leaves → one lane orchestrator per lane.
6. **Tag notable moments** in replies, one per line: `DECISION:` (what, rejected alt, why) · `BUG:` · `NEXT:` ·
   `NOTE:` — the memory update files them.

Simplicity, comments, verification order and testing: `rules/standards/` (always loaded).

## How-to — by area

One command to reach for first per area; the area file holds the rest of its commands and gotchas (and
auto-loads while you work in that code). Anything unlisted: `claude-code describe`.

| Area | First command | Commands + gotchas |
| --- | --- | --- |
| Install, settings, hooks | `make install` (`claude-code install --dry-run` previews) | `rules/harness/settings-management.md` · `rules/harness/hooks.md` |
| Providers, models, credentials, proxy | `claude-code doctor` · `claude-code vault set [NAME]` | `rules/harness/providers.md` |
| Agents and dispatch | `claude-code ask --agent <agent> "<task>"` · `/code-workers` ‖ `copilot` | `rules/workflow/agents.md` · `rules/workflow/master-workflow.md` |
| Memory | `claude-code memory recall <terms>` | `rules/workflow/memory-bank.md` |
| Git and worktrees | `claude-code worktree create\|merge <name>` | `rules/standards/worktree-git.md` |
| Network, web, MCP, cleanup | `claude-code network mcp` · `claude-code network websearch --query "<q>"` | `rules/harness/network.md` |
| Test, lint, review | `go test ./internal/<pkg>` · `make lint` · `/code-review` | `rules/standards/quality.md` |
