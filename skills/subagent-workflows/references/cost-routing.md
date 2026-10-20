# Cost Routing — Spread Steps Evenly Across Runners

Claude Code stays the manager. Steps are spread evenly across the available runners — Claude in-context
is one of them — so usage is shared between accounts and the results can be compared per runner. A
delegated step runs through `claude-code ask` and bills to that runner's account; Claude spends only the
brief and the check.

## Decide per step

1. `claude-code agents` — which runners are available on this box (✓ = binary + credential found).
2. Each row lists the runners suited to that step kind:

| Step kind | Runners (bills to) | Call |
|---|---|---|
| Scoped implementation, mechanical edits, test repair | rotate: `claude` (in-context) · `copilot` (premium requests) · `codex` (ChatGPT plan) · `opencode` (Copilot `claude-opus-5.5`, medium, via its ladder) | `claude-code ask --agent <runner> --tools --workdir <repo> --fallback none --format json "<brief>"` (copilot: `--model claude-sonnet-5.5`) |
| Adversarial diff review | `codex` (ChatGPT plan) | `claude-code ask --agent codex --format json "<brief>"` |
| Read-only sweep, cross-file map, local lookup, web search | `gemini` (Google sign-in via agy) | `claude-code ask --agent gemini --tools "<brief>"` (web too: keep `--tools`, ask for URLs) · 1M context: `claude-code gemini bridge` |
| Doc drafting | `opencode` | `claude-code ask --agent opencode --tools "<brief>"` |
| Architecture, judgement, integration, final verification | Claude (this session) | stays here — never delegated |

3. **Distribute evenly.** The manager assigns runners round-robin across the suitable, available ones
   for implementation steps (e.g. 4 leaves → claude, copilot, codex, opencode), names it in each handoff as
   `runner: <name>`, and keeps the split even across a session rather than per batch. Judgement and
   integration stay with Claude regardless.
4. Size the model to the step: Copilot's `claude-haiku-5.5` for mechanical edits, `claude-sonnet-5.5`
   by default, `claude-opus-5.5` only for an unusually complex step (premium multipliers scale with the
   model). `claude-code models --live --provider github-copilot` lists what the seat serves.
5. The manager (not the leaf) assigns `runner: claude` when the step is tiny (the brief costs more
   than the work), tightly coupled to context only this session holds, or the runner failed its
   smoke test (`ask --agent <r> "Reply READY only."`).

## Alternative when a runner is unavailable

`internal/agent.FallbackChain` already orders this (codex/gemini → copilot → opencode → claude;
copilot/opencode → the other vendors → claude last). In a lane, re-dispatch on the alternative
explicitly instead of relying on it (`--fallback none`, per above): codex quota → `copilot --model
gpt-6-astra`; copilot down → opencode; gemini (agy) down → copilot; Claude only when nothing else answers.

## Verify the leg actually ran

`--format json` returns the envelope: `agent`, `provider`, `model`, `usage`, and `fallback` /
`fallback_reason` when another runner stood in. A non-empty `fallback` means the saving did not happen —
report it. `--fallback none` makes a failed leg exit non-zero instead of silently running on Claude, so the
caller decides (do the step in-context, or retry later).

## Compare results

Every leaf reports `runner`, `model`, `fallback` (if any), verification pass/fail on first try, and how
much it had to fix by hand. The manager tallies these per runner in its final report; `claude-code ask
--log` lists the vendor dispatches. Shift the split only on evidence from that tally.

Treat the answer as a claim: inspect `git diff --stat` for the files it should have touched and run the
step's verification command yourself.

A leaf that was assigned a runner but did the work itself counts as a skipped leg, not a success - report why.

## Brief

Same contract as any dispatch ([SKILL.md](../SKILL.md)): one objective, the files it may touch, the
verification command, expected output. In `~/.claude` add: never `git stash`/`checkout`/`reset`/`restore`.
The vendor CLI starts cold — paste the facts it needs; it cannot see this session.
