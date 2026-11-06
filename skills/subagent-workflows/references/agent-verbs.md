# Agent CLI verbs — shared skeleton

`codex`, `copilot`, `gemini` and `opencode` point here for the verb table and the shared procedure; each agent file
keeps only its own deltas.

## Verbs

| Verb | Use it for | Output |
| --- | --- | --- |
| `claude-code <agent> [flags] [args…]` · `<agent> -- <args…>` | launch the CLI, unknown words pass through · force passthrough (shadowed names, e.g. `-- review`) | agent CLI |
| `claude-code <agent> ask [flags] <task>` · `resume <id> <task>` | one-shot run (`--tools` edits, `--background` detaches) · continue a session | answer |
| `claude-code <agent> review [scope] [--adversarial] [--background]` | diff review | verdict + findings |
| `claude-code <agent> explore [--no-web] [--background] <question>` | read-only code scan + web research | Codebase Context Brief (file:line, URLs) |
| `claude-code <agent> transfer [--source f] [--wait] [note]` | hand this Claude session to the agent | job id, or answer |
| `claude-code <agent> status [id] [--all]` · `result [id]` · `cancel [id]` | background jobs in this repo · a finished answer · interrupt | rows (id, status, elapsed) · answer + session id + resume cmd · `cancelled <id>` |
| `claude-code <agent> tail [job] [-f]` | follow a job's live log; `-f` follows with a stall heartbeat | log lines |
| `claude-code <agent> setup [--format json]` · `catalog` | binary, login, route, projection checks · the agent's model catalog | 4 rows, exit on failure · list |
| `claude-code dispatch explore\|fleet\|compare [flags] <task>` | one task as parallel legs by role (gemini, copilot, opencode) | `### label (agent/model) [status]` blocks |

- `<id>` = an exact dispatch id or a unique prefix; omitted = the newest job for the agent in this repo. `--background`
  on `ask`/`review`/`transfer` prints `job <id> started`.
- `claude-code ask --agent <agent>` is the one-shot form saved Workflows use; inline definitions: `--agents <json|file>`
  (`rules/harness/agent.md` → Dispatch behavior).

## Headless `ask` flags

`claude-code <agent> ask` = `claude-code ask --agent <agent>`; read-only unless `--tools`. Agent files list only what
differs; the full list is `claude-code ask -h`.

| Need | Flags |
| --- | --- |
| Edit files / run shell | `--tools` · `--workdir <repo>` · `--read-only` (read, never write or shell) |
| Guard the run | `--deny 'Bash(git push:*)'` · `--allow` · `--ask` · `--mode default\|auto\|plan` (hook-enforced per agent) |
| Long run | `--background` → `<agent> status [--all]` · `<agent> tail -f <id>` · `<agent> result <id>` · `<agent> cancel` |
| Model and depth | `--model <alias\|version\|wire id>` · `--effort low..max` · `--profile <p>` |
| Input | task arg or stdin (`-`) · `--attach <file>` · `--diff` / `--dirs <d,…>` / `--files <glob,…>` (inline context) · `--rubric <file>` · `--system-prompt`/`--append-system-prompt` |
| Output | `--format json` (envelope: `status`, `answer`, `model`, `fallback`) · `stream-json` · `--output-schema <json>` |
| Also | `--session <id>` (= `resume`) · `--env` · `--fallback all\|agents\|none` · `--host <ssh>` · `--direct` · `--overlay <file>` · `ask --log\|--retry <id>` · `--batch <jsonl> --jobs N [--fail-fast]` |

Interactive launch takes `--model` · `--profile <p>` · `--direct` (skip the proxy) · `--overlay <file>`
(`{set,unset,keep}` env) · `--print-env` (resolved binary, argv, env; no launch); `--docs` prints the CLI's help.

## Procedure skeleton

1. **`claude-code <agent> setup`** first. A non-zero exit (binary missing or not logged in) stops you: report it, never
   do the work yourself silently.
2. **Pick the verb** the request needs (each agent file's own selection rules).
3. **Run it.** Any `ask` that may pass 150s uses `--background` (a foreground Bash call moves to the background at 180s
   and the leaf idles). Then run `tail -f <id>` as a background Bash call (`run_in_background`; you are re-invoked when
   the job ends) — never a foreground wait or `sleep` (copilot's shell hook refuses it) — and read `result <id>`;
   `cancel` on a stall.
4. **Verify the result as a claim** — re-run what it claims, spot-check one cited `file:line` per answer.
5. **Report** per `../SKILL.md` → Handoff contract.

Dispatch choice and fallback: `cost-routing.md` → Fallback. A saved Workflow named in the brief (`Workflow:`) runs per
`saved-workflows.md` → Run from a leaf.

## Don't

- Invent a verb — the table above is the whole surface; `claude-code <agent> -h` lists it.
