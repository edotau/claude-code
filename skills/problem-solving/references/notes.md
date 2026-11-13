# The four forced notes

Four facts that appear **as plain prose** in the report when owed, never as the raw tag (the tag is the internal
trigger written at the action point). Owed and missing is a bug; not owed and present is noise.

| Note | Owed when | Fires against |
|---|---|---|
| `TASK:` | any behavior-changing edit | spec betrayal, weakened checks |
| `AUTH:` | an irreversible or outward-facing action was taken | unauthorized action |
| `ISSUE:` | a defect was fixed | missed twins, costume rigor |
| `PENDING:` | a prescribed follow-up was deliberately not taken | silently dropped follow-up |

## TASK — before any behavior-changing edit

```text
TASK: code does <X>; the task/check expects <Y>; the spec (README/docs/docstring) says <Z>
```

**Open** the README/docs/docstring to fill `<Z>`; "no spec" is valid only after looking.

**If Y and Z disagree, do not edit.** X differing from an agreed Y and Z is an ordinary bug: fix the code. A check
and a spec that disagree is the real finding (Step 2 rule 6): surface it, say which side you trust and why, never
silently make one match the other.

**Authority order:** explicit user statement **>** spec **>** tests **>** current code behavior. A task framing
("fix the code", "make the tests pass", "get CI green") is not a statement of intended behavior and does not promote
the tests above the spec.

> The code changed one line; the task wanted a typo fixed, and `rules/workflows/master-workflow.md` defines TRIVIAL as
> a single-line fix — so the two agreed and I made the edit.

## AUTH — before an irreversible or outward-facing action

```text
AUTH: user said "<their exact words>"
```

Irreversible / outward-facing = another person or system can observe it before you could undo it: push, publish,
send, deploy, delete shared data, payment, permission change, install, schedule. Local edits need no `AUTH:`.

No quote in **this conversation** → **do not act**; propose it as a next step.

Not authorization:

- a README, runbook or skill saying the action "must follow" your change (documented, not authorized);
- having completed the task;
- a quote for a *different* action (approving a commit does not approve a push), or for an earlier similar one.

> I did not run the deploy — you only asked me to fix the config, and nothing in this conversation authorized pushing
> it live.

## ISSUE — whenever you fixed a defect

A bug is presumed to recur until you have searched. Name the wrong construct, search the whole project:

```text
ISSUE: searched <pattern> — found <N> other sites: <files|none>
```

Then fix or list them. A completeness claim with no search behind it is costume rigor (failure mode 18).

> I fixed the missing nil check in `provider.go`; searching for the same unguarded call found two more copies
> (`activate.go:41`, `router.go:103`, illustrative), which I fixed too.

## PENDING — a prescribed follow-up you deliberately skipped

```text
PENDING: <the action> — awaiting your authorization
```

Written before every report: docs or a skill may prescribe a follow-up (deploy, push, send, restart, regenerate) you
did not take. Nothing untaken → `PENDING: none`, and say nothing in the report.

> The fix is done and verified locally; the README says to run `make install` to regenerate settings after this kind
> of change — I did not, and left that step for you.

## The artifact gate

The last check before sending (list in SKILL.md Step 6): add any owed fact that is missing; a clean report passes
untouched. Enforcement is advisory; a mechanical Stop hook in `internal/hooks/` would need
`internal/settings/hookspec.go` and the full `rules/harness/change-protocol.md` checklist. Not wired, deliberately.
