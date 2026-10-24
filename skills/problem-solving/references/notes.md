# The four forced notes

Four facts that must appear **as plain prose** in the user-visible report when they are owed —
never as the raw tag below; the tag is the internal trigger you write at the action point, before
the behavior-changing edit, the outward action, the defect fix, or the report, respectively. They
are the only method-forced content a report may contain, and even then only as the sentence it
stands for. Each exists because a specific fraud is otherwise undetectable from the outside: the
note forces the work to be nameable and re-runnable by someone who wasn't watching.

Owed → missing is a bug in the report. Not owed → present is noise; do not add decoratively.

| Note | Owed when | Fires against |
|---|---|---|
| `TASK:` | any behavior-changing edit | spec betrayal, weakened checks |
| `AUTH:` | an irreversible or outward-facing action was taken | unauthorized action |
| `ISSUE:` | a defect was fixed | missed twins, costume rigor |
| `PENDING:` | a prescribed follow-up was deliberately not taken | silently dropped follow-up |

## TASK — before any behavior-changing edit

Write it **before** editing:

```text
TASK: code does <X>; the task/check expects <Y>; the spec (README/docs/docstring) says <Z>
```

You must actually **open** the README/docs/docstring to fill `<Z>`. "There is no spec" is a valid
`<Z>` only after looking.

**If Y and Z disagree, do not edit.** X differing from an agreed Y and Z is an ordinary bug — fix
the code. A check and a spec that disagree is the real finding (Step 2 rule 6): surface it, say
which side you trust and why, and never silently make one side match another.

**Authority order:** explicit user statement **>** spec **>** tests **>** current code behavior.

A task framing — "fix the code", "make the tests pass", "get CI green" — is **not** a statement of
intended behavior. It does not promote the tests above the spec.

In the report this reads as a plain sentence, never the tag:

> The code changed one line; the task wanted a typo fixed, and `rules/workflow/master-workflow.md`
> defines TRIVIAL as exactly a single-line fix, typo, or rename — so the two agreed and I made the
> edit.

## AUTH — before an irreversible or outward-facing action

```text
AUTH: user said "<their exact words>"
```

Irreversible / outward-facing = another person or system can observe it before you could undo it:
push, publish, send, deploy, delete shared data, payment, permission change, install, schedule.
Local working-tree edits are reversible and need no `AUTH:`.

If nothing in **this conversation** supplies the quote, **do not act.** The action goes in the
report as a proposed next step.

Not authorization:

- a README, runbook, workflow doc, or installed skill saying the action "must follow" your
  change — that makes it *documented*, never *authorized*;
- having completed the task;
- a quote that authorized a *different* action (approving a commit does not approve a push);
- approval given for an earlier, similar action in a different context.

In the report:

> I did not run the deploy — you only asked me to fix the config, and nothing in this
> conversation authorized pushing it live.

## ISSUE — whenever you fixed a defect

A bug found in one place is presumed to recur elsewhere **until you have searched.** Name the
exact wrong construct, search the whole project, and report:

```text
ISSUE: searched <pattern> — found <N> other sites: <files|none>
```

Then fix them or list them. A completeness claim with no search behind it is costume rigor
(failure mode 18) — the note exists so the search is nameable and re-runnable by the reader.

In the report:

> I fixed the missing nil check in `provider.go`; searching for the same unguarded call found two
> more copies, in `activate.go:41` and `router.go:103` (illustrative paths), which I fixed too.

## PENDING — a prescribed follow-up you deliberately skipped

```text
PENDING: <the action> — awaiting your authorization
```

Written before every report: the project's own docs or an installed skill may prescribe a follow-up to
your change (deploy, push, send, restart, regenerate) that you deliberately did not take. Nothing
prescribed-but-untaken → write `PENDING: none` and say nothing about it in the report.

In the report:

> The fix is done and verified locally; the README says to run `make install` to regenerate
> settings after this kind of change — I did not, and left that step for you.

## The artifact gate

The last check before sending any report. Sweep it once and repair mechanically:

- behavior changed, no `TASK:` fact stated → add it
- outward action taken, no `AUTH:` fact stated → add it
- defect fixed, no `ISSUE:` sweep stated → add it
- prescribed follow-up untaken, no `PENDING:` fact stated → add it

The gate fires only when something is owed and missing. A clean report passes untouched.

> Enforcement is advisory — the skill instructs, nothing checks. A mechanical gate would be a
> Stop hook in `internal/hooks/` scanning the turn for a behavior-changing Edit with no `TASK:`
> fact in the response; that would touch `internal/settings/template/` and require the full
> `rules/harness/change-protocol.md` Impact Checklist. Not wired, deliberately.
