---
name: approval-gate
description: Pre-completion verification (harness variant of the built-in /verify) — the iron law (no completion claims without fresh evidence) plus the ordered gate loop. Triggers on "verify before done", "run the gates", "is this complete", "pre-completion check", "are we done", "claim complete", before commit/push/PR. This is the harness pre-completion gate chain; not the built-in /verify (runtime behavior).

metadata:
  version: 1.0.0
---

# Verification

Two protocols: the iron law (no claims without fresh evidence) and the verification loop (multi-gate sequence).

## Iron Law

Before claiming completion, run the verification command, read the output, then claim. Stale results from earlier in the session don't count.

```
IDENTIFY → RUN → READ output → VERIFY success → CLAIM
```

## Gate Sequence

Run in order. A failure blocks subsequent gates until fixed.

```
Gate 1: FORMAT/LINT — formatters + linters clean
Gate 2: TESTS       — full test suite green
Gate 3: AUDIT       — full project gate (deps, static analysis, vuln, drift)
Gate 4: AUTH/SEC    — auth verification (auth changes only); no hardcoded secrets
Gate 5: DIFF REVIEW — diff matches intent, no unrelated changes?
```

This harness: `make lint` → `make test`. If `internal/settings/settings.tmpl.json` was touched, also
render it (`claude-code settings`) and diff the output against the live `settings.json`.

## Gate Loop (max 5 iterations)

On any failure: fix it, then re-run from Gate 1. After 5 iterations, stop and escalate to
the user with the remaining failures.

Per-gate fix strategy:

- **Format** — auto-fix and re-run.
- **Lint** — fix only the flagged lines.
- **Tests** — fix the code, not the tests (unless the test itself is wrong).
- **Audit** — read the failing check's rule first, then fix.

## Report Format

```
VERIFICATION REPORT (iteration N/5)

| Gate        | Status    | Details                |
|-------------|-----------|------------------------|
| Format/Lint | PASS/FAIL | <output summary>       |
| Tests       | PASS/FAIL | N passed, N failed     |
| Audit       | PASS/FAIL | <output summary>       |
| Auth/Sec    | PASS/SKIP | <findings>             |
| Diff        | PASS/FAIL | <scope check>          |

Verdict: PASS / FAIL (Gate N)
```

## Escalation

- 5 loop iterations exhausted → escalate to user with remaining failures
- Never skip a failing gate without documenting why
- Migration needed → stop and ask user to run it manually

## Claim Discipline

`rules/standards/communication.md` carries the honesty rule; this is its pre-completion form:
**if you haven't run the verification command in *this* message, you cannot claim it passes.**
Stale output, "should work now", "I'm confident", "the linter passed", and an agent's own success
report are all non-evidence — verify a subagent's work from the VCS diff, not its summary.

Claims whose evidence is *not* just the obvious command:

| Claim | Requires |
|-------|----------|
| Bug fixed | The original symptom re-tested — not "code changed, assumed fixed" |
| Regression test works | Red-green cycle verified — not one green run |
| Agent completed | VCS diff showing the changes |
| Requirements met | Line-by-line checklist — tests passing is not it |

**STOP** on "should"/"probably"/"seems to", or on satisfaction voiced before verification
("Done!", "Perfect!"). Paraphrases and implications count, not just these exact words.
