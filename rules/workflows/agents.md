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

- Verb table (`ask`, `status`, `result`, `cancel`, `wait`, `brief`, `diff`, `find`, `ask --waves`, codex/gemini/
  opencode/copilot verbs): `skills/subagent-workflows/references/agent-verbs.md`. Ask flags, `--live`, `--host`,
  stream-json: `skills/subagent-workflows/references/cost-routing.md` → Ask flags and lifecycle.
- Not in those: `claude-code agents sync [--agent a] [--dry-run|--check]` projects agents and CLAUDE.md into every
  agent CLI (`install` runs it); `agents tree [--watch|--detail|--files]` (`--files`: held and free dirty files, leaf
  notes); `agents send parent <note>` (a leaf's one-line note to its dispatcher, shown on its next prompt);
  `agents watch [--once]` lists this session's stale jobs with the next command (exit 3; the hooks run it
  automatically, `hooks.md`);
  `agents label <type> [--profile p] [--model m] --task F` prints the description prefix (an equivalent
  alias or full id is accepted; on a Databricks session the generated label is the full `databricks-` id);
  `review scope --range <r> --caps`.
- Read an exit code, not output: `agents wait`, `agents diff --check --path <p>...`.
- Review a diff: `/code-review`, `claude-code codex review [--adversarial] [--base <b>]`, or `@agent-codex`.
- Cloud lanes send the diff or repo to the vendor's cloud, so each needs overlay `{"cloud":true}` on the outermost run
  and the vendor's own login: `claude-code claude review [--base b|--pr N] --overlay '{"cloud":true}'` runs
  `claude ultrareview`; `claude-code codex cloud exec "<q>" --env <id>` submits a Codex Cloud task, while
  `cloud status|list|diff|apply` read back without the opt-in. The
  vendor passthrough refuses `claude ultrareview` and `codex cloud exec`; the lane is the only way in.
- `claude-code agents list` lists the CLI agents, not this roster; `--format json` gives the capability schema.

## Slash commands

All are owner-only (`disable-model-invocation: true`); the model dispatches with the Agent tool itself.

- `/codex` · `/copilot` · `/gemini` · `/opencode` `[--host <h>] [--profile <p>] [--model <m>] <task>` — one Agent call
  (`subagent_type` = the name) whose brief opens `Run: claude-code agents ask --agent <type> [flags] --tools`. There
  is no `/claude`: the name could collide.
- `/coworker --agent <codex|copilot|gemini|opencode|claude> [--overlay <json|file>] [--model <m>] <task>` — the same
  dispatch, run here on the Databricks arm with its requests sent through the other machine (default overlay
  `{"provider":"databricks","profile":"auto","remote":"auto"}`; `--overlay` replaces it). Not a new agent type.
  The route travels whole (provider, profile, host, remote); workflow steps take it from one source and pass
  `--overlay`. Overlay keys and precedence: `rules/harness/agent.md`;
  what `auto` resolves to: `rules/harness/network.md`.
- `--model auto` (or overlay `model: auto`, a record pin `auto`; model strings carry no profile: `providers.md`)
  ranks the models that one profile serves by the brief's tier from `internal/catalog/ranks.json`, over a pinned
  model; the label shows the pick.
  `auto`, typed or pinned, replaces a ladder cell's model on that cell's row; with nothing to rank it runs the
  agent's untyped default with a note, and it is never refused (`--fallback none` refuses only a typed concrete
  model).
- `/discover-models [--profile p] [--dry-run]` runs `claude-code models discover --apply` (`--dry-run`: no `--apply`).
- A brief opening `Run: claude-code agents ask …` keeps its prompt byte-identical through `dispatch-effort`; a missing
  or wrong `<model>:` description prefix (the model the run resolves) is rewritten, never denied. Its hand-back is
  never lost: with no `ask: ran on` line it is held while the job runs, the hook runs a never-run `Run:` line itself,
  and relay-done work passes with a top-line note naming the relay model.

## Agent and command files

- `agents/<name>.md` is the single definition, flat, `name:` equal to the stem. Sections: `Role · Invocation · Owns ·
  Knows · Verbs · Hands off to · Procedure · Report contract · Don't` (omit those that do not apply); `description:`
  says when to use it. `Knows` lists references and when to read them: nothing loads upfront.
- `commands/<name>.md` is a short launcher (Usage block allowed): read `$ARGUMENTS`, run the agent or script, relay
  the report; procedure lives in the agent or skill. Skill shell logic lives in `skills/<name>/scripts/`, anything
  else in a Go verb or Makefile target.
- Agents hold no memory scope: they propose memory in their report and the main session writes it; the dispatch hook
  appends recall pointers to a brief; in auto mode, or on a brief opening `Run: claude-code agents ask`, they arrive
  as a context note instead.

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
