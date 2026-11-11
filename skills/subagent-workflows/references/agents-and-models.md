# Agents and Models: What Runs Where

Serves `../SKILL.md` Phase 2 (dispatch). Written 2026-11-11 from the code and from measurements; a claim not checked
is marked "unverified". Roles and the ladder: [cost-routing.md](cost-routing.md); verbs:
[agent-verbs.md](agent-verbs.md).

## 1. Pick: task to agent, model, workspace, flags

Workspaces `qa` and `prod` measured the same; `dev` is the 429-prone one for GPT (section 6). Model ids are the
Databricks endpoint names (`claude-code opencode catalog list`).

| Task | Agent | Model | Workspace | Flags |
| --- | --- | --- | --- | --- |
| Edit and verify (this repo, `~/.claude`) | `opencode` | `databricks-claude-sonnet-5-5` | `qa` or `prod` | `--tools --fallback none` |
| Edit and verify (any other repo) | `claude` or `opencode` | `sonnet` / `databricks-claude-sonnet-5-5` | `qa` or `prod` | `--tools --fallback none` |
| Read-only audit | `opencode` (fixed today, section 6) | `databricks-claude-sonnet-5-5` | `qa` or `prod` | `--read-only --fallback none` |
| Read-only audit, GPT | `copilot` | `databricks-gpt-6-1-sol` | `prod` | `--read-only --fallback none` |
| Second-opinion review | `codex` | `gpt-6.1-sol` (ChatGPT login) | own login | `claude-code codex review --adversarial` |
| Long-context sweep (1M window) | `gemini` | `gemini-4-argon` | Google login | `--tools --dirs <d> [--index]` |
| Cheap mechanical edit | `opencode` | `databricks-claude-haiku-5-5` (unmeasured) | `qa` or `prod` | `--tools --fallback none` |
| GPT-family run | `codex` | `databricks-gpt-6-1-sol` | `prod` (5% refusals on 2026-11-10) | `--tools --route databricks:prod` |

- Edits in this repo never go to `claude` on Databricks: it cannot write here (section 6). Evidence for the first
  rows: tools-on benchmark 2026-11-10, opencode 18/18 passes, median 14 s; claude after the Bash fix 18/18, 12-13 s
  (`memory/claude-code/reports/2026-11-10-databricks-model-benchmark.md`, rounds 2 and rerun).
- Codex leaves a Go build cache of about 1,200 files in the working directory (8 of 9 runs): use a scratch workdir.
- The haiku row is by the model tables only; no run of it is recorded. Accuracy between agents was not separated.

## 2. Agents

Registration rows: `internal/{claude/claude.go, codex/register.go, copilot/copilot.go, gemini/gemini.go,
opencode/register.go}`; `claude-code agents list --format json` prints the flag subset each honors. Pins today:
`claude-code <agent> profile` reports codex `prod`, opencode `dev`, copilot `personal` (GitHub), gemini `antigravity`.

| Agent | What it is | Authenticates with | Base prompt comes from |
| --- | --- | --- | --- |
| `claude` | Claude Code CLI, the in-context default leaf | `claude auth login` (Anthropic) or a Databricks profile | `~/.claude/CLAUDE.md`, rules, skills, natively |
| `codex` | OpenAI Codex CLI | `codex login` (ChatGPT) or a Databricks profile | `~/.codex/AGENTS.md`, rendered by `agents sync` |
| `copilot` | GitHub Copilot, run through its SDK for `ask` | `copilot login` (GitHub); Databricks by BYOK proxy | SDK appended text plus instruction dirs |
| `gemini` | Google Antigravity (`agy` binary) | `agy` Google login; a Databricks gateway row on `--profile` | `~/.gemini/GEMINI.md`, rendered by `agents sync` |
| `opencode` | opencode CLI, the model-menu agent | `opencode auth login`, or a Databricks profile via a tracked overlay | `~/.claude/CLAUDE.md` natively |

Sources for the last column: `rules/harness/agent.md` -> Skills and instructions parity. Credentials sit in the vault
or a provider's `auth.command` only (`rules/harness/providers.md`); `--as user|sp|auto` picks a Databricks identity.

Capability matrix (`agent.Caps` rows; N = native, E = emulated by a harness shim, - = unsupported, so the flag is
dropped and named in the envelope's `dropped`):

| Cap | claude | codex | copilot | gemini | opencode |
| --- | --- | --- | --- | --- | --- |
| Tools (`--tools`) | N | N | N | N | N |
| ReadOnly (`--read-only`) | - | - | N | E | E (was - until 2026-11-11 11:14) |
| Autonomous | N | - | N | N | N |
| SafeMode | N | N | - | - | N |
| Permissions (`--allow/--deny`) | N | N | N | N | N |
| Effort | N | N | N | N | N |
| SessionID (`--session`) | N | N | N | N | N |
| OutputSchema | N | N | E | N | E |
| Persona (agent file) | N | E | E | E | E |
| SystemPrompt / Append | N / N | N / N | N / N | E / E | N / E |
| NoTools / NoPersist | N / N | - / N | - / - | - / - | - / - |
| Fleet / Memory | - | - | N / N | - | - |
| Live session (`--live`) | N | - | - | N | - |

Read-only on an agent marked "-" drops its tools (fail closed): `internal/agent/run.go` `applyCaps`. Per-call
overhead, tools-off prompt "Reply with exactly: 4242", 2 runs per cell, 2026-11-11, from the baseline
(`.../scratchpad/dbx/base.jsonl`, command `agents ask --route databricks:<ws> --fallback none --format json`):

| Agent (model) | Input tokens reported | Cache read | Wall median | Wall range (6 runs) |
| --- | --- | --- | --- | --- |
| claude (`sonnet`) | 308 | 6,178 on the warm run | 1.6 s | 1.5-1.7 s |
| copilot (`databricks-gpt-6-1-sol`) | 3 | not reported | 2.5 s | 2.2-3.8 s |
| codex (`databricks-gpt-6-1-sol`) | 24,586 (24,599 once) | not reported | 4.3 s | 3.7-4.6 s |
| opencode (`databricks-claude-sonnet-5-5`) | 4 | 12,411 when reported | 5.0 s | 3.9-6.2 s |

- Fixed prompt, not task size, drives the cost of small legs: claude about 6.5k, opencode about 12.4k, codex about
  24.6k tokens per call. Copilot's 3 looks like a count without the prompt (unverified). Gemini was not measured.
- 2026-11-10 benchmark, 6 tasks x 3 runs, tools off: claude 2.9 s median / 5.4k in; opencode on sonnet 4.4-4.5 s /
  13.1k; codex 4.5 s / 25.3k; opencode on `databricks-kimi-k3` and `databricks-glm-5-3` had 40.5 s and 18.4 s tails.
  All 156 calls were correct, so accuracy is not ranked.

## 3. Models and wire APIs

`claude-code opencode catalog list --live --profile <p>` on 2026-11-11: 52 menu entries; dev, qa and prod are identical
and each serves 51 (`deepseek-v4-pro-0813` shows `missing` on all three). `claude-code codex catalog --profile <p>`
lists four endpoints on each. Catalog source: `internal/models/catalog.json`; limits shown are its entries.

| Family on every workspace | Examples (endpoint = `databricks-<id>`) | Wire API |
| --- | --- | --- |
| Claude | `claude-{opus,sonnet,haiku}-5-5` (1M context, 128k output), 4.x and 5 | Anthropic Messages |
| GPT, Responses only | `gpt-6-1-sol`, `gpt-6-astra`, `gpt-5-5`, `gpt-5-5-pro` | Responses |
| GPT, other | `gpt-5`..`gpt-5-4*`, `gpt-6-luna`, `gpt-6-sol`, `gpt-oss-120b` (chat only) | Chat Completions |
| Gemini | `gemini-3-8-flash`, `gemini-3-7-flash`, `gemini-3-5-flash`, ... | Chat Completions |
| Open weights | `kimi-k3`, `glm-5-3`, `deepseek-v4-pro-0813`, `grok-4-7`, `qwen35-122b-a10b`, `llama-4-maverick` | Chat Completions |

Which agent can use which (`rules/harness/providers.md` -> Databricks routes; `rules/harness/agent.md`):

| Agent | Route on a workspace | Can run |
| --- | --- | --- |
| `claude` | `<ws>/serving-endpoints/anthropic` | Claude models; a Responses-only model is refused at launch |
| `codex` | `<ws>/serving-endpoints/responses` | the four Responses-only GPT models only |
| `opencode` | three providers per row: `<row>` (chat), `<row>-gpt` (Responses), `<row>-claude` (Anthropic) | the menu; an off-menu `--model` is refused |
| `copilot` | `<ws>/serving-endpoints/chat/completions` through the proxy registry | it ran `databricks-gpt-6-1-sol` today; wire unverified |
| `gemini` | Google, or a Databricks gateway row named by `--profile` | Gemini family; never a non-Google row otherwise |

Alias to model is per agent in `internal/router/table.json` (e.g. `sonnet` on claude on Databricks resolved to
`databricks-claude-sonnet-5-5` in every baseline run). Provider rows and aliases: `internal/providers/defaults.json`.

Known limits:

- `gpt-6-1-sol` on Databricks returned `REQUEST_LIMIT_EXCEEDED`: 106 refusals on 2026-11-10, per workspace share of
  requests dev 21%, qa 30%, prod 5%; none on 2026-11-11, no `Retry-After` header, the window cleared in 20-110 s.
- The proxy log on 2026-11-11 held 544 Databricks requests, all HTTP 200; `databricks-claude-sonnet-5-5` median
  3.8-7.6 s per request, by workspace.
- Later the same day one HTTP 502 and one HTTP 400 on `dev` opened a `claude` run (cause unknown; the log line has no
  message, message logging lands with the next install).
- Output ceilings: `gpt-oss-120b` 25,000 tokens; Claude 5.5 models 128,000.

## 4. Routes

`--route default|first-party|databricks[:<profile>]` picks the fallback ladder (`internal/providers/defaults.json`
`routing.ladders`); it is exported as `HARNESS_ROUTE`.

- `databricks:<ws>` runs the asked agent on that workspace first, whatever the agent's own pin
  (`internal/router/databricks.go` `gateArm`). Bare `databricks` uses the session's Databricks profile.
- With `--fallback none` the only candidate is the arm; a missing credential fails the call instead of moving on.
- `--fallback all` (default) adds sibling workspaces, the agent's own logins, then Claude; `agents` stops before
  Claude; `none` fails fast. Gates and scoring: `claude-code gateway --agent <a>`.
- `fallback from <agent>` on the `ran on` line means the final hop used a different agent, or a route other than the
  agent's pin (`internal/agent/fallback.go` `chainOutcome`). Before 2026-11-11 11:14 the label also fired for the
  `--route` arm itself: `--route databricks:qa` on codex (pin `prod`) ran on `databricks·qa`, status ok, one hop,
  yet printed `fallback from codex`, reason `codex·databricks·prod: outside databricks:qa`; 14 of the 24 baseline
  runs carry it, exactly where the workspace differed from the pin (codex `prod`, opencode `dev`, copilot GitHub).
  After the install (`internal/agent/route.go` `otherRoute` exempts the arm) the same codex call prints
  `fallback none`. A real fallback still shows; either way `hops[]` (`agent`, `account`, `model`) says what ran.
- `--profile <p>` leads then falls back (`--fallback none` pins). `claude-code <agent> profile <p>` changes the pin for
  every later run; a dispatch flag never does.

## 5. Dispatch

```bash
cd <repo> && claude-code agents ask --agent opencode --route databricks:qa --model databricks-claude-sonnet-5-5 \
  --tools --fallback none --background "$(cat brief.txt)"
```

Prints `job <id> started`. A `--tools` run gets 45 min per attempt, a tools-off run 10 min (`--timeout`).

| Need | Command |
| --- | --- |
| Newest dispatches, with agent, status, time, fallback line | `claude-code agents ask --log` |
| State, live log, final answer | `agents status <id>` · `agents tail -f <id>` · `agents result <id> [--format json]` |
| Interrupt, replay | `agents cancel <id>` · `agents ask --retry <id>` |
| Machine-readable result | `--format json`: `status`, `model`, `answer`, `usage`, `fallback`, `hops`, `ran_on`, `dropped` |
| Read-only job | `--read-only` (implies `--tools`); without `--tools` every agent is tools-off |
| Many at once | `agents ask --batch f.jsonl --jobs N`, `agents dispatch explore\|fleet\|compare` |

- `<id>` is a dispatch id or a unique prefix. Use `--workdir` and then check `git diff --stat`: opencode has written
  the live tree before. Run `claude-code <agent> setup` first when the agent is new to the machine.
- The `ran on` line (stderr, and `ran_on` in the envelope) is the proof of which agent, account and model answered.

## 6. Failure modes seen

| Symptom | Cause (known or not) | Do |
| --- | --- | --- |
| `REQUEST_LIMIT_EXCEEDED` on a GPT model | Databricks per-workspace GPT window; no `Retry-After`; clears in 20-110 s | `prod` first; wait about 2 min or retry on another workspace |
| A foreground dispatch dies near 9 minutes | A wrapper timeout, reported by the owner today (unverified; code deadlines are 10/45 min) | `--background`, then `tail -f` and `result` |
| `claude` on Databricks cannot write, status still `ok` | dontAsk mode with a base allow list; it refuses Write/Edit inside its config dir, which is every file of this repo | Send edits here to `opencode`, `codex` or `copilot` |
| Same run: `claude-code agents list` denied | Bash outside the allow list is denied in that mode | `--allow 'Bash(<cmd>:*)'`, or another agent |
| Same run: HTTP 502 and 400 on `dev` | Cause unknown (see section 3) | Retry on `qa` or `prod` |
| `opencode --read-only` fails: "the model answered with tool calls, but tools are off" | Binary before 11:14: the row lacked ReadOnly, so `applyCaps` dropped it and ran tools off | Reinstall; see below |
| `fallback from <agent>` with `--fallback none` | Binary before 11:14 labeled the `--route` arm (section 4) | Read `hops[]` |
| Codex pinned to `prod` | The pin applies unless a dispatch names a workspace | `--route databricks:<ws>` overrides per call (ran on `qa`, `dev`); `codex profile <p>` changes the pin |

opencode `--read-only`, both states. Before today's install: `partial`, `dropped: ["--read-only (tools off)"]`, the
model emitted `<invoke name="bash">` that never ran (reproduced on `qa`). After (build 11:14): the same
command answered `17` (correct, `ls internal/textutil` has 17 `.go` files), `dropped: []`, 6 s. The change is
`ReadOnly: agent.Emulated` in `internal/opencode/register.go` and `hub.go`: `agent.ReadOnlyDeny` (Bash, Write, Edit) is
appended to the dispatch permissions, read tools stay on, no `--auto`. That a Write or `bash -c` is refused was
checked by the fixing job, not here (unverified in this document). `copilot --read-only` also answered `17`
(`prod`, 6.6 s, 63k tokens).
