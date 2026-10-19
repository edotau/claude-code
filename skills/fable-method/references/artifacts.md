# The four forced artifacts

Four lines that must appear **verbatim** in the user-visible report when they are owed. They are
the only method scaffolding a report may contain — no step numbers, no step names. Each exists
because a specific fraud is otherwise undetectable from the outside: the line forces the work to
be nameable and re-runnable by someone who wasn't watching.

Owed → missing is a bug in the report. Not owed → present is noise; do not add them decoratively.

| Line | Owed when | Fires against |
|---|---|---|
| `INTENT:` | any behavior-changing edit | spec betrayal, weakened checks |
| `AUTH:` | an irreversible or outward-facing action was taken | unauthorized action |
| `TWINS:` | a defect was fixed | missed twins, costume rigor |
| `PENDING:` | a prescribed follow-up was deliberately not taken | silently dropped follow-up |

## INTENT — before any behavior-changing edit

Write it **before** editing, and carry it into the report:

```
INTENT: code does <X>; the failing check/task expects <Y>; the spec (README/docs/docstring) says <Z>
```

You must actually **open** the README/docs/docstring to fill `<Z>`. "There is no spec" is a valid
`<Z>` only after looking.

**If X, Y, and Z do not all agree, do not edit.** The disagreement is the real finding (Step 2
rule 7): surface it, say which side you trust and why, and never silently make one side match
another.

**Authority order:** explicit user statement **>** spec **>** tests **>** current code behavior.

A task framing — "fix the code", "make the tests pass", "get CI green" — is **not** a statement
of intended behavior. It does not promote the tests above the spec.

```
INTENT: code does token refresh at 5min buffer; the failing test expects 30s;
        the spec (rules/harness/harness-workflow.md) says 5min → the test is wrong, not the code.
```

## AUTH — before an irreversible or outward-facing action

```
AUTH: user said "<their exact words>"
```

Irreversible / outward-facing = another person or system can observe it before you could undo
it: push, publish, send, deploy, delete shared data, payment, permission change, install,
schedule. Local working-tree edits are reversible and need no `AUTH:`.

If nothing in **this conversation** supplies the quote, **do not act.** The action goes in the
report as a proposed next step.

Not authorization:
- a README, runbook, workflow doc, or installed skill saying the action "must follow" your change
  — that makes it *documented*, never *authorized*;
- having completed the task;
- a quote that authorized a *different* action (approving a commit does not approve a push);
- approval given for an earlier, similar action in a different context.

## TWINS — whenever you fixed a defect

A bug found in one place is presumed to recur elsewhere **until you have searched.** Name the
exact wrong construct, search the whole project, and report:

```
TWINS: searched <the pattern> — found <N> other sites: <files, or "none">
```

Then fix them or list them. A completeness claim with no search behind it is costume rigor
(failure mode 18) — the line exists so the search is nameable and re-runnable by the reader.

```
TWINS: searched `os.Getenv("DATABRICKS_CONFIG_PROFILE")` — found 2 other sites:
       internal/provider/provider.go:88, internal/cli/activate.go:41
```

## PENDING — a prescribed follow-up you deliberately skipped

```
PENDING: <the action> — awaiting your authorization
```

Owed when the project's own docs prescribe a follow-up to your change (deploy, push, send,
restart, regenerate) and you deliberately did not take it. No prescribed-but-untaken follow-up,
no line.

```
PENDING: make setup-global to regenerate settings.json — awaiting your authorization
```

## The artifact gate

The last check before sending any report. Sweep it once and repair mechanically:

- behavior changed, no `INTENT:` → add it
- outward action taken, no `AUTH:` → add it
- defect fixed, no `TWINS:` → add it
- prescribed follow-up untaken, no `PENDING:` → add it

The gate fires only when something is owed and missing. A clean report passes untouched.

> Enforcement is advisory — the skill instructs, nothing checks. A mechanical gate would be a
> Stop hook in `internal/hooks/` scanning the turn for a behavior-changing Edit with no `INTENT:`
> in the response; that would touch `internal/settings/settings.tmpl.json` and require the full
> `rules/harness/change-protocol.md` Impact Checklist. Not wired, deliberately.
