---
paths:
  - "agents/**"
  - "workflows/**"
  - "commands/**"
---
# Agent Authoring and Commands

Dispatch and the allowed roster: `CLAUDE.md` guideline 3. Brief fields, relay, concurrency, escalation and the test
agent: `skills/subagent-workflows/SKILL.md`.

## Commands

- Verb table (`ask`, `status`, `result`, `cancel`, `wait`, `brief`, `diff`, `find`, `dispatch --waves`, codex/gemini/
  opencode/copilot verbs): `skills/subagent-workflows/references/agent-verbs.md`. Ask flags, `--live`, `--host`,
  stream-json: `skills/subagent-workflows/references/cost-routing.md` → Ask flags and lifecycle.
- Not in those: `claude-code agents sync [--agent a] [--dry-run|--check]` projects agents and CLAUDE.md into every
  agent CLI (`install` runs it); `agents tree [--watch|--finished]`; `agents label <type> --task F` prints the
  description prefix; `review scope --range <r> --caps`.
- Read an exit code, not output: `agents wait`, `agents diff --check --path <p>...`.
- Review a diff: `/code-review`, `claude-code codex review [--adversarial] [--base <b>]`, or `@agent-codex`.
- `claude-code agents list` lists the CLI agents, not this roster; `--format json` gives the capability schema.

## Slash commands

All are owner-only (`disable-model-invocation: true`); the model dispatches with the Agent tool itself.

- `/codex` · `/copilot` · `/gemini` · `/opencode` `[--host <h>] [--profile <p>] [--model <m>] <task>` — one Agent call
  (`subagent_type` = the name) whose brief opens `Run: claude-code agents ask --agent <type> [flags] --tools`. There
  is no `/claude`: the name could collide.
- `/coworker --agent <codex|copilot|gemini|opencode|claude> [--overlay <json|file>] [--model <m>] <task>` — the same
  dispatch, run here on the Databricks arm with its requests sent through the other machine (default overlay
  `{"provider":"databricks","profile":"auto","remote":"auto"}`; `--overlay` replaces it). Not a new agent type.
  Overlay keys and precedence: `rules/harness/agent.md`; what `auto` resolves to: `rules/harness/network.md`.
- `/discover-models [--profile p] [--dry-run]` runs `claude-code models discover --apply` (`--dry-run`: no `--apply`).
- A brief opening `Run: claude-code agents ask …` passes `dispatch-effort` unchanged; a missing or wrong `<model>:`
  description prefix is denied once, naming the text to resend.

## Agent and command files

- `agents/<name>.md` is the single definition, flat, `name:` equal to the stem. Sections: `Role · Invocation · Owns ·
  Knows · Verbs · Hands off to · Procedure · Report contract · Don't` (omit those that do not apply); `description:`
  says when to use it. `Knows` lists references and when to read them: nothing loads upfront.
- `commands/<name>.md` is a short launcher (Usage block allowed): read `$ARGUMENTS`, run the agent or script, relay
  the report; procedure lives in the agent or skill. Skill shell logic lives in `skills/<name>/scripts/`, anything
  else in a Go verb or Makefile target.
- Agents hold no memory scope: they propose memory in their report and the main session writes it; the dispatch hook
  appends recall pointers to every brief.

## Authoring: file, skill or brief

- **Agent file** = who (a role reused across sessions); **skill** = how (a procedure named in a brief's `Method:`);
  **brief** = this task. A one-off role is a brief to `claude`, never a new file.
- **Split** a role only when its expertise, tool grant or authority differs (read-only `Explore` vs editing `claude`)
  or several shapes reuse it; **merge** files that differ only in task wording. A role that only picks another agent
  is that agent's leaf type (`codex`, `copilot`…).
- **Model:** an alias, never a dated id (`rules/harness/providers.md` → Model selection): `sonnet` for leaves and
  `claude` (`gemini`: `haiku`), `Plan` inherits; raise one call with the Agent tool's `model`. `inherit` in a file
  means the session model (the hook pins nothing); `model: inherit` on a call reads as unset. **Tools:** list them;
  a read-only role gets none that write.
- **Frontmatter worth knowing:** `effort`, `maxTurns`, `isolation: worktree`, `background`, `memory`. Unknown fields
  are silently ignored: check with `claude --debug`.
- **Prose instructs, hooks guarantee.** A new external agent type is added once, in `agentid.All()`
  (`internal/agentid/agentid.go`); the ran-on stop gate (`externalTypes`) and labels (`labelAgent`) in
  `internal/hooks/leaves.go` derive from it. Shapes and enforcers: `skills/subagent-workflows/references/patterns.md`.
- Thinking is adaptive: never hand-roll a thinking budget. Mark stable large prefixes with `cache_control`; keep
  per-call content outside the cached block.
