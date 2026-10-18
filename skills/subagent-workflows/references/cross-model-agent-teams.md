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
| Gemini (bridge) | One-shot 1M-context sweep over inlined workspace files, rides the subscription via `agy` when logged in | `claude-code gemini bridge --dirs <d> -- "<task>"` |
| OpenCode / Copilot | Independent implementation or second-opinion review | `claude-code ask --agent opencode\|copilot "<brief>"` |
| Any leg as a Workflow stage | Smoke-tested, JSON envelope relayed, sonnet fallback | `workflow("vendor-leg", {leg, cwd, task})` or `audit-fix-verify {refuters: ["codex"]}` |

Demonstrated calls per named role:

- **Codex adversarial refuter** — `workflow("audit-fix-verify", {range: "main..HEAD", cwd, refuters: ["codex"]})`;
  codex refutes each finding before fix ‖ test runs per package.
- **Gemini 1M sweep as a stage** — `workflow("vendor-leg", {leg: "gemini", cwd, task: "<sweep brief>"})`, one
  call per round inside a loop — never the opus tier.
- **Copilot commit tail** — `workflow("vendor-leg", {leg: "copilot", cwd, tools: true, task: "stage and commit
  <files> with <message>"})` — `tools: true` runs never fall back; call this only after the gates are green.
- **OpenCode second opinion** — `workflow("vendor-leg", {leg: "opencode", cwd, task: "<review brief>"})`, roughly
  1.5–3 min per call.

`--format json` returns the machine envelope (agent, provider, model, answer, usage). A leg's model and
provider come from the registry: `claude-code providers`, `claude-code models`. Interactive sessions on
another vendor CLI: `claude-code run codex|gemini|opencode|copilot`.

The Gemini bridge needs either a cached Google login (so `agy` carries the subscription) or
`GEMINI_API_KEY` set (in `env.d/secrets.env`) as the fallback.

## Team rules

- Apply the dispatch, ownership, and verification rules from [SKILL.md](../SKILL.md).
- Run independent read-only legs in parallel (one Bash call each, backgrounded, or one Workflow step each).
- Run writer then reviewer sequentially; reviewers need the completed artifact.
- Legs on distinct providers add throughput; legs sharing one provider share its rate limit, so reduce
  concurrency if `429` errors persist.

A useful default for a large change: Claude manages, Gemini maps broad impact, Codex performs an
independent review, and Claude resolves findings and verifies the final result.
