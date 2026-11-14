---
paths:
  - "agents/**"
  - "workflows/**"
  - "commands/**"
---
# Agent Authoring and Commands

Dispatch and the allowed roster: `CLAUDE.md` guideline 3. Brief fields, relay policy, concurrency,
escalation and the test agent: `skills/subagent-workflows/SKILL.md`. The commands below are relay, script and owner
conveniences.

## Commands

| To… | Run |
| --- | --- |
| Relay or explicitly requested script (one shot) | `claude-code agents ask --agent <agent> "<task>"` (`--format json` = envelope) · `agents ask --log` · `agents ask --retry <id>` · inline agents: `agents ask --agents <json\|file>` (`rules/harness/agent.md` → Dispatch behavior) |
| Stream events, set env/system prompt, run remote, or hold a live claude session | `agents ask --format stream-json` · `--env K=V` · `--append-system-prompt` · `--host <h>` · `--live` + `agents send <id> "<task>"`; `agents status\|result\|tail\|cancel <id>`; `agents list --format json` = capability schema (`skills/subagent-workflows/references/cost-routing.md` → Ask flags and lifecycle) |
| Launch or drive codex/gemini/opencode/copilot | `claude-code <v> [args]` (`--` forces passthrough) · `<v> ask\|resume\|review\|transfer\|status\|result\|cancel\|setup\|catalog` · `--print-env` |
| Owner prompt-box syntax | `@agent-<name>` selects a subagent; owner-only `/<agent>` and `/coworker` shortcuts below |
| Sweep many files in one Gemini call (1M context) | `claude-code agents ask --agent gemini --tools --dirs <d> [--index] [--diff] [--rubric <file>] "<task>"` (or any agent; large files need `--max-file-bytes`; test files and `testdata` need `--with-tests`) |
| Review a diff; second opinion on another model | `/code-review` · `claude-code codex review [--adversarial] [--base <b>]` · `@agent-codex` |
| Project agents + CLAUDE.md into every agent CLI | `claude-code agents sync [--agent a] [--dry-run\|--check]` (also run by `install`) |
| After a leaf returns | `claude-code agents wait <id>...` · `agents result <id> --save <path>` · `agents diff --check --path <p>...` · `review scope --range <r> --caps`; read the exit code, not the output |
| Subagent status | `claude-code agents tree [--watch\|--background]` · `agents reap` |
| What a leaf changed, stayed inside its files, passes the gates | `claude-code agents diff [--base <ref>] [--path <path>]... [--check] [--verify <id>] [--format json]` (gates: gofmt, build, vet, golangci-lint, complexity, test, docs on changed `.md`; exit 1 gate failed or MISMATCH, 3 outside `--path`) |
| Search repo code deterministically (tracked and new files, never ignored ones) | `claude-code agents find <pattern> [--name\|--func\|--def\|--callers] [-i] [-w] [-l] [--path <p>]... [--max 200] [--format text\|json]` |

## Slash commands

| Command | Does |
| --- | --- |
| `/codex` · `/copilot` · `/gemini` · `/opencode` `[--host <host>] [--profile <p>] [--model <m>] <task>` | Sends the task to that agent type on its own pin, on this machine or, with `--host <host>`, on that one: ONE Agent tool call (`subagent_type` = the name, dispatch per `CLAUDE.md` guideline 3) whose brief opens with `Run: claude-code agents ask --agent <type> [flags] --tools`. No `/claude`: the name could collide |
| `/coworker --agent <codex\|copilot\|gemini\|opencode\|claude> [--overlay <json\|file>] [--model <m>] <task>` | The same dispatch with one difference: the agent still runs here, but on the Databricks arm with its Databricks requests sent through the other machine, so the leaf's call is `claude-code agents ask --agent <type> --overlay '{"provider":"databricks","profile":"auto","remote":"auto"}' --tools`. `--overlay <json\|file>` replaces that document. Not a new agent or type |
| `/discover-models [--profile p] [--dry-run]` | Runs `claude-code models discover --apply` (`--dry-run`: no `--apply`) |

All six are owner-only (`disable-model-invocation: true`); the model dispatches with the Agent tool itself. A brief
that opens with `Run: claude-code agents ask …` passes `dispatch-effort` with its tool input unchanged (context notes
only); a missing or wrong `<model>:` description prefix is denied once, naming the text to resend. What `auto`
resolves to: `rules/harness/network.md`.

## Agent and command files

- `agents/<name>.md` is the single definition, flat, `name:` equal to the stem. Skeleton:
  `Role · Invocation (agents with a slash entry) · Owns · Knows (when it has on-demand references) · Verbs (when it
  drives a CLI) · Hands off to · Procedure · Report contract · Don't` (`##` sections); `description:`
  says when to use it. `Knows` lists the references the agent reads and when: make knowledge available, not
  mandatory; nothing loads upfront. Memory needs no entry: the dispatch hook appends recall pointers to every brief.
- `commands/<name>.md` is a short launcher (Usage block allowed): read `$ARGUMENTS`, run the agent/script, relay the
  report, point at the agent file; procedure lives in the agent or skill.
  Skill shell logic lives in `skills/<name>/scripts/`; anything else in a Go verb or Makefile target.
- Agents hold no memory scope; they PROPOSE memory in their report and the main session writes it.
- `claude-code agents list` lists the CLI agents, not this roster.

## Authoring: file, skill or brief

- **Agent file** = who (a role reused across sessions); **skill** = how (a procedure any role follows, named in a
  brief's `Method:`); **brief** = this task. A one-off role is a brief to `claude`, never a new file.
- **Split** a role into its own file only when its expertise, tool grant or authority differs (read-only `Explore`
  vs editing `claude`), or several shapes reuse it; **merge** when two files differ only in task wording. A role
  that only picks a different agent is the leaf type of that agent (`codex`, `copilot`…), not a new file.
- **Model:** an alias, never a dated id: `sonnet` for leaves and `claude` (`gemini`: `haiku`), `opus` for `Plan`;
  raise one call with the Agent tool's `model` (`opus` for a hard task, `fable` only for two large designs at once);
  provider aliases resolve through `internal/models` and `internal/providers/defaults.json`. Frontmatter in
  `agents/*.md` uses an alias; `inherit` in a file means the session model (the hook then pins nothing), `model:
  inherit` on the call reads as unset; pinning is for third-party providers. **Tools:** list them; a read-only role
  gets none that write.
- **Frontmatter worth knowing:** `effort`, `maxTurns`, `isolation: worktree`, `background`, `memory`. Unknown fields
  are silently ignored: check with `claude --debug`.
- **Prose instructs, hooks guarantee.** A new external agent type is added once, in `agentid.All()`
  (`internal/agentid/agentid.go`); the ran-on stop gate (`externalTypes`) and labels (`labelAgent`) in
  `internal/hooks/agents/leaves.go` derive from it. Shapes and what enforces each:
  `skills/subagent-workflows/references/patterns.md`.

## Authoring: thinking and caching

- Thinking is adaptive; never hand-roll a thinking budget in a skill or command — a model that needs one gets it in
  provider config.
- Mark stable large prefixes with `cache_control`; content that changes per call must stay outside the cached block.
