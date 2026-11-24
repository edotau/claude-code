# Hook details — full matrix cells

Mechanism detail for the ¶ rows of `rules/harness/hooks.md` → Event → hook matrix (the matrix keeps a synopsis of
each, and the guardrails an agent needs before acting).

## safety-scan

The order and the two rules live in `rules/harness/hooks.md`; this is the detail. A leaf's edit-tool write to a
protected file is refused by `safety-scan` on codex, gemini, opencode, pi and copilot.

**Attribution footer.** `Co-Authored-By:` naming Claude, Anthropic, Codex, Copilot, Gemini, OpenCode or agy, or
`Generated with|by` one of them, blocks on every agent when it opens a line of a message: a commit, merge or tag
(`git commit|merge|tag`, `claude-code worktree commit`), or a PR (`gh pr create|edit` `--title`/`-t`, `--body`/`-b`,
`--body-file`/`-F`; `claude-code worktree pr create` `--title`, `--body-file`). It reads inline words (`-m`,
`--trailer`, `--body=`, `-bTEXT`), a literal `"$(cat <<'EOF' …)"` or `--body-file -` heredoc body, a message file that
exists (`-F`, `-t`, `--body-file`) and one the same command writes first (redirect, `tee`, an interpreter). A message a
substitution or variable builds at run time blocks when the command also names a footer. A footer named mid-subject
(`docs: forbid Co-Authored-By: Claude footers`) passes, as do `git log --grep`, `echo` of the phrase,
`gh pr comment` and a human co-author. It follows wrappers, `rtk`, `cd`, `git -C`, `bash -c` and `eval`; a command
nested past four levels blocks when its text holds a footer.

**Deny rules.** The live `~/.claude/settings.json` `permissions.deny` (the template's when it is unreadable) blocks a
command a `Bash(...)` rule covers, `sudo`/`doas` in command position when `Bash(sudo:*)` is denied (also inside
`bash -c`, `eval`, `ssh`, `xargs`, `find -exec`, a wrapper, a substitution, a printf fed to a shell or a script an
interpreter runs), and a Bash write (`sed -i`, redirect, `tee`, `cp`/`mv`) to, or `rm`/`mv`-away/`truncate` of, a file
an `Edit(...)` deny glob protects. An `rm`/`unlink`/`mv`-away of a literal path under a temp folder passes, never one
inside the config dir or home, through a symlink, `..` or glob, or beside `ln`/`truncate`. An interpreter script
(`python`, `node`, `perl`, `ruby`, `deno`, `bun`) naming a protected path beside a write call **asks** on Claude and
blocks on codex, copilot and agy; reads pass. Blocks name the allowed route and that the owner can run it with `!`.
A Go or other compiled program's own `exec.Command("sudo", …)` is out of the hook's reach.

Both rules match a real invocation, never words in data: a `claude-code agents|memory|plan` brief, an agent CLI prompt,
a heredoc fed to `cat > f`, `tee f` or a data command when no file runs in the same line, `grep`/`rg`/`sed` patterns
without `e`, `echo`/`printf` text not fed to a runner, `which`/`man`. A `printf`/`echo` piped into a shell, or whose
output runs (`eval "$(…)"`, `bash <(…)`, `xargs`, a file a later segment runs), is a command, judged on its decoded
text. An `apply_patch` call's patch text is not scanned; the rest of the command is. A command a Python script (stdin
heredoc or `-c`) launches from literal text, a shell string (`os.system`, `subprocess.*(…, shell=True)`) or an argv list
(`subprocess.run([…])`, `os.spawn*`, `os.exec*`) gets the verdict the same command gets in the shell; one built at run
time is not judged. `uv run`, `poetry run`, `pipx run`, `conda run` and `python -m` of one count as the interpreter they
run. A `#` after a `\`-newline continuation is mid-word (bash joins the lines) unless a blank precedes the `\`.
A `#` inside `${…}` or `$((…))` is word text (`${l%% #*}`), and one right after a subshell's `)` starts a comment.
A `>` or `<` inside `${…}` or `$((…))` is no redirect (`$((3>2))`).
A file the same command line writes from `echo`/`printf` (a redirect or `tee`) and then runs (`sh f`, `bash f`,
`zsh f`, `. f`, `source f`, `./f`) has its printed text, escapes and formats applied, and is judged as if run directly.
`cd -` returns to the previous directory, or to the starting directory when that one is unknown. A
`"$(cat <<'EOF' …)"` commit message blocks only on a footer at a line start, not one named mid-subject. The
write-path checks (this hook and docs-placement) read glued and fd redirects (`a>f`, `cp a f 2>&1`, `>|f`), keep a
subshell's `cd` inside it, and take `mv -t DIR`'s sources, not `DIR`, as moved away. `safety-scan` places a
command's relative operand (`..` included) in the session cwd under a Read deny, refusing a climbing one only after
a cd to an unknown dir. Blocks log to `claude-code doctor --audit` by reason, pattern and session id, never the
command text.

A plain Python script fed on stdin (`python3 [-I…] - <<'EOF'`, `uv run python -`; quoted delimiter or a body with no
`$(…)`, not piped on, outside any other heredoc or quote, no later shell or interpreter run, and no `exec`/`eval`,
`os.system`/`popen`/`exec*`/`spawn*`, reflection or import outside a fixed stdlib list) skips the shell-word match
(`sudo`) in its body; the deny-rule write checks still scan it. In such a script a protected path named only in a `#`
comment or inside a multi-word string no longer triggers the interpreter-write ask, unless the script splits, strips or
slices strings; a one-word or f-string literal still does.

## rtk-rewrite

Rewrites a command via `rtk rewrite` (chains included; exit 0/3 with changed output) and runs the pinned `bin/rtk`
(else PATH); for Claude in place with no permission decision, or in auto mode a block naming the form when that form
passes the hook itself. codex and opencode (plugin, after the guards allow) run `rtk hook
<agent>` instead, never with `CODEMODE` on or only; the rewrite
is offered only when every Bash PreToolUse gate the agent runs (`safety-scan`, `docs-placement`,
`worktree-claim-check`; an unknown one stops it) allows both the raw and the rewritten command, any error runs the
command as typed, and copilot and agy are not hooked (agy judges its own rules on the rewritten text). On codex a
matching dispatch ask or allow rule means no rewrite, and so does any `*.rules` file in `$CODEX_HOME/rules`,
`/etc/codex/rules` or an absolute cwd ancestor's `.codex/rules` under which `codex execpolicy check` prompts on or
forbids a segment, or judges a raw segment and its rewrite differently (any error or timeout counts): codex matches
`.rules` prefixes, which `rtk <cmd>` no longer does. On opencode any bash permission other than allow-all (`"allow"`,
`{"*":"allow"}`) in `OPENCODE_PERMISSION` or `OPENCODE_CONFIG_CONTENT` (agent blocks too) means no rewrite; rtk reads
only the config files. Vetoes: already wrapped, any `git` in a linked worktree, any `git commit`, machine-readable flags
(`--porcelain`, `--json`), value subcommands (`rev-parse`), `go vet` and `go test` with `-v`/`-list` (rtk 1.1.0 reports
a failed vet as "No issues found" and drops `-v`/`-list` test output). Exact bytes: `rtk bypass <cmd>` (or the pinned
path when PATH resolves another rtk); `HARNESS_RTK_BIN` overrides. A bare `rtk hook claude` entry in settings is stale
and removed by hand or explicit `configure --restore`. Auto permission mode skips the rewrite: its classifier gives no
verdict on a hook-changed input, so the call would bounce for a verbatim resend

## docs-placement

Besides Write/Edit targets it checks `apply_patch` headers and Bash write targets. A new `*plan*.md` at a repo root or
in `docs/` is rewritten to the dated plans home (denied with the fix where the host cannot rewrite). A Claude first
write to a native plan draft that lacks only `# Plan:` or `## Tasks` is rewritten with a note; any other first write
lacking the `docs/templates/plan.md` headings is denied, listing them. A leaf's Bash block becomes allow + note
. `HARNESS_HOOK_SKIP=docs-placement` skips both
the placement block and the plan checks.

## dispatch-effort

Gates type (a `coworker:<agent>` naming one roster agent is rewritten to it, any brief); for a type with no `model:`
line (built-ins such as `Explore`, `general-purpose`) and no explicit model, pins model and effort from the task's tier
(effort only when neither the call nor the agent file sets one); claims
slots, recalls context and labels the row, for Codex-spawned children too (`ee4a67ce`). Recall rides `additionalContext`
as a context note so the brief prompt stays unchanged. A dispatched `agents ask` carries the same block for every agent
(`withRecall`, `internal/agent/ask/memory.go`), except on `--retry`, `--job` or `--host`. The label is
the model the run resolves: a typed `--model`/`--profile` resolves as typed (a vendor CLI with no `--model` runs its
pin, else the model of the brief's task tier: a `Tier:` line, else the classifier, opus when the brief has no text;
`auto` ranks the row's served models; claude runs the brief's alias), an untyped external dispatch takes the route
ladder's first hop; a route that does not resolve offline leaves the description unlabelled with a note; print it with
`claude-code agents label <type> [--profile p] [--model m] --task FILE`. Every fix (type, label, pin) rides one
`updatedInput`, one note line each. Brief routing: `skills/subagent-workflows/SKILL.md` → Handoff contract.
Implementation: `internal/hooks/effort.go`; workflow validation: `internal/workflows/workflowlint.go`. In auto mode a
rewritten call costs one mechanical resend of the exact corrected input (the classifier refuses a hook-modified input
once)

## dispatch-failure · dispatch-workflow · dispatch-stop · dispatch-handback

`dispatch-handback` (PreToolUse on `SubagentHandback`, 15 s budget) runs the same two gates on a background leaf's
hand-back, which ends it before SubagentStop; the verbs below say hand-back or stop for that reason.

Retire the spawn's slots. `dispatch-stop` first blocks a leaf's stop (up to 2 hours from the dispatch's start, then a
visible fail-open) while a `job <id> started` its own transcript printed is still running, naming `claude-code agents
result -f <id>`; a blocked stop retires nothing. An external-type leaf (reachable through the type gate's fallback)
whose tool results carry no `ask: ran on <agent> ·` line or `ran_on` field is checked for a receipt.
Its job is an id its own `agents ask|result` call printed, else a record of that agent started
within 2 minutes after one of its ask calls. (1) Job running: a hand-back polls it
every 25 ms for up to 13 s and passes at once when it ends; otherwise (and at once for a stop) it is held, not
counted, naming `claude-code agents result -f <id>`, until the job ends or the 2 h cap (then it passes with a note); a
hold keeps every finished job's answer and the later pass attaches them in dispatch order; once ended it passes with a
top-line note quoting the record's `ran on` line (fallback included) and, on a hand-back, the job's result appended to
`message`: one summary line and its last message in full; an answer the report already quotes is replaced by `(final
answer already in the report above)`. (2) No job: it passes with a top-line note that the work was done on the relay
model, not on the agent. (3) Every hold saves the report it carried (`message`, else the last assistant text) to the
reports home and names the path. A stop has no input to rewrite, so it gets the note as a system message only.
Unreadable transcripts fail open. A stop it isn't blocking kills the leaf's leftover background shells (its own task ids
only).

On Bash, `dispatch-workflow` (the synchronous entry, so the line reaches the model) gives a subagent of an external
type (`relayLeaf`; never a `claude` leaf or the main session) whose own `agents result` poll returned on an
ended job the context line `job <id> ended: hand back now with this result`, and marks jobs it read collected for the
stuck-leaf watchdog.

On a hand-back naming a plan task (dispatch `plan_task` or the brief's `Task:`), `dispatch-handback` runs the task's
`verify:` and the scoped gate on the leaf's owned dirty files within the budget and puts `verify <id>: …` lines on
top of the message; it never holds. A rejected shape prints `verify not run: shape` and leaves only the scoped gate.
For the `verify:` allowlist, see `VerifyShape` in `internal/plans/verify.go`.

`dispatch-handback` prepends the note and receipt and appends the job result to the hand-back `message`. When a leaf
started several jobs, every finished job's answer is attached in dispatch order, each under its own receipt line,
including jobs that finished before the wait. Attribution: only ids the leaf's own `agents ask|result` call
printed count, never ids from listings, logs, files or the answer body, except one its own successful `agents
result|wait <id>` named whose record is this session's, started within 2 min after the leaf's ask, with that ask's
agent and a prompt holding its brief. With no such id it binds a record only when exactly one of this session's
records of the ask's agent has a prompt equal to the ask's whole task (parsed by `ask`'s own grammar, flags anywhere,
`--` ends them; a stdin task ties nothing) and launched within 2 min after it; otherwise the report passes with an
unattributed note naming the candidate ids, never held or refused. Every hold names `claude-code agents result -f <id>`
to run once (never a sleep).

On Bash, only in a subagent of an external type (`relayLeaf`; a `claude` leaf and the main session pass), it
refuses a poll of a finished job the leaf already handed back (`already reported: stop`) and a second follow poll of
one job while the first is in flight or backgrounded with a live writer (`one poller only`). That leaf's Bash is
limited to `claude-code` asks, commands the shared judge allows, `gofmt -l`, and `go build|vet|test`
(behind `rtk`, a bare `env`, `time [-p]`, `nice [-n N]` or `timeout [opts] N[smh]`) with `./` package patterns inside
the repo and a short flag list: `-args`, writing or program-running flags, any `go build` but `./...` or `-o
/dev/null`, and an inline or process `GOFLAGS`/`GOENV`/`GOWORK`/`GOBIN`/`GOCACHE`/`GOTOOLCHAIN` are refused (the relay
shell's own env is invisible to the hook).

## worktree-claim-check

Once per file, notes before an edit (Bash: its write targets) that a dirty file is claimed by another live session
(owner pid alive) or sibling leaf (transcript fresh) and not committed or seen clean since; asks on a `git add`/`commit`
that stages one (`-p`, `-n`/`--dry-run`, `-N` pass), blocks in a leaf under `~/.claude` (a leaf's Bash block becomes
allow + note); a sibling leaf counts until it
ends, is interrupted or stops on max_tokens; an infra error allows with a warning. Dead owners' files are removed; a
file under this process's own pid (a new session slug after `/clear` or a resume) is ours, not a peer's. Any `git`
command also gets the drained notices and divergence line. Under vault env `HARNESS_WORKTREE=lanes` it also asks (a
leaf: blocks) on an edit, stage (`add -p` included), `git apply`/`am` or `git mv` of a tracked file in the live
checkout (dry runs pass), naming `claude-code worktree create <slug>`, and leaves a lane alone; in any other repo's
primary checkout it only asks, once per session per repo (other agents get a note)

## worktree-claim

Claims cover codex `apply_patch` headers, Copilot `toolArgs.path` and OpenCode `args.filePath`; a move claims both
names, a delete its path; an edit the vendor reports failed claims nothing. Each line records the owner's start token,
so a file whose pid was reused is dropped. A non-Claude agent's line inside a dispatch carries the dispatch id as
`agent_id` (read from the running dispatch record when the hook lacks the env), and `worktree commit` there treats a
claim stamped with it, or filed by its own CLI process, as that leaf's own. Write|Edit|MultiEdit stays synchronous;
Bash runs in the async entry (`rules/harness/hooks.md` → Groups and the dispatcher)

## docs-plan-tick / docs-plan-reminder

A commit on the trunk of the primary checkout starts one detached `plan tick --verify` per plan for the open tasks its
`Task: <plan-slug> <id>` trailers name (`claude-code plan status`), in any plan. It runs each task's `verify:` (the
allowlist is under dispatch-handback) plus the scoped gate on that sha in a scratch checkout and ticks the green ones; a
shape-refused `verify:` ticks on the gate with a note, red stays open with the failing command and tail; with no trunk,
nothing ticks and one line says so. Only the main session's verified `worktree commit --task` ticks; coverage prints one
candidate per task (recent or claimed plans) and writes nothing; draft mirrors and struck tasks never tick. A lane, a
feature branch or a detached HEAD ticks nothing; a lane commit ticks when it lands (`worktree merge` promotes it, or the
hook sees in the HEAD reflog a fast-forward or merge commit land on main, whatever the call printed or a later command
did; `plan tick --range` otherwise). The Stop reminder's partials record (one shared lock) is no longer written.
`claude-code clean` sweeps obsolete `*.partial.lock` files older than one day

## memory-update

Context steward first: at ≥`HARNESS_CONTEXT_STEWARD_PCT` context (default 75; `0` mutes only the handoff) it spawns a
detached `claude-code memory update` that also writes `handoffs/<date>-session-<id8>.md`
(the hook-chosen path, kept in the marker), silently, once per crossing and only with no background task, subagent
marker or live dispatch; pending or running jobs are rechecked under the stamp lock and count as covered (a dip below
the threshold re-arms). No prompt and no continuation: compaction is `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE`. Then the
memory update, weighing edits and user turns (2 turns = 1 edit); it never blocks Stop. A failed spawn prints a
NOT-saved note naming `claude-code memory update` at once; each of the next two Stops retries the spawn once (3 spawns
in all), a Stop whose retry succeeds is silent, and the note shows once more at the cap (SessionEnd/PreCompact retry
too)

## memory-start

The focus is the bank's Current Focus, Blockers, Next Steps and In Progress; the `memory search` pointer appears when
`memory/index.md` exists; the `plans open:` line gives each plan's slug, ticked/total and next task id. The repaired
`projects/<slug>/memory` links to the store root. On `clear` it reports the compaction record, the newest
model-written handoff under 24 h, then the session's generated one; on `compact` it injects this session's own
steward handoff body first (budgeted within the 9,500-character context cap, with its path and a truncation notice),
then the record and the pointers (`rules/workflows/memory-bank.md` → Who writes what)

## memory-compact

Runs concurrently: dispatches the background update for uncovered edits (plus a record under `state/harness/compact/`)
and starts the steward handoff job when this session has none fresh, bounding steward stamp-lock acquisition by the
hook context; it waits up to 75 s for a completed, full-session-verified handoff (a timeout or failure is a stderr
line). The fact-only handoff holds git state, this session's dispatches, the newest rulings and pointers.
SessionStart(compact) then injects the steward handoff's body

## secrets-out

Matches exact and encoded forms of vault values, with no shape pass; no match returns nothing. A vault read not done
within 3/4 of the 2 s budget masks credential shapes instead, with a stderr note and a user-visible message naming the
cause (at most once a minute); a failed read leaves `state/harness/hooks/secrets-down`, and for the next minute calls
skip the vault read

## secrets-end

The job is `hook SessionEnd --secrets-end-job`; a path outside `projects/` or through a link is refused; one pass runs
at a time (`state/harness/hooks/secrets-end.lock`), and counts are logged to `secrets-end.log`

## stuck-leaf watchdog

The age limit scales by the job's `Tier:` (`fast` 0.5x, `deep` 2x). Runs `agents watch`'s classifier over this
session's jobs and adds each line once per job and reason. "Uncollected"
means no Bash call has read the finished job (`dispatch-workflow` marks it). The resume is `agents ask --session <id>
--background` with a fixed continuation prompt; a silent live job is only reported. Markers live in
`state/harness/watch/`

## worktree-busy

Vault env `HARNESS_WORKTREE`: unset asks when a peer joins the checkout (not this session's own tree, a linked
worktree, or an `agy --hub` daemon); `off`/`false`/`0` is silent. Never creates a worktree; no yes means keep working
in place. It also drains this session's notices (`state/harness/notices/`, ≤5) and, in a shared checkout diverged from
its upstream, adds one line per HEAD/upstream pair, whatever the mode

## pi-guard

Pi has no prompts and no sandbox: its only tools are the guard extension's seven (read, bash, edit, write, grep, find,
ls), each judged by `claude-code hook PreToolUse --pi-guard` (`safety-scan` for
bash). Only an explicit allow runs the tool, on the judged path; `HARNESS_HOOK_*` is ignored there. Mapping, path
rules and fail-closed behavior: `/Users/edotau/.claude/rules/harness/hooks.md` → Pi · plugin. A pi ask
carries no permission policy, like any agent (R30); `make` and the live check stay with the main session.
