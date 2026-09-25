# Cross-Model Agent Teams

Recruit another model family (Codex, Gemini, OpenCode, Copilot) only when a different model or an
additional independent workstream materially improves the result.

## Decision

1. Run `claude-code agents` to see which runners are available (binary on PATH, provider credential
   present). Availability is not proof of a working credential.
2. If authentication is uncertain, smoke-test the leg before substantive work:
   `claude-code ask --agent <leg> "Reply READY only."`
3. Use a team when the task has independent domains, needs broad-context exploration, or benefits from
   an independent review perspective.
4. Stay with one agent for small, coupled, or single-file work.

Failures are fail-soft: use a local Claude subagent rather than blocking the task.

## Role guide

| Leg | Best use | Entry point |
|---|---|---|
| Claude | Manager, synthesis, architecture, final decisions | Local `Agent`/`Workflow` tools |
| Codex | Scoped implementation or adversarial diff review | `claude-code ask --agent codex "<brief>"` |
| Gemini | Large-context repository exploration and cross-file analysis | `claude-code ask --agent gemini "<brief>"` |
| Gemini (bridge) | One-shot 1M-context sweep over inlined workspace files, no vendor CLI | `claude-code gemini bridge --dirs <d> -- "<task>"` |
| OpenCode / Copilot | Independent implementation or second-opinion review | `claude-code ask --agent opencode\|copilot "<brief>"` |

`--format json` returns the machine envelope (agent, provider, model, answer, usage). A leg's model and
provider come from the registry: `claude-code providers`, `claude-code models`. Interactive sessions on
another vendor CLI: `claude-code run codex|gemini|opencode|copilot`.

The Gemini bridge needs only `GEMINI_API_KEY` set (in `env.d/secrets.env`) — no `gemini` CLI on PATH.

## Team rules

- Apply the dispatch, ownership, and verification rules from [SKILL.md](../SKILL.md).
- Run independent read-only legs in parallel (one Bash call each, backgrounded, or one Workflow step each).
- Run writer then reviewer sequentially; reviewers need the completed artifact.
- Legs on distinct providers add throughput; legs sharing one provider share its rate limit, so reduce
  concurrency if `429` errors persist.

A useful default for a large change: Claude manages, Gemini maps broad impact, Codex performs an
independent review, and Claude resolves findings and verifies the final result.
