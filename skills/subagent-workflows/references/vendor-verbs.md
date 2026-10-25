# Vendor CLI verbs — shared skeleton

The four vendor agents (`codex`, `copilot`, `gemini`, `opencode`) point here for the verb table, the
trailing bullets, and the setup/run/verify/report procedure they all share — each agent file keeps
only its own deltas (vendor-specific rows, flags, and sections).

## Verbs

| Verb | Use it for | Output |
| --- | --- | --- |
| `claude-code <vendor> [flags] [args…]` | launch the CLI; unknown words pass through | vendor CLI |
| `claude-code <vendor> -- <args…>` | force passthrough (shadowed names, e.g. `<vendor> -- review`) | vendor CLI |
| `claude-code <vendor> ask [flags] <task>` | one-shot run; `--tools` edits, `--background` detaches | answer |
| `claude-code <vendor> resume <id> <task>` | continue a vendor session | answer |
| `claude-code <vendor> review [scope] [--adversarial] [--background]` | diff review | verdict + findings |
| `claude-code <vendor> explore [--no-web] [--background] <question>` | read-only code scan + web research | Codebase Context Brief (file:line, URLs) |
| `claude-code <vendor> transfer [--source f] [--wait] [note]` | hand this Claude session to the vendor | job id, or answer |
| `claude-code <vendor> status [id] [--all]` | background jobs for the vendor in this repo | rows: id, status, elapsed |
| `claude-code <vendor> tail [job] [-f]` | follow a job's live log; `-f` follows with a stall heartbeat | log lines |
| `claude-code <vendor> result [id]` | a finished job's answer | answer + session id + resume cmd |
| `claude-code <vendor> cancel [id]` | interrupt a running job | `cancelled <id>` |
| `claude-code <vendor> setup [--format json]` | binary, login, route, projection checks | 4 rows; exit on failure |
| `claude-code <vendor> models` | the vendor's own model catalog | list |

- `<id>` = an exact dispatch id or a unique prefix; omitted = the newest job for the vendor in this repo.
- `--background` on `ask`/`review`/`transfer` prints `job <id> started` — follow with `status`/`result`.
- `claude-code ask --agent <vendor>` still works (saved Workflows use this form).
- A shadowed native name needs the `--` passthrough: `claude-code <vendor> -- <native-verb>`.
- Resume the underlying CLI session directly with the vendor's own resume mechanism.

## Procedure skeleton

1. **`claude-code <vendor> setup`** first. A non-zero exit (binary missing or not logged in) stops you
   — report it, never fall through to doing the work yourself silently.
2. **Pick the verb** the request needs (each agent file's own selection rules).
3. **Run it.** `--background` for anything likely over ~3 minutes, then poll with `status`/`result`;
   `cancel` on a stall.
4. **Verify the result as a claim** — re-run what it claims where you can, spot-check at least one
   cited `file:line` per answer.
5. **Report** per `../SKILL.md` → Handoff contract.

Fallback ladders (built-in `ask` chain and by-hand reassignment): `cost-routing.md` → Fallback.

## Don't

- Invent a verb — the table above is the whole surface; `claude-code <vendor> -h` lists it.
