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
| `cmd/claude-code/`, `internal/` | The binary, stdlib-first; external SDKs boxed one package each. Each verb area owns its package and `internal/cli` is the command table, menu and dispatcher importing only their `Run`/`Spec`. **Every command is `internal/cli/<verb>.go` + `<verb>_test.go`, never more**: flags and output only, its logic in the area's package (`<area>cmd` packages are legacy, being folded in; agent verbs in `internal/agent/agentcmd`, `network` in `internal/network`); engines live below; a package more than one area imports stays top-level (`router`, `ledger`), one area's helper nests under it (`agent/tools`), and a provider's own code stays under that provider (`databricks/oauth`) |
| `internal/settings/hookspec.go` · `internal/hooks/` | Hook declarations (the one authoring surface) · their handlers |
| `internal/providers/defaults.json` | Built-in providers; the vault's provider overlay replaces them by name |
| `state/vault.db` | The vault (untracked): pins, custom env, provider/agent overlays (`vault ls\|edit`), sealed secrets (`vault set\|get`) |
| `agents/<name>.md` | Subagents, flat; `name:` **must equal the file stem** |
| `commands/<command>.md` | Slash commands, flat — thin launchers into an agent or skill |
| `workflows/*.js` | Workflow tool scripts (`skills/subagent-workflows/references/saved-workflows.md`) |
| `rules/` | `standards/` (always-on) · `workflows/` · `harness/` · `software/<lang>/`; scoping contract → `rules/README.md` |
| `skills/<name>/SKILL.md` | On-demand references. Untracked skill dirs are the user's own (symlinks into `~/.agents`) — leave them. `skills/synced/` = claude.ai account skills, re-downloaded each sync; edit them on claude.ai |
| `plans/` | Implementation plans (native `plansDirectory`), gitignored: this repo's at the top, others' under `plans/<repo-slug>/` |
| `memory/` | The one memory store, a local-only git repo. `memory/index.md` = generated catalog (never hand-edit); `memory/<repo>/` = native auto-memory + six typed bank files (harness = `memory/claude-code/`, home = `memory/home/`); `projects/<slug>/memory` symlinks into it. Engine, store location and locking `internal/memory` |
| `~/worktrees/<repo>/<slug>` | Linked worktrees |

## Hard rules (always on)

1. **`settings.json` is yours:** hand-edited, untracked; install never writes an existing one; reset with
   `claude-code configure --restore` (`rules/harness/settings-management.md`).
2. **Secrets live only in the encrypted vault (`state/vault.db`; `claude-code vault set`) or a provider's
   `auth.command`** — never in a plain file, `settings.json`, a commit, or a log line. The vault's master key
   comes from the OS keyring, `HARNESS_VAULT_KEY`, or `key_command`.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never `git stash` / `checkout` / `reset` / `restore`, and never rewrite a shared commit (`commit --amend`,
   `rebase`), in the live checkout (`~/.claude`).** It is one checkout shared by parallel subagents and every live
   session; fix a commit with a new one. Default is no worktree; when to ask, the
   commands and their gotchas: `rules/standards/worktree-git.md`.
5. **Done means pushed to the current branch (`main` by default)** — merge, `make install`, push (no pre-push gate:
   run `make audit` yourself before pushing; CI runs it on `main`).

## Behavioral guidelines

1. **Clarify, stay surgical, verify.** State assumptions, touch only what the request requires, turn tasks
   into checks and loop until they pass: `rules/standards/principles.md`.
2. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
3. **Dispatch, then verify what comes back.** Anything bigger than a one-line edit goes to a leaf whose brief names
   the files it owns: `claude` (in-context default) or an external agent by name (`codex`, `copilot`, `gemini`,
   `opencode`; `Model:`/`Profile:` lines in the brief, experimental).
   Dispatch external agents yourself, by leaf: a plan phase is never a command handed to the owner to run.
   Non-trivial work starts with `Explore` legs; an unrelated mid-task request goes to a background leaf. Run leaves
   in parallel only on disjoint files (cap: the dispatch hook's 24 per account), and check every leaf's claims
   (diff, gate, deletions) before committing. Pipeline:
   `rules/workflows/master-workflow.md`; shapes, briefs, test agent and recovery:
   `skills/subagent-workflows/SKILL.md`.
4. **Tag notable moments** in replies, one per line: `DECISION:` (what, rejected alt, why) · `BUG:` · `NEXT:` ·
   `NOTE:` — the memory update files them.

Simplicity, comments, verification order and testing: `rules/standards/` (always loaded).

## How-to — by area

One command to reach for first per area; the area file holds the rest of its commands and gotchas (and
auto-loads while you work in that code). Anything unlisted: `claude-code -h`.

| Area | First command | Commands + gotchas |
| --- | --- | --- |
| Install, settings, hooks | `make install` (`claude-code install --dry-run` previews) | `rules/harness/settings-management.md` · `rules/harness/hooks.md` |
| Providers, models, credentials, proxy | `claude-code doctor` · `claude-code vault set [NAME]` | `rules/harness/providers.md` |
| Agents and dispatch | `claude-code agents ask --agent <agent> "<task>"` · `/coworker agents=<a,b> <task>` · `claude` ‖ `copilot` (features and bug fixes only: `rules/standards/testing-guidelines.md`) | `rules/workflows/agents.md` · `rules/workflows/master-workflow.md` |
| Memory | `claude-code memory recall <terms>` | `rules/workflows/memory-bank.md` |
| Git and worktrees | `claude-code worktree create\|merge <name>` | `rules/standards/worktree-git.md` |
| Network, web, MCP, cleanup | `claude-code doctor --mcp` · `claude-code network websearch --query "<q>"` | `rules/harness/network.md` |
| Test, lint, review | `go test ./internal/<pkg>` · `make lint` · `/code-review` | `rules/standards/quality.md` |

Open audit findings (2026-10-31; not yet fixed): `memory/claude-code/reports/2026-10-31-wrapup-*.md` and
`2026-10-31-gemini-review-dispatch-system.md` — struct/helper dups, env drift, agent frontmatter `Bash` grants.
