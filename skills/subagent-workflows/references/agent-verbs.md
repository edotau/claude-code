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

- `<id>` = an exact dispatch id or a unique prefix; omitted = the newest job for the agent in this repo.
- `--background` on `ask`/`review`/`transfer` prints `job <id> started` — follow with `status`/`result`.
- `claude-code ask --agent <agent>` is the one-shot form saved Workflows use.
- A shadowed native name needs the `--` passthrough: `claude-code <agent> -- <native-verb>`.
- Resume the underlying CLI session directly with the agent's own resume mechanism.

## Procedure skeleton

1. **`claude-code <agent> setup`** first. A non-zero exit (binary missing or not logged in) stops you
   — report it, never fall through to doing the work yourself silently.
2. **Pick the verb** the request needs (each agent file's own selection rules).
3. **Run it.** `--background` for anything likely over ~3 minutes, then poll with `status`/`result`;
   `cancel` on a stall.
4. **Verify the result as a claim** — re-run what it claims where you can, spot-check at least one
   cited `file:line` per answer.
5. **Report** per `../SKILL.md` → Handoff contract.

Dispatch choice and fallback ladders (`claude-code gateway router`, the built-in `ask` chain, by-hand reassignment):
`cost-routing.md` → Fallback. A saved Workflow named in the brief (`Workflow:`) runs headless per `saved-workflows.md`
→ Run from a leaf.

## Don't

- Invent a verb — the table above is the whole surface; `claude-code <agent> -h` lists it.
