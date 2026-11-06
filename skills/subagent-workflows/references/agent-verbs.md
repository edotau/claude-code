# Agent CLI verbs — shared skeleton

The four non-Claude agents (`codex`, `copilot`, `gemini`, `opencode`) point here for the verb table, the
trailing bullets, and the setup/run/verify/report procedure they all share — each agent file keeps
only its own deltas (agent-specific rows, flags, and sections).

## Verbs

| Verb | Use it for | Output |
| --- | --- | --- |
| `claude-code <agent> [flags] [args…]` | launch the CLI; unknown words pass through | agent CLI |
| `claude-code <agent> -- <args…>` | force passthrough (shadowed names, e.g. `<agent> -- review`) | agent CLI |
| `claude-code <agent> ask [flags] <task>` | one-shot run; `--tools` edits, `--background` detaches | answer |
| `claude-code <agent> resume <id> <task>` | continue an agent session | answer |
| `claude-code <agent> review [scope] [--adversarial] [--background]` | diff review | verdict + findings |
| `claude-code <agent> explore [--no-web] [--background] <question>` | read-only code scan + web research | Codebase Context Brief (file:line, URLs) |
| `claude-code <agent> transfer [--source f] [--wait] [note]` | hand this Claude session to the agent | job id, or answer |
| `claude-code <agent> status [id] [--all]` | background jobs for the agent in this repo | rows: id, status, elapsed |
| `claude-code <agent> tail [job] [-f]` | follow a job's live log; `-f` follows with a stall heartbeat | log lines |
| `claude-code <agent> result [id]` | a finished job's answer | answer + session id + resume cmd |
| `claude-code <agent> cancel [id]` | interrupt a running job | `cancelled <id>` |
| `claude-code <agent> setup [--format json]` | binary, login, route, projection checks | 4 rows; exit on failure |
| `claude-code <agent> catalog` | the agent's own model catalog | list |
| `claude-code dispatch explore\|fleet\|compare [flags] <task>` | one task as parallel legs by role (gemini, copilot, opencode) | `### label (agent/model) [status]` blocks |

- `<id>` = an exact dispatch id or a unique prefix; omitted = the newest job for the agent in this repo.
- `--background` on `ask`/`review`/`transfer` prints `job <id> started` — follow with `status`/`result`.
- `claude-code ask --agent <agent>` is the one-shot form saved Workflows use.
  Inline definitions: `--agents <json|file>` (`rules/harness/agent.md` → Dispatch behavior).
- A shadowed native name needs the `--` passthrough: `claude-code <agent> -- <native-verb>`.
- Resume the underlying CLI session directly with the agent's own resume mechanism.

## Headless `ask` flags

`claude-code <agent> ask` = `claude-code ask --agent <agent>`; read-only unless `--tools`. Each agent file lists only
what differs (which flags it refuses, emulates or adds); the full list is `claude-code ask -h`.

| Need | Flags |
| --- | --- |
| Edit files / run shell | `--tools` · `--workdir <repo>` |
| Guard the run | `--deny 'Bash(git push:*)'` · `--allow` · `--ask` · `--mode default\|auto\|plan` (hook-enforced per agent) |
| Long run | `--background` → `<agent> status [--all]` · `<agent> tail -f <id>` · `<agent> result <id>` · `<agent> cancel` |
| Model and depth | `--model <alias\|version\|wire id>` · `--effort low..max` · `--profile <p>` |
| Input | task arg or stdin (`-`) · `--attach <file>` · `--rubric <file>` · `--system-prompt`/`--append-system-prompt` |
| Output | `--format json` (envelope: `status`, `answer`, `model`, `fallback`) · `stream-json` · `--output-schema <json>` |
| Continue | `--session <id>` (= `<agent> resume <id> "<task>"`) |
| Isolation | `--env NAME=VALUE` · `--fallback all\|agents\|none` · `--host <ssh>` · `--direct` · `--overlay <file>` |
| Replay / bulk | `ask --log` · `ask --retry <id>` · `--batch <jsonl> --jobs N [--fail-fast]` |

Interactive launch (`claude-code <agent> [flags] [args…]`) takes `--model` · `--profile <p>` · `--direct` (skip the
proxy) · `--overlay <file>` (`{set,unset,keep}` env) · `--print-env` (resolved binary, argv, env; no launch); `--docs`
prints the CLI's own help.

## Procedure skeleton

1. **`claude-code <agent> setup`** first. A non-zero exit (binary missing or not logged in) stops you
   — report it, never fall through to doing the work yourself silently.
2. **Pick the verb** the request needs (each agent file's own selection rules).
3. **Run it.** Any `ask` that may pass 150s uses `--background` (a foreground Bash call is moved
   to the background at 180s and the leaf idles). Then run `tail -f <id>` as a background Bash call
   (`run_in_background`; you are re-invoked when the job ends) — never a foreground wait or `sleep` (copilot's shell
   hook refuses it) — and read `result <id>`; `cancel` on a stall.
4. **Verify the result as a claim** — re-run what it claims where you can, spot-check at least one
   cited `file:line` per answer.
5. **Report** per `../SKILL.md` → Handoff contract.

Dispatch choice and fallback ladders (`claude-code gateway router`, the built-in `ask` chain, by-hand reassignment):
`cost-routing.md` → Fallback. A saved Workflow named in the brief (`Workflow:`) runs headless per `saved-workflows.md`
→ Run from a leaf.

## Don't

- Invent a verb — the table above is the whole surface; `claude-code <agent> -h` lists it.
