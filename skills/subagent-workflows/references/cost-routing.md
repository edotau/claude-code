# Cost Routing and Cross-Model Legs

Claude stays the manager. A step delegated through `claude-code ask` bills to that runner's account; Claude
spends only the brief and the check. Use another family when it is cheaper for the step, or when a second model
adds real coverage (independent review, broad-context sweep). Stay inline for small, coupled work.

## Classify, then route

| Kind | Signal | Runner | Call |
|---|---|---|---|
| `sweep` | many files, whole-repo context, web search | `gemini` (agy, Google sign-in) | `claude-code gemini explore "<question>"`; targeted: `claude-code ask --agent gemini --tools "<brief>"`; 1M context: `claude-code gemini bridge --dirs <d> "<task>"` |
| `review` | adversarial second opinion on a diff or plan | `codex` (ChatGPT plan) | `claude-code ask --agent codex --format json "<brief>"`; a diff: `claude-code codex review --adversarial [--base <b>]` |
| `mechanical` | one-liner, lookup, mechanical edit | `copilot` `--model claude-haiku-5.5` | `claude-code ask --agent copilot --tools --workdir <repo> --fallback none --format json "<brief>"` |
| `standard` | everything else (the default) | rotate `claude` · `copilot` (`claude-sonnet-5.5`) · `codex` · `opencode` | as above, `--agent <runner>` |
| `reasoning` | explicitly flagged hard reasoning | Claude `opus` | stays in session; never by default |

Docs drafting goes to `opencode`. Architecture, judgement, integration and final verification never leave Claude.
`--kind` from a caller is a hint: route by judgement and say so when you disagree.

Gemini needs `--tools` for any repo or web work — without it agy answers from model memory. `gemini-4-argon` is its
`opus` tier (second-opinion review, hard sweeps); `flash-high` is the default.

## Run the leg

1. `claude-code agents` lists available runners (✓ = binary + credential). Availability is not a working
   credential: smoke-test once with `claude-code ask --agent <r> "Reply READY only."`.
2. Assign runners round-robin across implementation steps and name it in the brief as `runner: <name>`; keep the
   split even across the session. Assign `runner: claude` when the brief would cost more than the work, the step
   needs context only this session holds, or the runner failed its smoke test.
3. Size the model: Copilot `claude-haiku-5.5` mechanical, `claude-sonnet-5.5` default, `claude-opus-5.5` only for an
   unusually complex step (premium multipliers scale). `claude-code models --live --provider github-copilot` lists
   what the seat serves.
4. Use `--fallback none` so a failed leg exits non-zero instead of silently running on Claude. Alternatives, by hand:
   codex limited → `copilot --model gpt-6.1-sol`; copilot down → opencode; agy down → copilot; Claude last.
5. A vendor leg in a shared tree runs in its own clean copy: `skills/subagent-workflows/scripts/isolate.sh <copy>`,
   `--workdir <copy>`, then apply and verify its diff (opencode has ignored `--workdir` and written the live tree).

Independent read-only legs run in parallel (one backgrounded Bash call each); a writer then its reviewer run in
sequence. Legs on one provider share its rate limit — reduce concurrency on persistent `429`s.

**Default runner per task class**

| Task class | Runner |
| --- | --- |
| sweep / research | `gemini` |
| mechanical edits | `copilot` |
| standard edits, docs | `opencode` |
| review / vet | `codex` |
| reasoning / integration | `claude` (in-context) |

## Verify and compare

`--format json` returns `agent`, `provider`, `model`, `usage`, and `fallback`/`fallback_reason` when another runner
stood in — a non-empty `fallback` means the saving did not happen; report it. Treat the answer as a claim: check `git
diff --stat` for the expected files and run the verification yourself. Each leaf's report carries the contract's `Ran
on:` line plus first-try pass/fail and hand fixes (`../SKILL.md` → Handoff contract); tally per runner (`claude-code
ask --log`) and shift the split only on that evidence. A leaf assigned a runner that did the work itself is a skipped
leg, not a success.

The vendor CLI starts cold: paste every fact it needs; in `~/.claude` add the shared-tree line from SKILL.md.
As a `Workflow` stage (only when the user asks for one): `code-agent {leg, cwd, task}`, or
`code-analyzer {refuters: ["codex"]}`. Interactive vendor session: `claude-code codex|gemini|opencode|copilot
[args]` (`--` forces passthrough).
