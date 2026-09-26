# Enforcement patterns — four levels

The discipline lens is only as good as how reliably it runs. Pick the level that matches how
much friction the team will accept. Higher levels catch more but cost more.

| Level | Mechanism | Catches | Friction |
|-------|-----------|---------|----------|
| 1 Passive | skill loads as context | self-review awareness | none |
| 2 Active | `/code-review` before commit | quality + security, on demand | a command |
| 3 Opt-in gate | `claude-code review gate` pre-commit | mechanical violations at commit time | one line |
| 4 CI | detectors in PR checks | every PR, no human in the loop | CI config |

---

## Level 1 — Passive

The skill is installed; the four principles load when the description triggers. Review
relies on the reviewer (human or agent) remembering to apply the lens. No setup. Good
baseline; weakest guarantee.

## Level 2 — Active (`/code-review`)

Run `/code-review` before committing. It scopes the diff, runs the detectors, fans out a
quality reviewer + a subagent running the `security-reviewer` skill in
parallel, and synthesizes a SHIP / FIX REQUIRED / BLOCK verdict. This is the recommended
default — explicit, on demand, no standing config.

## Level 3 — Opt-in gate (`claude-code review gate`)

A non-blocking advisory that runs the simplicity + surgical detectors on staged files and
prints findings. **Always exits 0** — it never blocks the commit; the goal is awareness. It
is deliberately **not** wired into the generated `settings.json`.

**Option A — git pre-commit (per repo):** a one-line hook containing `exec claude-code review gate`:

```bash
printf '#!/bin/sh\nexec claude-code review gate\n' > .git/hooks/pre-commit
chmod +x .git/hooks/pre-commit
# or call it from an existing husky / pre-commit chain
```

**Option B — Claude Code PreToolUse.** Add the hook to `internal/settings/settings.tmpl.json`, then
re-render `settings.json` with `claude-code install` (never hand-edit the generated file):

```json
{ "matcher": "Bash",
  "hooks": [ { "type": "command",
    "command": "claude-code review gate" } ] }
```

Because the gate exits 0, even at this level it is awareness, not a block. To actually
block, gate in CI (Level 4).

## Level 4 — CI gate

Run the detectors as a PR check. They exit 0, so enforce by parsing the JSON verdict and
failing the job yourself.

```yaml
# .github/workflows/code-review.yml (sketch)
- name: complexity
  run: |
    out=$(claude-code review complexity $(git diff --name-only origin/main...HEAD) \
            --threshold medium --json)
    echo "$out"
    [ "$(echo "$out" | jq -r .verdict)" != "FAIL" ]
- name: surgical
  run: |
    claude-code review diff --diff origin/main...HEAD --json
```

Tune which verdicts fail the build (`FAIL` only, or `WARN` too) to the team's tolerance.

---

## Choosing a level

- Solo / fast iteration → Level 1–2.
- Team repo, want a nudge without blocking → add Level 3.
- Regulated or high-blast-radius repo → Level 4, fail on CRITICAL/HIGH from the security
  pass and on detector `FAIL`.

The detectors never block on their own — that is intentional. Blocking is a policy
decision; make it explicitly in CI, not as a side effect of an advisory script.
