# Hook details — full matrix cells

Verbatim cells moved from `rules/harness/hooks.md` → Event → hook matrix (the matrix keeps a synopsis of each).

## safety-scan

**Block** destructive shapes (`rm -rf /`, a system dir or `$HOME` as the shell reads it: quoted, escaped, `//`, `..`;
fork bomb; `curl \| sh` also by absolute shell path; `eval "$(curl …)"`; `base64 -d \| sh`) a Bash write (`sed -i`,
redirect, `tee`, `cp`/`mv`) to, or `rm`/`mv`-away/`truncate` of, a file the template's `Edit(...)` deny rules protect
(an `rm`/`unlink`/`mv`-away of a literal path under a temp folder passes, never one inside the config dir or home,
through a symlink, `..` or glob, beside `ln`/`truncate`; an interpreter script — `python`, `node`, `perl`, `ruby`,
`deno`, `bun` — naming one beside a write call **asks**; reads pass), and a `commit --amend`/`rebase`/`reset <commit>`
in a shared checkout (`~/.claude`, a tree a peer session works in, a main tree where this session has live leaves or the
caller is a leaf; own linked worktree exempt; an infra error allows with a warning); **ask** on Claude (block on
codex/copilot) for `git stash/checkout/switch/reset/restore/clean` under `~/.claude` (nested repos too; linked worktrees
exempt), an AI-attribution footer (any footer asks; safety-scan never rewrites or strips a command), and on
intent-dependent shapes (`sudo`, `DROP TABLE`, `rm -rf .git`, a force-push of main/master: spelled out, after git global
flags, or with no refspec while main/master is checked out) naming the safer alternative; **warn** only prints. Every
shape ignores inert text (commit/tag/`gh` messages, `echo`/`printf`/`grep`/`rg` arguments; file-only `cat`/`tee` heredoc
bodies for the pipe, exfil and SQL shapes) unless it is piped on or holds a `$(…)`; a hard-block shape found only in
such text asks instead of blocking, but any `<<` in the command scans the heredoc bodies too and blocks. An
`apply_patch` call's patch text (its `*** Begin Patch` heredoc or one literal argument) is not scanned; the rest of
the command is. Blocks log to `claude-code doctor --audit` by reason and pattern, never the command

## rtk-rewrite

Rewrites a command via `rtk rewrite` (chains included; exit 0/3 with changed output; never sets a permission decision)
and runs the pinned `bin/rtk` (else PATH); Claude sessions only. Vetoes: already wrapped, any `git` in a linked
worktree, any `git commit`, machine-readable flags (`--porcelain`, `--json`), value subcommands (`rev-parse`), `go
vet` (rtk 1.1.0 reports its failures as "No issues found"). Exact bytes: `rtk bypass <cmd>` (or the pinned path when
PATH resolves another rtk); `HARNESS_RTK_BIN` overrides. A bare `rtk hook claude` entry in settings is stale and
removed by hand or explicit `configure --restore`. Auto permission mode skips the rewrite: its classifier gives no
verdict on a hook-changed input, so the call would bounce for a verbatim resend

## dispatch-effort

Gates type (a `coworker:<agent>` naming one roster agent is rewritten to it, any brief); for a type with no `model:`
line (built-ins such as `Explore`, `general-purpose`) and no explicit model, pins model and effort from the task's tier
(effort only when neither the call nor the agent file sets one), never for a `Run: claude-code agents ask` brief; claims
slots, recalls context and labels the row. Recall is a context note in auto mode for every brief, otherwise only on a
`Run:` brief; any other brief gets it appended. A `Run:` line naming a missing Databricks workspace, or (with
`--fallback none`) a model that workspace's fresh cached listing does not serve, is denied with `agents ask`'s own
preflight text plus the fix (code cannot pick the workspace or model); a stale or missing listing passes. The label is
the model the run resolves: a typed `--model`/`--profile` resolves as typed (a vendor CLI with no `--model` runs its
pin, else opus; claude runs the brief's alias), an untyped external dispatch takes the route ladder's first hop; a route
that does not resolve offline leaves the description unlabelled with a note. Every fix (type, label, pin) rides one
`updatedInput`, one note line each. Brief routing: `skills/subagent-workflows/SKILL.md` → Handoff contract.
Implementation: `internal/hooks/effort.go`; workflow validation: `internal/workflows/workflowlint.go`. In auto mode a
rewritten call costs one mechanical resend of the exact corrected input (the classifier refuses a hook-modified input
once)

## dispatch-failure · dispatch-workflow · dispatch-stop

Retire the spawn's slots. `dispatch-stop` first blocks a leaf's stop (up to 2 hours from the dispatch's start, then a
visible fail-open) while a `job <id> started` its own transcript printed is still running, naming `claude-code agents
result -f <id>`; a blocked stop retires nothing. A `codex`/`copilot`/`gemini`/`opencode` leaf whose tool results carry
no `ask: ran on <agent> ·` line or `ran_on` field (`claude` leaves are exempt; the type is the request) is never
refused. Its job is an id its results printed, else a record of that agent started within 2 minutes after one of its ask
calls (or after the hook's own launch). (1) Job running: the hand-back or stop is held, not counted, naming `claude-code
agents result -f <id>`, until the job ends or the 2 h cap (then it passes with a note); once ended it passes with a
top-line note quoting the record's `ran on` line (fallback included) and, on a hand-back, the job's result appended to
`message`: one summary line and its last message in full, skipped when the report already quotes it. (2) No job, the
brief opens with a `Run:` ask line and the leaf made no Edit/Write/MultiEdit/NotebookEdit call and no file-changing
Bash call (redirect, `sed -i`, `tee`, `cp`, `mv`, `rm`, `git apply`, a formatter; temp-dir scratch excluded): the hook starts that line detached with its flags unchanged (marker and output under
`state/harness/dispatch-relay/`), holds the leaf (`sleep 60`, then hand back again) and attaches the output and receipt
with a note saying the hook ran it. (3) The leaf edited files, or the brief has no `Run:` line: it passes with a
top-line note that the work was done on the relay model, not on the agent; the line is not run. (4) Every hold saves the
report it carried (`message`, else the last assistant text) to the reports home and names the path. A stop has no input
to rewrite, so it gets the note as a system message only. Unreadable transcripts fail open. A stop it isn't blocking
kills the leaf's leftover background shells (its own task ids only)

## worktree-claim-check

Once per file, notes before an edit (Bash: its write targets) that a dirty file is claimed by another live session
(owner pid alive) or sibling leaf (transcript fresh) and not committed or seen clean since; asks on a `git add`/`commit`
that stages one (`-p`, `-n`/`--dry-run`, `-N` pass), blocks in a leaf; a sibling leaf counts until it ends, is
interrupted or stops on max_tokens; an infra error allows with a warning. Dead owners' files are removed; a file
under this process's own pid (a new session slug after `/clear` or a resume) is ours, not a peer's. Any `git` command
also gets the drained notices and divergence line

## docs-plan-tick / docs-plan-reminder

A landed commit ticks the plan tasks its `owns:` covers (`claude-code plan status`), in the newest open plan it
touches (an older plan only with a live claim); `git commit -q` prints no sha, so it ticks nothing (`plan tick
--range`); partly landed tasks are reported once at Stop under one shared partials lock, not per-session locks.
`claude-code clean` sweeps obsolete `*.partial.lock` files older than one day

## memory-update

Context steward first: at ≥`HARNESS_CONTEXT_STEWARD_PCT` context (default 75; `0` mutes only the prompt) it spawns a
detached `claude-code memory update` and asks for a handoff then `/compact`, once per crossing and only with no
background task, subagent marker or live dispatch (a dip below the threshold re-arms; never blocks Stop). Then the
memory update, weighing edits and user turns (2 turns = 1 edit); it never blocks Stop; a failed spawn prints
a NOT-saved note naming `claude-code memory update` (SessionEnd/PreCompact retry)

## worktree-busy

Vault env `HARNESS_WORKTREE`: unset asks when a peer joins the checkout (not this session's own tree, a linked
worktree, or an `agy --hub` daemon); `off`/`false`/`0` is silent. Never creates a worktree; no yes means keep working
in place. It also drains this session's notices (`state/harness/notices/`, ≤5) and, in a shared checkout diverged from
its upstream, adds one line per HEAD/upstream pair, whatever the mode
