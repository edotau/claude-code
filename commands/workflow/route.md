---
description: "Portfolio router — classify a task and dispatch the cheapest capable leg: Gemini 1M sweep, Codex second-opinion review, Haiku quick lookup, or Opus reasoning."
argument-hint: "[--kind sweep|review|mechanical|reasoning] [--dirs a,b] [--files g1,g2] <task>"
---

# /workflow:route — Dispatch Across the Agent Portfolio

Classify the task (or honor an explicit `--kind`) and dispatch the cheapest capable leg instead
of defaulting everything onto the session model:

| Kind | Leg | Why |
|---|---|---|
| `sweep` | `claude-code gemini bridge --dirs <d> "<task>"` | 1M window — one call instead of N greps |
| `review` | `claude-code ask --agent codex "<task>"` | a different model family catches blind spots a same-model self-review shares |
| `mechanical` | `claude-code ask --agent <runner> --model haiku "<task>"` | cheapest capable leg for lookups/one-liners |
| `standard` | in-session, or `code-worker` if dispatched as a fan-out leaf | ordinary scoped work — the classified DEFAULT; nothing buys Opus by silence |
| `reasoning` | `claude-code ask --agent claude --model opus "<task>"` | genuine design/debug reasoning — explicit-only, or keep it in-session |

Classify by the shape of the ask:

- Many files / whole-repo context needed → `sweep`
- A diff or a request wants an adversarial second opinion → `review`
- A one-liner, a lookup, a mechanical edit → `mechanical`
- Everything else → `standard` (handle it yourself, or fan out via `code-worker`)
- Explicitly hard reasoning the caller flags with `--kind reasoning` → `reasoning`

Pass `--dirs`/`--files` through to `claude-code gemini bridge` when the chosen leg is `sweep`, so
it has file context for the 1M-context pass. Doctrine on fan-out sizing and effort tiers:
`rules/workflow/agents.md`. Leg availability: `claude-code agents`.

When your own judgment disagrees with the `--kind` the caller passed, say so and route by
judgment instead — `--kind` is a hint, not a gate.
