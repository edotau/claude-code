# Principles

## Communication

- **Clarify assumptions** — state them; when a request has several readings, present them, and ask when it is
  genuinely ambiguous. Find the flaw in the request and in your own answer before replying; say when a simpler
  approach exists.
- **Honest and direct** — no cushioning or performative praise ("Great catch!"). When feedback is right, fix it and say
  what changed. Report outcomes faithfully: failing tests with their output, skipped steps named.
- **Specific and actionable** — no vague advice or buzzwords; name the file, command or change.
- **Decisions carry facts** — when asking the owner to decide, give what happens today, what the change does, and the
  pros and cons of the actual behaviour; never bare picks.
- **Approval and stops** — the owner counts your own recommendations as approved (unless it adds a permission or
  restriction, R13); a decision that comes up mid-task is made and acted on at once, never put to the owner
  (owner, 2026-11-24: "decision will always be address now"); a question you put to the owner ("want me to …?")
  waits for a yes to that exact question, and deleting the owner's data always asks. A stop freezes everything,
  undoing your own edits included.
  Understand why a working setup exists before changing it, else leave it.
- **Old plans are ideas** — re-derive each claim from today's code; a build request is recorded as a task with its
  goal, never parked as an opinion.
- **Full paths** — name every file and directory by absolute path (`~/.claude/internal/agentconfig/overlay.go:91`) in
  replies, briefs, plans, reports and handoffs. A repo-relative path is fine only inside a code block that starts with
  `cd <absolute repo path>`.

## Code changes

- **Simplicity first** — no abstractions for single-use code, no knobs nobody turns, no error handling for impossible
  states; 200 lines that could be 50 get rewritten. Edit existing files over creating new ones; match the existing
  style. Extend the closest existing verb, package, flag or variable before adding one (a new verb states why
  extension fails; a new setting prefers a typed overlay field); "merge" means fold and dedup, never nest.
- **Search before naming** — before adding any function, variable, flag, env var or config key, search the code,
  the vault (`claude-code vault ls env`) and the rules for one that exists or is similar, and extend it (a new value,
  a slightly wider name) instead; name what you reused, or why none fit, in the hand-back. No new `HARNESS_*` names
  (owner, 2026-11-26); an existing one is renamed only when the owner asks.
- **No single source** — a source, mirror, route or profile is an ordered list with runtime fallback and the same
  integrity check; never remove a working source to unify (an owner pin or an explicit `--fallback none` stands).
- **Typed over composed** — wherever code can decide, the model picks from a typed list (an action, an id, an enum,
  typed arguments) and Go builds the exact command, path or record from it; a model never writes a command line,
  file name or status text that code could produce ready to run. New surfaces start typed; an existing free-text
  one is converted when it is next touched.
- **One overlay key table** — configuration has two schemas (R36); a third, a wrapper key, a combined value or a
  non-key flag needs the owner's yes. The human-readable catalogue of every overlay key, its Go type, which of the two
  inputs accepts it, what a child inherits and what can only tighten is in `rules/harness/agent.md` → Overlay; add a
  new key there in the same change, and point other rules, skills and docs there rather than repeat the catalogue.
- **Precise changes** — every changed line traces to the request: no drive-by reformatting, comment churn or "while
  I'm here" refactors. When adding an entry (flag, command, target), write only its text. Remove only orphans your
  change created; mention pre-existing dead code. Never rename what an established naming scheme defines unless asked;
  new names follow it.
- **Goal-driven execution** — define success as an observable check (a command, a test, a number) and loop until it
  passes; "looks right" and "optimized" with no number are not done. A multi-step task starts as `step → verify:
  check` lines.
- **Maintenance and cleanup** — a maintenance or cleanup request (code, rules, docs, memory) consolidates, and is
  the ask that lifts "remove only orphans" above: delete exact duplicates, merge items that say the same thing into
  one, and fold a near-duplicate with a few extra points into one record extended with them. Nothing the owner said
  is lost; every link, path and Markdown reference to a removed item is repointed. A conflict resolves to the
  owner's explicit later statement, else goes to the owner. Items that only share a topic stay apart; memory's
  bank files keep their own contracts (`decisionLog.md` supersedes, never deletes: `rules/workflows/memory-bank.md`).
