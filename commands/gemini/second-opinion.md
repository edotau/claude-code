---
description: "Whole-module architecture review via long-context Gemini"
argument-hint: "[--dirs path,...] [--files pattern,...] [review focus]"
allowed-tools: Bash, Glob, Read
---

# /gemini:second-opinion — Long-Context Module Review

Packs whole modules (`--dirs`) into a single Gemini **opus-tier** call (`gemini-3.1-pro-preview`) and
asks for an architecture/consistency review. The niche: Gemini's context window holds an entire module
at once, so it sees cross-file drift — inconsistent naming, duplicated logic, layering violations —
that per-file review misses. Costs zero Claude token budget.

## Usage

```bash
/gemini:second-opinion --dirs internal/hooks,internal/cli review the hook dispatch layering
/gemini:second-opinion --dirs internal/vendors --files "rules/software/go/*.md" does the code match the documented conventions
/gemini:second-opinion --dirs internal/providers,internal/router   # default focus: architecture + consistency
```

## Execution Instructions

Parse `$ARGUMENTS` into `DIRS` (`--dirs`, default: the directory most relevant to the current
task — ask if ambiguous), `FILES` (`--files`, optional), and `FOCUS` (remaining text, may be
empty). Then invoke the shared bridge exactly as `/gemini` does:

```bash
claude-code gemini bridge --model opus \
  --dirs <DIRS> [--files <FILES>] -- "<TASK>"
```

where `<TASK>` is:

```
Task: second-opinion architecture and consistency review of the packed modules.
Priorities, in order:
1. Cross-file inconsistencies: divergent naming, duplicated logic, contradictory conventions
2. Layering/boundary violations: imports or responsibilities that cross module lines
3. Dead or orphaned code paths visible only with the whole module in view
4. Doc drift: comments or docs that no longer match the code
Rules: report findings only, no praise, no summary of what the code does.
For each finding: <file:line> <one-line issue> <one-line suggestion>.
If clean, say exactly: VERDICT: no actionable findings.
<FOCUS, if any: "Additional focus: ...">
```

Relay Gemini's findings back verbatim under a `## Gemini second opinion` heading, then add
your own one-paragraph assessment of which findings you agree with.

## When to use

- **After a multi-file change lands** — verify the module still reads as one design
- **Before refactoring an unfamiliar module** — map inconsistencies first
- **Doc/code drift sweeps** — pack `rules/` alongside the code they describe
- **As the long-context leg of a cross-provider review team** — pair with another
  model's diff-focused review while Claude holds the main thread

Not for: single-file review (use `claude`), diff review (use `/code-review` or `/security`), or
interactive debugging.

## Notes

- The bridge collects context first, then makes **one** deterministic call — keep `--dirs`
  scoped to the modules under review.
- Model is pinned to the opus tier here on purpose — the sonnet/haiku tiers miss the
  cross-file synthesis this command exists for. Override with judgment only.

## Related

- `/gemini` — general-purpose bridge invocation (syntax, backends, error table)
- `/code-review`, `/security` — the local review counterparts
- `skills/subagent-workflows/references/cross-model-agent-teams.md` — where this fits in a cross-model team
