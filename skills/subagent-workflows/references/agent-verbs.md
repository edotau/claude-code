# Agent CLI verbs — shared skeleton

Carried asks and plain `pi` asks point here for the verb table and the shared procedure; every agent uses the same
CLI verbs. Manager dispatch is `CLAUDE.md` guideline 3; this is the leaf/script CLI reference.

## Verbs

| Verb | Use it for | Output |
| --- | --- | --- |
| `claude-code <agent> [flags] [args…]` · `<agent> -- <args…>` | launch the CLI, unknown words pass through · force passthrough (shadowed names, e.g. `-- review`) | agent CLI |
| `claude-code agents ask --overlay '{"agent":"<agent>"}' [flags] <task>` | one-shot run (overlay `"tools":true` edits, `--background` detaches; `--session <id>` continues a session) | answer |
| `claude-code <agent> review [--base b\|--commit c\|--pr\|--uncommitted] [--adversarial] [--background] [focus…]` | diff review (default: uncommitted); codex, copilot, gemini, opencode, pi | verdict + findings |
| `claude-code <agent> transfer [--source f] [--wait] [note]` | hand this Claude session to the agent | job id, or answer |
| `claude-code agents status [<id>] [--agent A] [--all]` | dispatch state: no id, no agent = the 20 most recent; `--agent` = that agent's jobs in this repo (`--all`: every repo) | rows (id, status, elapsed) |
| `claude-code agents result [<id>] [--agent A] [-f] [--events] [--save F] [--verdict]` | a dispatch's answer; `-f` follows a running job with a stall heartbeat, `--events` streams its events | answer + session id + resume cmd · log lines |
| `claude-code agents cancel (<id> \| --lost) [--agent A] [--dry-run]` | interrupt a running job · `--lost` marks dead-but-running records as error | `cancelled <id>` |
| `claude-code agents brief --task F --own P... [--report P]` | print a leaf brief: `Owned:` line, the task, the leaf rules and reporting contract (`internal/assets/brief/brief.tmpl`, the one copy); pipe it to `agents ask -`; `agents diff --verify <id>` reads `Owned:` back | the brief; exit 2 usage |
| `claude-code agents wait <id>... [--timeout 9m30s] [--progress]` | block until the dispatches finish (`--progress`: a stderr line per activity change); exit 0 all done, 1 any error or lost, 2 usage, 4 timed out with jobs still running | final status rows |
| `claude-code agents serve [--start\|--stop\|--restart\|--status] [--cap N]` | opt-in daemon: each `ask --background` job waits for its `(provider, profile)` pool (account cap, or `--cap`; 429 cool-down) before its agent starts; no daemon or a refusal runs the job at once; `--restart` drains (waiters re-queue) | `--status`: pid and per-pool `running/cap`, cooling end, waiting ids; exit 1 not running |
| `claude-code doctor --agent <a> [--format json]` | that agent's binary, login, route and projection rows; exit 1 when its binary or login fails | rows; JSON `{ok, checks:[{name, ok, hard, detail}]}` |
| `claude-code <agent> profile [<p>\|--clear\|--off]` · `catalog` | show or set the profile (`--clear` = own login, `--off` = stop routing) · the model catalog | the profile · list |
| `claude-code agents ask --waves <plan.md\|manifest.jsonl> [--wave N[-M]] [--dry-run] [--status] [--fresh] [--gate CMD] [--format text\|json] [--timeout D]` | run a plan's waves (`Phase W.k` lines, `docs/templates/plan.md` → Tasks), one leaf per package, a `Blocked by:` task after its blockers (`--dry-run` shows the waits); `--timeout` is per leaf; `--overlay`, `--fallback`, `--profiles`, `--provider` pass to every leaf; never commits; a failed wave reruns whole and skips packages already verified; long-running: launch in the background, `agents wait <id> --timeout 9m` per call | per-wave package and verifier rows; exit 0 every requested wave passed, 1 leaf, gate or verifier failed or a leaf ran off its asked route, 2 usage or invalid plan, 3 changed file outside every owned set or unattributed, 4 interrupted or timed out with leaves running (run again) |
| `claude-code agents diff [--base b] [--path p]... [--check [--gates g] [--all]] [--verify id] [--format json]` | changed files with +/- counts, files outside `--path` (repeat per path), with `--check` gofmt, build, vet, golangci-lint, complexity (only findings the change introduced), test on the touched packages, quality (`make lint-quality`'s comment and size checks), plus docs (`bin/mdsmith check` on the changed `.md` files, plus `mdlint.Parity` when a lint config changed or with `--all`, plus `memory fmt --check` when a memory-store file changed), findings as `path:line: tool/rule: message` rows (`--format json`: objects), `--gates lint\|test\|<names>` picks gates (lint = gofmt, vet, golangci-lint with `make lint`'s flags, docs, yaml, shell, quality; a missing tool then fails), `--all` checks the whole tree, never writes, with `--verify` a dispatch's routing proof (workspace, model, fallback, proxy requests matched by `dispatch=<id>`, by time window only for untagged log lines) | file rows, `FAIL\|skip` gate rows, verify section, summary including pass count (`--format json` keeps all gates); exit 1 gate failed or MISMATCH, 2 usage or unknown id, 3 outside `--path` |
| `claude-code agents fix --path p... [--dry-run]` | the formatter fixers (gofmt, golangci-lint fmt, mdsmith fix for Markdown, yamlfmt) on the tracked files under each `--path` only; refuses no `--path`, untracked or symlinked files and files a live peer holds; `--dry-run` lists what would change | `path: tool` rows, summary; exit 1 skipped file, missing tool or failed fixer, 2 usage |
| `claude-code agents find <pattern> [--name\|--func\|--def\|--callers] [-i] [-w] [-l] [--path p]... [--max 200] [--format json]` | content, paths or Go AST search over tracked and new unignored files; callers are name-based; same path prefixes/globs as diff | sorted path:line:text or JSON matches/more; more is a lower bound; exit 0 matches, 1 none, 2 error |

- `<id>` = an exact dispatch id or a unique prefix; omitted with `--agent` = that agent's newest job in this repo.
  `--background` on `ask`/`review`/`transfer` prints `job <id> started`.
- `claude-code agents ask --overlay '{"agent":"<agent>"}'` is the one-shot form saved Workflows use; inline
  definitions: overlay `agents` (an object) (`rules/harness/agent.md` → Dispatch behavior).
- `agents ask --retry <id> --recover` reruns a dead or cut-off job in a fresh session (not `--session`) seeded from that
  job's event log and claims the record once; a second recover names the winner's id and fails.
  `agents result <id> --events --after <seq>` resumes an event stream after sequence `<seq>`.

## Headless `ask` flags and overlay keys

Every headless ask is `claude-code agents ask --overlay '{"agent":"<agent>"}'`; an ask with no tools key gets tools on.
Agent files list only what differs; the full list is `claude-code agents ask -h`.

| Need | Flags |
| --- | --- |
| Edit files / run shell | `"tools":true` · `--workdir <repo>` (tools are on by default; `"tools":false` turns them off, `"noTools":true` too for claude only). No agent policy: `safety-scan` (deny rules, the AI-footer block) applies to every agent; `"autonomous"` never enables tools |
| Long run | `--background` → `agents status --agent <agent> [--all]` · `agents result -f <id>` · `agents cancel --agent <agent>` |
| Model and depth | `"model":"<alias\|version\|wire id>"` · `"effort":"low..max"` · `"tier":"fast\|standard\|deep"` (heads the brief with `Tier:`) · `"provider":"databricks","profile":"<p>"` (R20; picks a profile and the tier's model) |
| Run | `"timeout":"<d>"` (per attempt, default 15m; a foreground ask past the 10m Bash cap runs `--background`) · `"runner":"pi\|own"` · `"codemode":"off\|on\|only"` (`on`/`only` need `"tools":true`; `only` is pi's) · `--plan-task <id>` · `--brief <json>` (task keys: `prompt`, `own`, `report`, `verify`, `contract`, `action`) |
| Input | task arg or stdin (`-`) · `--attach <file>` · `--diff` / `--dirs <d,…>` / `--files <glob,…>` (inline context) · `--rubric <file>` · `systemPrompt`/`appendSystemPrompt` |
| Output | `--format json` (envelope: `status`, `answer`, `model`, `fallback`) · `stream-json` · `outputSchema` |
| Also | `--session <id>` (continues that session) · `set` · `"fallback":"none\|pin\|agents\|all"` · `"host":"<ssh>"` · `"endpoint":"direct"` · `--overlay <file\|json\|->` (the dispatch config) · `agents status` (the log) · `agents ask --retry <id>` · `--batch <jsonl> --jobs N [--fail-fast]` (compare models: one `model` per line) · `"fleet":true,"tools":true` (copilot fleet) |

The route fragments above must be combined: `profile` and `endpoint` require their `provider` and `profile` in the same
overlay. For the special session lead use the legacy `--profile session` flag, which has different semantics.

Interactive launch takes `--model` · `--profile <p>` · `--direct` (skip the proxy) · `--overlay <file\|json\|->`
(explicit `--overlay` also accepts routing; launch acceptance: `rules/harness/agent.md` → Overlay).
`--print-env` shows the resolved binary, argv and env without launching; `--docs` prints the CLI's help.

## Procedure

Choose once (`../SKILL.md` → Carried dispatch / External relay contract; plain ask for Pi, `<agent> -h` when unsure;
`doctor --agent <name>` only to diagnose a route or login failure), run once, observe, report: backgrounding,
`result -f`, no-verify and the verbatim `## Result` are that contract's. Dispatch choice and fallback:
`cost-routing.md` → Fallback; a brief's `Workflow:` runs per `saved-workflows.md` → Run from a leaf. Never invent a
verb: the table above is the relay surface; `claude-code agents -h` and `<agent> -h` list the rest.
