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
