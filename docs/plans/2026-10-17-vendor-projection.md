# Vendor projection: agents/ + skills/ → every agent CLI, git-tracked

Status: IMPLEMENTED 2026-10-17 — `claude-code agents sync` (internal/crossgen: targets.go, render.go, skills.go, sync.go);
`dist/<vendor>/agents/` tracked; verified live on all four CLIs (gemini/codex headless OK, copilot lists 9, opencode debug agent).
Session: edotau.github.io worktree, 2026-10-17. Harness repo is ~/.claude (hard rule 5: no stash/checkout/reset).

## Problem

- Gemini warned "Skill conflict detected" for 7 harness skills: Codex desktop's external-agent import sync
  (`[desktop] external-agent-import-sync-enabled` in ~/.codex/config.toml) copied ~/.claude skills + commands into
  ~/.agents/skills (bannerless, `.claude`→`.Codex` rewrites, `source-command-*` dirs, ~/.codex/agents/*.toml),
  while `claude-code agents sync --gemini` projected the same names into ~/.gemini/skills.
- Done so far: 7 copies moved to ~/.agents/stale-harness-skills-20261017/, sync flag set false
  (backup ~/.codex/config.toml.bak-20261017). Gemini shows 0 conflicts.

## Research results (verified by subagents against installed binaries + bundled docs)

| Vendor | Skills read from | Agents | Symlinks |
| --- | --- | --- | --- |
| gemini 0.64 | ~/.gemini/skills AND ~/.agents/skills (latter wins; conflict compares paths, not realpath) | ~/.gemini/agents/*.md, strict zod schema: name, description, [tools (Gemini names), model, temperature, max_turns, timeout_mins]; body = system prompt | skill dirs followed; agent .md symlinks SKIPPED (isFile) |
| codex 0.160 | ~/.agents/skills, ~/.codex/skills (both listed, no dedup) | ~/.codex/agents/<name>.toml: name, description, developer_instructions, [model, model_reasoning_effort] | skill dirs followed; SKILL.md itself must be a real file |
| opencode 1.18 | ~/.config/opencode/skills, ~/.claude/skills, ~/.agents/skills (recursive, last-wins + warning) | ~/.config/opencode/agents/*.md: description, mode: subagent (new-format keys: model, variant, request, system, description, mode, hidden, color, steps, disabled, permissions) | skills + agents followed |
| copilot 1.0.98 | ~/.copilot/skills, ~/.agents/skills (recursive; ~/.copilot copy wins); NOT ~/.claude/skills | ~/.copilot/agents/*.md (Claude-style frontmatter loads as-is); NOT ~/.claude/agents | followed (dirs + files) |

Codex import item-types table: `{SESSIONS, AGENTS_MD, CONFIG, SKILLS, PLUGINS, MCP_SERVER_CONFIG, SUBAGENTS, HOOKS, COMMANDS, MEMORY}` each bool or "original".

## Design

1. **Skills: one location, zero copies.** `~/.agents/skills/<name>` → symlink to `~/.claude/skills/<name>` for every real,
   non-skip-listed skill (source is already tracked). Every vendor reads ~/.agents/skills. Prune the skill projections
   from ~/.gemini/skills (banner-marked only). Skill assets need no linking anymore.
2. **Agents: tracked rendering + per-vendor install.** Render `dist/<vendor>/agents/<name>.<ext>` in the repo (tracked,
   banner-marked, `--check` gates drift in CI), then install into the vendor home as a real file copy (Gemini skips
   symlinked agent files; copying everywhere is uniform). Gemini agents: `tools` translated via geminiToolNames (render.go), `model` omitted; codex: name/description/developer_instructions/model_reasoning_effort from `effort:`.
   Drop the Gemini agent-as-skill projection (native subagents now exist).
3. **Verb:** `claude-code agents sync [--vendor v] [--dry-run] [--check] [--no-prune]` — `--gemini` removed (clean
   break; fix callers: CLAUDE.md:64, README.md:214/234, Makefile:83 audit target, commands/harness/sync-agents.md,
   internal/cli/complete.go:98, cli.go:78, rules/harness/change-protocol.md:47, rules/standards/README.md:33).
   `claude-code install` runs the sync best-effort after settings so the homes track the repo.
4. **Ownership rules unchanged:** only banner-marked files / symlinks pointing into ~/.claude are ever rewritten or
   pruned; hand-authored entries KEEP. Never RemoveAll.
5. **Cleanup:** move remaining Codex imports out of ~/.agents/skills (approval-gate, synced, 12 source-command-*) and
   the mangled ~/.codex/agents/*.toml into the stale dir; set codex item-types table to SESSIONS=true, rest false,
   then re-enable sync (narrow mechanism) — report to user as reversible.

## Code map

- internal/crossgen: crossgen.go (GeminiSkillsDir), discover.go (DiscoverAgents/DiscoverSkills, skips symlinked skill
  dirs), transform.go (skipNames, banners, projectedNames), sync.go (Sync, writeSkillDir, linkSkillAssets),
  prune.go, docs.go (AGENTS.md/GEMINI.md), frontmatter.go (ParseFrontmatter, buildFrontmatterFile, cleanDescription,
  stripMemorySection). Tests: sync_safety_test, collision_test, banner_and_assets_test, prune_reason_test,
  roster_guard_test, docs_dest_test, helpers_test (isolate() sets HOME + CLAUDE_CONFIG_DIR).
- internal/cli/agent.go cmdAgentsSync; internal/launch/install.go Install(); internal/vendors/vendors.go specs
  (codex, gemini, opencode, copilot) — reuse Names() for the vendor list.
- Agent frontmatter in use: name, description, model, tools, color, effort, memory, maxTurns.
