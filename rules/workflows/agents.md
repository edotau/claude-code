---
paths:
  - "agents/**"
  - "workflows/**"
  - "commands/**"
---
# Agent Authoring and Commands

Roster/dispatch: `CLAUDE.md`; briefs, relay, concurrency, escalation and test agents:
`skills/subagent-workflows/SKILL.md`.

## Commands

- Verbs: `skills/subagent-workflows/references/agent-verbs.md`; ask flags/lifecycle: `cost-routing.md`.
- `claude-code agents sync [--agent a] [--dry-run|--check]` projects agents/instructions; install runs it.
- `agents tree [--watch|--detail|--files]` shows sessions, held/free dirty files and leaf notes.
  `agents send parent <note>` sends a leaf's note on the dispatcher's next prompt.
  `agents watch [--once]` lists stale jobs and next actions; hooks run it automatically.
- `agents label <type> [--profile p] [--model m] --task F` prints the resolved description prefix;
  aliases/full ids work; Databricks labels use full ids. `review scope --range <r> --caps` checks scope.
- Reviews: `/code-review` or `claude-code codex review [--adversarial] [--base <b>]`.
- Cloud execution needs outermost overlay `{"cloud":true}` and the vendor login:
  `claude-code claude review [--base b|--pr N]` runs ultrareview;
  `claude-code codex cloud exec "<q>" --env <id>` submits. Cloud status/list/diff/apply need no opt-in.
  Vendor passthrough refuses ultrareview/cloud exec; use the harness lane.
- `agents list` shows CLI agents, not the subagent roster; JSON exposes capabilities.

## Slash commands

Owner-only (`disable-model-invocation: true`); each launcher names its dispatch tool.

- `/codex`, `/copilot`, `/gemini`, `/opencode` accept host/provider/profile/model/effort/fallback and task.
  Run `agents ask --carry --background --overlay '{"agent":"<type>","tools":true}' - < brief`,
  adding supplied keys, then `agents result -f <id>`. With host, omit carry/background and relay foreground.
  No `/claude` (name collision).
- `/coworker` adds `--codemode on|only`; run a plain background ask (Pi refuses carry) with overlay
  `{"agent":"pi","provider":"databricks","profile":"<p>","autonomous":true,"codemode":"on","tools":true}`,
  then `agents result -f <id>`. Supplied provider/profile replace defaults.
  Its type/receipt is coworker, identical to the Agent type and ask overlay `agent:coworker`; no `/pi`.
- Route fields travel together through one overlay; workflows pass that source unchanged.
  Keys/precedence: `rules/harness/agent.md` → Overlay; auto host/profile: `rules/harness/network.md`.
- `model:auto`, typed or pinned, ranks served models for the brief tier using `internal/assets/catalog/ranks.json`,
  replacing the ladder cell's model over a pin. No candidates means the untyped default plus a note, never refusal;
  `fallback:none` refuses only a concrete typed model. Labels show the pick; model strings contain no profile.
- `/discover-models [--profile p] [--dry-run]` runs `claude-code models discover --apply`, omitting apply for dry-run.
- External agents run via direct carried asks (Pi plain) or Haiku relay types codex/copilot/gemini/opencode/coworker.
  Relays run their baked Databricks overlay, wait and check. Vendor verbs and `runner:own` keep vendor logins.
  Unnamed Claude code leaves run Pi first; typed Claude on Databricks uses the pi runner unless `runner:own`.
  Owner contracts: `skills/subagent-workflows/SKILL.md` → Owner layer; Databricks arm: R20.
  `dispatch-effort` corrects missing/wrong model prefixes, never denies them.

## Agent and command files

- `agents/<name>.md`: flat, one definition, frontmatter name equals stem; description says when to use it.
  Sections as needed: Role, Invocation, Owns, Knows, Verbs, Hands off to, Procedure, Report contract, Don't.
  Knows lists references and when to read them; nothing loads upfront.
- `commands/<name>.md`: short launcher reading `$ARGUMENTS`, running the agent/script and relaying the report.
  Put procedures in agents/skills, skill shell in `skills/<name>/scripts/`, other logic in Go verbs/Make targets.
- Agents propose memory in reports; the main session writes it. Dispatch attaches recall pointers.

## Authoring: file, skill or brief

- Agent = reusable role; skill = method; brief = task. One-off roles are Claude briefs, never files.
- Start skills from `docs/templates/skill.md`; one procedure home, heavy material in references.
- Split for different expertise, tools, authority or reused shapes; merge wording-only variants.
  Routing-only roles become direct carried asks (Pi plain), except the four external workers; saved stages use Claude.
- Use model aliases (`providers.md` → Model selection). Sonnet leaves; unpinned Claude uses tier-based
  haiku/sonnet/opus; Plan inherits; relay types use haiku. Raise a call with Agent `model`.
  File `inherit` means session model with no hook pin; call `model:inherit` is unset. List only needed tools.
- Frontmatter includes effort, maxTurns, isolation:worktree, background, memory; check unknowns with `claude --debug`.
- Register a CLI in `internal/routing/agentid/agentid.go` and its vendor `agent.Register` row; subagent types live in agents.
  Prose instructs, hooks enforce; dispatch-stop reports a running background job without holding the leaf.
  Shapes/enforcers: `skills/subagent-workflows/references/patterns.md`.
- Use adaptive thinking, never manual budgets. Cache stable large prefixes with `cache_control`; keep per-call text out.
