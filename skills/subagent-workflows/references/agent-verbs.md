# Agent CLI verbs — shared skeleton

`codex`, `copilot`, `gemini` and `opencode` point here for the verb table and the shared procedure; each agent file
keeps only its own deltas. Manager dispatch is `CLAUDE.md` guideline 3; this is the relay/script CLI reference.

## Verbs

| Verb | Use it for | Output |
| --- | --- | --- |
| `claude-code <agent> [flags] [args…]` · `<agent> -- <args…>` | launch the CLI, unknown words pass through · force passthrough (shadowed names, e.g. `-- review`) | agent CLI |
| `claude-code <agent> ask [flags] <task>` · `resume <id> <task>` | one-shot run (`--tools` edits, `--background` detaches) · continue a session | answer |
| `claude-code <agent> review [--base b\|--commit c\|--pr\|--uncommitted] [--adversarial] [--background] [focus…]` | diff review (default: uncommitted) | verdict + findings |
| `claude-code <codex-or-copilot> explore [--no-web] [--background] <question>` | read-only code scan + web research | Codebase Context Brief (file:line, URLs) |
| `claude-code <agent> transfer [--source f] [--wait] [note]` | hand this Claude session to the agent | job id, or answer |
| `claude-code <agent> status [id] [--all]` · `result [id]` · `cancel [id]` | background jobs in this repo · a finished answer · interrupt | rows (id, status, elapsed) · answer + session id + resume cmd · `cancelled <id>` |
| `claude-code <agent> tail [job] [-f]` | follow a job's live log; `-f` follows with a stall heartbeat | log lines |
| `claude-code <agent> setup [--format json]` · `catalog` | binary, login, route, projection checks · the agent's model catalog | 4 rows, exit on failure · list |
| `claude-code agents dispatch explore\|fleet\|compare [flags] <task>` | shell roles: explore/fleet on copilot, compare on opencode (unless vault overlay overrides); also takes ask's `--overlay`, `--allow`, `--ask`, `--deny`, `--mode`, `--fallback` | `### label (agent/model) [status]` blocks |
| `claude-code agents dispatch --waves <plan.md\|manifest.jsonl> [--wave N[-M]] [--dry-run] [--status] [--fresh] [--gate CMD] [--format text\|json] [--timeout D]` | run a plan's waves (`Phase W.k` lines, `docs/templates/plan.md` → Tasks), one leaf per package; `--timeout` is per leaf; `--overlay`, `--fallback`, `--mode`, `--allow`, `--ask`, `--deny` pass to every leaf; never commits; a failed wave reruns whole and skips packages already verified; long-running: launch in the background, `agents wait <id> --timeout 10m` | per-wave package and verifier rows; exit 0 every requested wave passed, 1 leaf, gate or verifier failed or a leaf ran off its asked route, 2 usage or invalid plan, 3 changed file outside every owned set or unattributed, 4 interrupted or timed out with leaves running (run again) |
| `claude-code agents diff [--base b] [--path p]... [--check] [--verify id] [--format json]` | changed files with +/- counts, files outside `--path` (repeat per path), with `--check` gofmt, build, vet, golangci-lint, complexity (only findings the change introduced), test on the touched packages, plus docs (`bin/mdsmith check` on the changed `.md` files only), with `--verify` a dispatch's routing proof (workspace, model, fallback, proxy requests matched by `dispatch=<id>`, by time window only for untagged log lines) | file rows, `ok\|FAIL\|skip` gate lines, verify section, summary; exit 1 gate failed or MISMATCH, 2 usage or unknown id, 3 outside `--path` |
| `claude-code agents find <pattern> [--name\|--func\|--def\|--callers] [-i] [-w] [-l] [--path p]... [--max 200] [--format json]` | content, paths or Go AST search over tracked and new unignored files; callers are name-based; same path prefixes/globs as diff | sorted path:line:text or JSON matches/more; more is a lower bound; exit 0 matches, 1 none, 2 error |

- `<id>` = an exact dispatch id or a unique prefix; omitted = the newest job for the agent in this repo. `--background`
  on `ask`/`review`/`transfer` prints `job <id> started`.
- `claude-code agents ask --agent <agent>` is the one-shot form saved Workflows use; inline definitions:
  `--agents <json|file>` (`rules/harness/agent.md` → Dispatch behavior).

## Headless `ask` flags

`claude-code <agent> ask` = `claude-code agents ask --agent <agent>`; without `--tools` claude and opencode get no tools
and gemini is only instructed not to use any. Agent files list only what differs; the full list is
`claude-code agents ask -h`.

| Need | Flags |
| --- | --- |
| Edit files / run shell | `--tools` · `--workdir <repo>` · `--read-only` (per agent: claude tools off; codex a read-only sandbox that keeps its shell; gemini, opencode deny Bash/Write/Edit; copilot also read-only git, no other shell) |
| Guard the run | `--deny 'Bash(git push:*)'` · `--allow` · `--ask` · `--mode default\|auto\|plan` (hook-enforced per agent); an `--allow` adds but never reopens a deny; codex and gemini refuse `--tools` with `--mode plan`; gemini Ask rules become Allow; copilot Ask is approved; `--autonomous` never enables tools by itself |
| Long run | `--background` → `<agent> status [--all]` · `<agent> tail -f <id>` · `<agent> result <id>` · `<agent> cancel` |
| Model and depth | `--model <alias\|version\|wire id>` · `--effort low..max` · `--profile <p>` |
| Input | task arg or stdin (`-`) · `--attach <file>` · `--diff` / `--dirs <d,…>` / `--files <glob,…>` (inline context) · `--rubric <file>` · `--system-prompt`/`--append-system-prompt` |
| Output | `--format json` (envelope: `status`, `answer`, `model`, `fallback`) · `stream-json` · `--output-schema <json>` |
| Also | `--session <id>` (= `resume`) · `--env` · `--fallback all\|agents\|none` · `--host <ssh>` · `--direct` · `--overlay <file\|json\|->` (the dispatch config) · `agents ask --log\|--retry <id>` · `--batch <jsonl> --jobs N [--fail-fast]` |

Interactive launch takes `--model` · `--profile <p>` · `--direct` (skip the proxy) · `--overlay <file\|json\|->`
(`set`/`unset`/`keep` only; routing keys refused) · `--print-env` (resolved binary, argv, env; no launch); `--docs`
prints the CLI's help.

## Procedure skeleton

1. **Choose once.** Follow `../SKILL.md` → External relay contract. Use a leading `Run:` or select the requested
   verb; check `claude-code <agent> -h` when uncertain. Run `setup` only to diagnose a route/login failure.
2. **Run once, observe, report.** Backgrounding, `tail -f`, no-verify and verbatim `## Result` rules: the same contract.

Dispatch choice and fallback: `cost-routing.md` → Fallback. A saved Workflow named in the brief (`Workflow:`) runs per
`saved-workflows.md` → Run from a leaf.

## Don't

- Invent a verb — the table above is the whole surface; `claude-code <agent> -h` lists it.
