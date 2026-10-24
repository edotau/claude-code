# Verify before claiming done

problem-solving Step 5's gate chain: the iron law, the ordered gates, the bounded loop.

## Iron Law

Before claiming completion, run the verification command, read the output, then claim. Stale
results from earlier in the session don't count.

```text
IDENTIFY → RUN → READ output → VERIFY success → CLAIM
```

## Gate Sequence

Run in order. A failure blocks subsequent gates until fixed.

```text
Gate 1: FORMAT/LINT   — formatters + linters clean                         — every iteration
Gate 2: SCOPED TESTS  — the tests for the packages/modules touched, named  — every iteration
Gate 3: FINAL PASS    — full build + full test suite + security scan      — run ONCE, after an iteration is clean
Gate 4: AUTH/SEC      — auth verification (auth changes only); no hardcoded secrets — part of the final pass
Gate 5: DIFF REVIEW   — diff matches intent, no unrelated changes?        — part of the final pass
```

This harness: `make lint` → scoped `go test ./internal/<pkg>` per iteration; `make test` only in Gate 3.
If `internal/settings/template/` was touched, also render it (`claude-code settings`) and diff
the output against the live `settings.json` as part of Gate 3.

## Gate Loop (max 5 iterations)

Scripted form: `Workflow green-keeper-loop` (only when the user asks for one;
`../../subagent-workflows/references/saved-workflows.md`).

Each iteration runs Gates 1-2 only — cheap, scoped to the packages the change touched — fixing
failures minimally. Once an iteration is clean, run Gates 3-5 (the full build + full test suite +
security scan + diff review) exactly ONCE as the final pass. Only a failing final pass sends the
loop back to Gate 1; after 5 iterations total, stop and escalate to the user with the remaining
failures. This is the brief for codex's final gate run (`claude-code codex ask --tools`) and matches
`rules/standards/quality.md`'s Verification Order — the expensive run happens once, never per iteration.

Per-gate fix strategy:

- **Format** — auto-fix and re-run.
- **Lint** — fix only the flagged lines.
- **Scoped tests** — fix the code, not the tests (unless the test itself is wrong).
- **Final pass** — if it fails, trace the failure to its package and fix minimally, then re-run
  from Gate 1; do not re-run the full suite again until the next iteration is clean.

## Report Format

```text
VERIFICATION REPORT (iteration N/5)

| Gate         | Status             | Details                                   |
|--------------|--------------------|--------------------------------------------|
| Format/Lint  | PASS/FAIL          | <output summary>                          |
| Scoped Tests | PASS/FAIL          | <packages>: N passed, N failed            |
| Final Pass   | PASS/FAIL/PENDING  | full build + full test suite + security scan |
| Auth/Sec     | PASS/SKIP          | <findings>                                |
| Diff         | PASS/FAIL          | <scope check>                             |

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
