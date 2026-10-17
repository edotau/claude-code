---
description: "Project agents/ into every vendor CLI (codex, copilot, gemini, opencode) via tracked dist/, link skills/ into ~/.agents/skills, regenerate AGENTS.md/GEMINI.md"
argument-hint: "[--vendor <v>] [--dry-run | --check | --no-prune]"
---

# /harness:sync-agents

One roster, every agent CLI. `agents/<name>.md` is rendered into each vendor's native subagent format under the
git-tracked `dist/<vendor>/agents/` and installed byte-identical into the vendor home; the hand-authored
`skills/<name>/` dirs are symlinked into `~/.agents/skills`, the one skills directory all four CLIs read natively.
`AGENTS.md`/`GEMINI.md` are rendered at the config root from the same roster.

## What syncs

| Vendor | Agents land in | Format |
| --- | --- | --- |
| codex | `~/.codex/agents/<name>.toml` | `name`, `description`, `model_reasoning_effort` (from `effort:`), `developer_instructions` |
| copilot | `~/.copilot/agents/<name>.md` | Claude-style frontmatter (`name`, `description`, `tools` passed through) |
| gemini | `~/.gemini/agents/<name>.md` | `name`, `description`, `tools` translated to Gemini names (Read→read_file, Bash→run_shell_command, …; unmappable ones dropped — strict schema) |
| opencode | `~/.config/opencode/agents/<name>.md` | `description`, `mode: subagent` |

Descriptions are cleaned of `<example>` blocks; the `## Memory` section is stripped from the body. Skills are
linked, never copied: `~/.agents/skills/<name>` → `~/.claude/skills/<name>`. Codex, Copilot, Gemini and OpenCode all
read `~/.agents/skills`; Gemini warns on the same name in two tiers, so nothing is written to `~/.gemini/skills` any
more (banner-marked copies an earlier sync left there are pruned).

## What's skipped / kept

- `skipNames` (`internal/crossgen/transform.go`) — Claude-Code-only sources such as `approval-gate`.
- Symlinked dirs under `skills/` — they already point into `~/.agents/skills` (the user's own skills).
- Any bannerless (hand-authored) file in a vendor home, any symlink there, any real dir or foreign symlink in
  `~/.agents/skills` — kept and reported as `KEEP`; delete it to let the harness own that name.

## Pruning

Only banner-marked files whose source is gone (in `dist/` and the homes), harness symlinks in `~/.agents/skills`
whose skill is gone, and legacy banner-marked dirs in `~/.gemini/skills`. Never `RemoveAll`. `--no-prune` skips it;
an empty roster refuses to prune.

## Execution

```bash
claude-code agents sync $ARGUMENTS
claude-code agents docs [--check|--dry-run from $ARGUMENTS]
gemini skills list 2>&1 | grep -c 'Skill conflict'    # expect 0
git -C ~/.claude status --short dist/                  # commit the tracked rendering
```

`agents sync` honours `--vendor`, `--dry-run`, `--check` (exit 1 on drift in `dist/` or a home) and `--no-prune`.
`agents docs` takes only `--check`/`--dry-run`: forward one of those verbatim, drop the others, run bare otherwise.
`claude-code install` runs the sync after settings, so a fresh box tracks the repo. Commit `dist/` with the agent
change that produced it.

Report the output.
