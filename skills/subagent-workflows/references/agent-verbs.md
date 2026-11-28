# Agent CLI verbs — shared skeleton

Dispatch choice and relay behavior: `../SKILL.md` → Leaf types and External relay contract.
Check `claude-code agents <verb> -h` or `claude-code <agent> <verb> -h` for exact arguments.

## Verbs

| Command | Purpose |
| --- | --- |
| `claude-code <agent> [flags] [args…]` | Launch; `-- <args…>` forces native CLI passthrough. |
| `agents ask` | Run one task; `--overlay` sets its route, `--background` detaches. |
| `<agent> review` | Review a diff; `--base`, `--commit`, `--pr` or default `--uncommitted`. |
| `<agent> transfer` | Hand over a session; `--source` selects it, `--wait` follows. |
| `agents status` | Inspect an id or `--agent`; `--all` includes other repos. |
| `agents result` | Read a result; `-f` follows, `--events` streams, `--save` writes it. |
| `agents cancel` | Interrupt a job; `--lost --dry-run` previews stale-record cleanup. |
| `agents brief` | Render `--task <file>` with repeated `--own`; `--path` reserves a fresh brief. |
| `agents label` | Print the model prefix for the leaf description. |
| `agents wait` | Wait for ids; `--timeout` bounds waiting, `--progress` reports activity. |
| `agents tree --files` | Inspect live sessions and owned dirty files. |
| `doctor --agent <a>` | Diagnose the agent's binary, login and route. |
| `<agent> profile`, `catalog` | Inspect pins and served models. |
| `agents ask --waves <plan>` | Execute plan waves; `--dry-run` previews dependencies. |
| `agents diff` | Inspect changes; `--path` scopes, `--check` validates, `--verify <id>` checks routing. |
| `agents fix --path <files>` | Apply scoped formatters; `--dry-run` previews. |
| `agents find <pattern>` | Search content, paths or Go symbols; `--callers` is name-based. |

An id is an exact dispatch id or unique prefix. Launching with `--background` returns the job id.
Detailed result formats and flags belong to CLI help, not copied schemas.
Validation scope: `rules/standards/testing-guidelines.md` → Several worktrees: collapse, then test once.

## Headless `ask` flags and overlay keys

Use `claude-code agents ask --overlay '{"agent":"<agent>"}' <task>`; stdin uses `-`.
Overlay keys, defaults and inheritance: `rules/harness/agent.md` → Overlay.
Flags and lifecycle: [cost-routing.md](cost-routing.md) → Ask flags and lifecycle.

- `--workdir` selects the checkout; `--brief` carries typed task data; `--attach` adds context.
- `--batch <jsonl> --jobs N` runs explicit batches; `--waves <plan>` uses plan dependencies.
  Inspect `--wave`, `--status`, `--fresh`, `--gate` and `--profiles` in help before use.
- `--session` resumes a session; `--retry` replays a recorded dispatch. Recovery authority:
  `../SKILL.md` → Error handling. A budget-ended task is main's work, not a retry recipe.
- Interactive launch flags are distinct; inspect `<agent> -h`. `--print-env` previews launch resolution,
  `--docs` prints vendor help.

## Procedure

Choose once, run once, observe and report through `../SKILL.md` → External relay contract.
Fallback: [cost-routing.md](cost-routing.md) → Fallback.

## Carried dispatch details

For an agent whose row sets `Carry` (codex, copilot, gemini, opencode), run the ask yourself as a background Bash
task, then follow it with one wait call; `--carry` refuses pi and claude and takes no `--dry-run` or `--env` (preview
by dropping it). Pi runs the same way without `--carry`, on any Databricks model for review, audits and self-tested
edits. As an Agent leaf instead, pick the type (`agents/<type>.md`); its own CLI on its own login stays reachable here
(`claude-code <agent> …`, overlay `{"runner":"cli"}`). Codex handles cross-family review, plan vetting and delegated
edits (`claude-code codex review` for a diff), gemini research and codebase sweeps, opencode code and docs work,
copilot test repair and mechanical edits:

```bash
claude-code agents ask --carry --background --overlay '{"agent":"<agent>"}' [ask flags] - < BRIEF
claude-code agents result -f <id>
```

The carrier runs the inner ask as a plain child (no model tokens); a cut-off child is finished by Pi, and the receipt
ends `ask: finished by pi (<model>)`. Rules: `rules/harness/agent.md` → Carried asks.
`agents wait --progress` shows a Pi leaf's `compacting` or `retrying` phase instead of `quiet`; the idle limit applies.
**Default leaf route:** a `claude` code leaf whose brief has no `Agent:` line runs the task as a Pi ask first; review,
docs and gate leaves stay in-context. A brief naming `Agent: pi` (or the type `pi`) is retyped to `coworker`; with
`Codemode: off` it stays a `claude` leaf running a plain Pi ask. A Pi refusal or failed start (not on the profile's
menu, no Databricks route, guard did not load) is the one refusal exception: it does the task in-context, `Ran on:`
naming the failed Pi attempt and what then ran. A brief needing the Agent tool, skills or MCP says `Agent: in-context`;
`Agent: claude` in a wave plan runs a headless claude ask. Copilot repair briefs name
`rules/standards/testing-guidelines.md` → TEST and use overlay `"tools":true`.

## External relay checklist

- Reserve the brief with `claude-code agents brief --path <agent>`, read it, then attach the task at that exact path.
  Keep the prescribed route and flags; add `--workdir` only for the brief's `Workdir:`.
- Use `agents ask --background` for background work, never shell backgrounding or `run_in_background`.
- On usage, startup failure or vendor refusal, return the command, redacted error and job id; do not silently
  substitute an agent (except the default Pi refusal above).
- Wait with one `claude-code agents result -f <id>` or `agents wait <id> --timeout 590s` per turn. Never start another
  poller, sleep, or poll files, status or verdicts. After four waits with a running job, hand back its id and follow
  command as "job still running".
- When the job ends, make `SubagentHandback` the next action. Include manager messages received meanwhile under
  "Open items (not delivered to the job)". Then stop; later notifications get "already reported".
- Return the work agent's verdict under `## Result`, preceded by its actual `ask: ran on` receipt and job/session
  ids. Add `Relay note:` for dispatch errors; never manufacture a receipt. An unattributed ask is unproven.
- Run only the stage's prescribed `agents diff --verify <id>`; fan-in belongs to the manager. Inside the vendor CLI or
  Pi, do the task directly; never invoke the wrapper or ask yourself.
