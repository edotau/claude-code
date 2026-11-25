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
- Review a diff: `/code-review`, or `claude-code codex review [--adversarial] [--base <b>]`.
- Cloud lanes send the diff or repo to the vendor's cloud, so each needs overlay `{"cloud":true}` on the outermost run
  and the vendor's own login: `claude-code claude review [--base b|--pr N] --overlay '{"cloud":true}'` runs
  `claude ultrareview`; `claude-code codex cloud exec "<q>" --env <id>` submits a Codex Cloud task, while
  `cloud status|list|diff|apply` read back without the opt-in. The
  vendor passthrough refuses `claude ultrareview` and `codex cloud exec`; the lane is the only way in.
- `claude-code agents list` lists the CLI agents, not this roster; `--format json` gives the capability schema.

## Slash commands

All are owner-only (`disable-model-invocation: true`); each launcher specifies its dispatch tool.

- `/codex` · `/copilot` · `/gemini` · `/opencode` `[--host <h>] [--provider <p>] [--profile <p>] [--model <m>]
  [--effort <e>] [--fallback <mode>] <task>` — the session
  runs `agents ask --carry --background --agent <type> [flags] --tools - < brief`,
  then `agents result -f <id>`. With `--host`, omit both `--carry` and `--background` and relay the foreground result.
  There is no `/claude`: the name could collide.
- `/coworker` (same flags plus `[--codemode on|only]`) — Pi in code mode: the session runs `agents ask --background
  --agent pi --provider databricks --codemode on [flags] --tools --autonomous - < brief` plain (`--carry` refuses
  pi), then `agents result -f <id>`; `--provider`/`--profile` replace the Databricks default. The run is typed
  `coworker` (receipt `coworker via pi`), the same run as the `coworker` Agent type and `agents ask --agent coworker`.
  There is no `/pi`. A route that travels whole (provider, profile, host, remote) is `agents ask --overlay`; workflow
  steps take it from one source and pass `--overlay`. Overlay keys and precedence: `rules/harness/agent.md`; what
  `auto` resolves to: `rules/harness/network.md`.
- `--model auto` (or overlay `model: auto`, a record pin `auto`; model strings carry no profile: `providers.md`)
  ranks the models that one profile serves by the brief's tier from `internal/assets/files/catalog/ranks.json`, over a
  pinned model; the label shows the pick.
  `auto`, typed or pinned, replaces a ladder cell's model on that cell's row; with nothing to rank it runs the
  agent's untyped default with a note, and it is never refused (`--fallback none` refuses only a typed concrete
  model).
- `/discover-models [--profile p] [--dry-run]` runs `claude-code models discover --apply` (`--dry-run`: no `--apply`).
- External work agents run directly from the main session via carried asks
  (`claude-code agents ask --carry --agent <a> …`; Pi as a plain ask; Databricks arm, R20), or as the Agent tool's
  `codex`, `copilot`, `gemini`, `opencode`, `coworker` types: a Haiku worker that runs the one typed
  `agents ask --agent <type> --provider databricks` its file bakes, waits and checks the result; the vendor verbs
  (`claude-code codex …`) and overlay `{"runner":"own"}` keep each CLI on its own login;
  an unnamed `claude` code leaf runs Pi first, and a typed `claude` ask on a Databricks route runs through the pi
  runner (`rules/harness/agent.md` → pi runner; `runner: own` opts out); the owner layer opens through `agents ask
  --spec` (`skills/subagent-workflows/SKILL.md` → Owner layer);
  `dispatch-effort` rewrites a missing or wrong `<model>:` description prefix on Agent leaves, never denies it.

## Agent and command files

- `agents/<name>.md` is the single definition, flat, `name:` equal to the stem. Sections: `Role · Invocation · Owns ·
  Knows · Verbs · Hands off to · Procedure · Report contract · Don't` (omit those that do not apply); `description:`
  says when to use it. `Knows` lists references and when to read them: nothing loads upfront.
- `commands/<name>.md` is a short launcher (Usage block allowed): read `$ARGUMENTS`, run the agent or script, relay
  the report; procedure lives in the agent or skill. Skill shell logic lives in `skills/<name>/scripts/`, anything
  else in a Go verb or Makefile target.
- Agents hold no memory scope: they propose memory in their report and the main session writes it; the dispatch hook
  attaches recall pointers as a context note on every Agent dispatch.

## Authoring: file, skill or brief

- **Agent file** = who (a role reused across sessions); **skill** = how (a procedure named in a brief's `Method:`);
  **brief** = this task. A one-off role is a brief to `claude`, never a new file.
- **Skill file:** start from `docs/templates/skill.md`; one home per procedure, heavy reference in `references/`.
- **Split** a role only when its expertise, tool grant or authority differs (`Explore` vs `claude`)
  or several shapes reuse it; **merge** files that differ only in task wording. A role that only picks another agent
  belongs in a direct carried ask (Pi plain), the four external worker types aside; saved workflow stages run on
  `claude`.
- **Model:** an alias, never a dated id (`rules/harness/providers.md` → Model selection): `sonnet` for leaves
  (`claude` has no pin: the hook picks haiku/sonnet/opus by brief tier), `Plan` inherits the session model (Opus on
  Databricks), the relay types (`codex`, `copilot`, `gemini`, `opencode`, `coworker`) to `haiku`; raise one call with
  the Agent tool's `model`. `inherit` in a file means the session model (the hook pins nothing);
  `model: inherit` on a call reads as unset. **Tools:** list the ones the role needs.
- **Frontmatter worth knowing:** `effort`, `maxTurns`, `isolation: worktree`, `background`, `memory`. Unknown fields
  are silently ignored: check with `claude --debug`.
- **Prose instructs, hooks guarantee.** A CLI agent is one row in the `agentid` roster table
  (`internal/agentid/agentid.go`) plus its vendor package's `agent.Register` row,
  subagent types in `agents/`; external agents run via carried asks (direct, or baked into the agent's own type)
  and `internal/hooks/dispatchhook/leaves.go` holds any leaf whose own background job is still running. Shapes and
  enforcers: `skills/subagent-workflows/references/patterns.md`.
- Thinking is adaptive: never hand-roll a thinking budget. Mark stable large prefixes with `cache_control`; keep
  per-call content outside the cached block.
