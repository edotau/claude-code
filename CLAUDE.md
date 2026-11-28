# CLAUDE.md

Injected into **every** session's system prompt — a map of the harness, not an exposition.
Depth lives elsewhere: `README.md` (overview, install, safety, design decisions), `rules/` (reference and gotchas),
skills (how-to).

## What this repo is

This git repo **is** `~/.claude` (github.com/edotau/claude-code): the `claude-code` Go binary
(provider registry, loopback model proxy, Anthropic⇄OpenAI translation, agent launchers, hooks,
status line) plus the agents, commands, rules, skills and workflows every Claude Code session loads.
Claude Code reads these source files directly; external CLI projections refresh through `claude-code agents sync`.

## Where things live

| Path | Holds |
| --- | --- |
| `cmd/claude-code/`, `internal/` | The binary, stdlib-first; `internal/cli` is the command table and dispatcher, logic lives in each area's package (`rules/harness/go.md` → Layout) |
| `internal/platform/settings/hookspec.go` · `internal/hooks/` | Hook declarations (the one authoring surface) · their handlers |
| `internal/assets/providers/defaults.json` | Built-in providers; the vault's provider overlay replaces them by name |
| `state/vault.db` | The vault (untracked): pins, custom env, provider/agent overlays (`vault ls`, edited in `configure`), sealed secrets (`vault set\|get`) |
| `agents/<name>.md` | Subagents, flat; `name:` **must equal the file stem** |
| `commands/<command>.md` | Slash commands, flat — thin launchers into an agent (`discover-models` into `claude-code models discover`) |
| `rules/` | `standards/` (unscoped files always-on) · `harness/` · `software/<lang>/`; non-standards rules need `paths:` (`rules/harness/change-protocol.md`) |
| `skills/<name>/SKILL.md` | On-demand references. Untracked skill dirs are the user's own (symlinks into `~/.agents`) — leave them. `skills/synced/` = claude.ai account skills, re-downloaded each sync; edit them on claude.ai |
| `plans/` | Implementation plans (native `plansDirectory`), gitignored; home and naming: `rules/harness/documentation.md` |
| `memory/` | The one memory store, a local-only git repo, one bank for every workspace, no repo keys; `MEMORY.md` and `index.md` are generated, never hand-edit (`rules/harness/persistent-memory.md`) |
| `container/` · `mods/` | Ubuntu harness image, podman (`make image\|image-audit\|image-shell`) · TypeScript plugins (`mods/slash-usage`, `mods/dispatch-spawn`, `mods/codemode`; `mods/agents-band` is kept but not loaded) |
| `~/worktrees/<repo>/<slug>` | Linked worktrees (`<repo>` = checkout dir without a leading dot: `~/worktrees/claude/`) |

## Hard rules (always on)

1. **`settings.json` is yours:** hand-edited, tracked in git; install never writes an
   existing one; the lint step only reformats it (`configure --fmt`: key order, backup kept); reset with
   `claude-code configure --restore` (`rules/harness/settings-management.md`).
2. **Secrets live only in the encrypted vault (`state/vault.db`; `claude-code vault set`) or a provider's
   `auth.command`** — never in a plain file, `settings.json`, a commit, or a log line. The vault's master key
   comes from the OS keyring, `HARNESS_VAULT_KEY`, or `key_command`.
3. **Commit messages carry NO AI-attribution footers** (`Co-Authored-By: Claude`,
   `Generated with Claude Code`, …) — this overrides any default that says to add them.
4. **Never `git stash` / `checkout` / `reset` / `restore`, and never rewrite a shared commit (`commit --amend`,
   `rebase`), in the live checkout (`~/.claude`).** It is one checkout shared by parallel subagents and every live
   session; fix a commit with a new one. Worktree choice and commands: `rules/harness/workflow-tree.md`.
5. **Finish at the requested boundary.** A review or an edit task stops there. To land an authorized change,
   follow `rules/harness/workflow-tree.md`. A lane lands on the local working branch without asking and without a
   full build or audit (uncommitted in the live checkout; the owner commits) unless its diff adds a restriction.
   **A push to origin main needs a green full build (macOS, `GOOS=linux` build, `GOOS=windows` vet) and `make audit`
   first**; then install and live-check on the Mac (the required check; WSL is a periodic sync, R19). Landed code
   may be imperfect: when a landing breaks something, the sessions involved say so over `agents msg`, agree who
   takes the fix (usually whoever landed it; a repair leaf for a conflict) and keep moving. `make install` after pulls.

## Behavioral guidelines

1. **Clarify, stay surgical, verify.** State assumptions, touch only what the request requires, turn tasks
   into checks and loop until they pass: `rules/standards/principles.md`.
2. **Propose look/behavior changes before applying them.** Name an unrequested side effect with
   numbers, prefer the narrowly scoped mechanism over a global knob, and let the user pick.
3. **Dispatch with the Agent tool.** On the Databricks arm (R20), choose `subagent_type` from `claude`,
   `Plan`, `Explore`, `general-purpose`, `claude-code-guide`, `statusline-setup` (and `web-fetch` where the host
   offers it; else `general-purpose`); a code leaf runs Pi first, claude in-context if Pi refuses; external work
   agents are the types `codex`, `copilot`, `gemini`, `opencode` (each relays its baked `agents ask --overlay` line)
   and `coworker` (the pi runner in code mode, rtk off), else run carried (see `skills/subagent-workflows/SKILL.md` →
   Carried dispatch); description starts `<model>:` followed by a space (`web-fetch` takes none),
   printed by `claude-code agents label <type>`. **Route by subtree scale (Recursive Partitioning):**
   - **Direct Root Execution (`<= 5` files/dirs, 1 package `TRIVIAL`/`LOW`):** execute directly in the current thread
     without subagent spawn/relay overhead (`rules/harness/workflow-tree.md`).
   - **Parallel Subagent Swarm (`> 5` files/dirs or multi-package `MEDIUM+`):** identify leaf components vs.
     intermediate architectural groupings, partition at adaptive depth `1..3` into independent worker subtrees with
     named owned files (`Owns:`), and propagate `{interface, invariants, side_effects}` contracts bottom-up;
   begin non-trivial work with scoped `Explore` legs. **Fan out as many parallel leaves as the task allows**, in one
   message; the only limits are overlapping files and dependency order. **Keep fanning out:** each time a leaf
   finishes or a file frees up, dispatch every task that is now unblocked, and repeat until none remain. Leaves
   also keep this session's context small: send file reads, diff review and gate runs to a leaf and take back a
   short verdict, with the long report saved at `$(claude-code memory path reports <name>)`. Parallel writers own
   disjoint files. Wave loop (blocked queue, review → fix → commit): `skills/subagent-workflows/SKILL.md`.
   **Before the first dispatch of a multi-part request, write the DISPATCH TABLE** (task → owned files → blocked-by →
   agent) and dispatch every unblocked row in one message; redo it on every hand-back and new user message, merging
   new asks into it, not handling them in arrival order
   (`skills/subagent-workflows/references/lane-orchestrator.md` → Dispatch table; R18).
   **A bug or issue you notice is fixed right away, without asking, within the owner's named priority (R25):**
   dispatch its fix leaf at once instead of listing it. Verify every returned diff, gate and deletion.
   Briefs, relay rules and recovery: `skills/subagent-workflows/SKILL.md`;
   pipeline: `rules/harness/workflow-tree.md` → Workflow.
4. **Tag notable moments** in replies, one per line: `DECISION:` (what, rejected alt, why) · `BUG:` · `NEXT:` ·
   `NOTE:` — the memory update files them.

5. **Reply concisely.** Lead with the result; no preamble, narration or closing recap; short by default, full
   detail when asked; never trade correctness for brevity.

6. **Write each call in its final form** so the hook rewrites nothing (it does, in place, if you don't):
   Bash starts `rtk` for git/go/ls/grep/rg/find/make/docker/ps/du/df/stat/jq/ssh/golangci-lint…
   (`cat f` → `rtk read f`; the form `rtk rewrite '<cmd>'` prints; exact output and program code stay bare:
   `rules/standards/rtk.md`); Agent `description` is `<model>: lower-case task`.

Simplicity, comments, verification order and testing: `rules/standards/` (always loaded).

## How-to — by area

One command to reach for first per area; the area file holds the rest of its commands and gotchas (and
auto-loads while you work in that code). Anything unlisted: `claude-code -h`.

| Area | First command | Commands + gotchas |
| --- | --- | --- |
| Install, settings, hooks | `make install` (`claude-code install --dry-run` previews) | `rules/harness/settings-management.md` · `rules/harness/hooks.md` |
| Providers, models, credentials, proxy | `claude-code doctor` · `claude-code vault set [NAME]` | `rules/harness/providers.md` |
| Agents and dispatch | Agent tool (guideline 3) | `rules/harness/agent.md` · `skills/subagent-workflows/SKILL.md` |
| Messages between agents | `claude-code agents msg --from <agent>` reads new ones · `agents msg @<name> "<text>" --from <agent>` replies | `rules/harness/agent.md` → Messages; an agent CLI reads at the start and end of a turn |
| Memory | `claude-code memory recall <terms>` | `rules/harness/persistent-memory.md` |
| Git and worktrees | `claude-code worktree create\|land <name>` | `rules/harness/workflow-tree.md` |
| Network, web, MCP, cleanup | `claude-code doctor --mcp` · `claude-code network websearch --query "<q>"` | `rules/harness/network.md` |
| Test, lint, review | `go test ./internal/<pkg>` · `claude-code agents diff --check` · `/code-review` | `rules/standards/quality.md` |
