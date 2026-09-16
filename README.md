# claude-code

A minimal, standalone harness for [Claude Code](https://docs.anthropic.com/en/docs/claude-code). This
repo **is** `~/.claude`: one Go binary, `claude-code`, plus the agents, slash commands, rules, skills and
Workflow scripts every session loads.

What the binary adds to a stock install:

- **Provider registry** — Anthropic, a Claude subscription, OpenRouter, OpenAI, Gemini, Ollama, or any
  gateway, selected per launch or pinned; tier slots (opus/sonnet/haiku/fable) map to model ids per provider.
- **Loopback router** — lets Claude Code talk to OpenAI-dialect upstreams (translation), mix providers per
  tier (`haiku=openai:gpt-5-nano`), and fail over on 429/5xx.
- **Agent runners** — one-shot tasks on Claude, Codex, Gemini, OpenCode or Copilot (`claude-code ask`), and
  interactive vendor CLIs wired to the same registry (`claude-code run`).
- **Hooks + status line** — a Bash safety gate, a context checkpoint, format-on-stop.

## Install

```bash
git clone https://github.com/edotau/claude-code ~/.claude   # or into an existing ~/.claude
cd ~/.claude
make install                        # builds bin/claude-code, links shims, renders settings.json
export PATH="$HOME/.claude/bin:$PATH"   # in your shell rc — the shims must come first
claude-code doctor
```

`make install` runs `claude-code install`: it copies the binary into `~/.claude/bin`, symlinks the shims
`claude codex gemini opencode copilot` → `claude-code`, and merges the harness hooks/permissions/statusLine
into `settings.json` after a timestamped backup (foreign hooks are preserved). Preview with
`claude-code install --dry-run`; print the render alone with `claude-code settings`.

With `~/.claude/bin` first on `PATH`, `claude` launches Claude Code on the selected provider and
`codex`/`gemini`/`opencode`/`copilot` launch those CLIs through the registry. The real binaries are found
past the shims.

## Providers

Built-ins live in `internal/providers/defaults.json`. `~/.claude/providers.json` overlays them **by name**
(a provider entry replaces the built-in; `"name": null` removes one; `default`/`fallback` override when
set). Start from [`providers.example.json`](providers.example.json).

```jsonc
{
  "default": "litellm",
  "fallback": ["litellm", "openrouter"],        // 429/5xx ladder (implies the router)
  "providers": {
    "litellm": {
      "kind": "anthropic",                       // dialect base_url itself speaks
      "base_url": "https://llm.example.com",
      "routes": {                                // extra dialects: path suffix or absolute URL
        "openai": "/v1"
      },
      "auth": {"type": "bearer", "env": "LITELLM_API_KEY"},
      "headers": {"x-team": "platform"},
      "models": {"opus": "claude-opus-5", "sonnet": "claude-sonnet-5", "haiku": "gpt-5-nano"},
      "one_m": true                              // upstream serves 1M context for Claude ids
    }
  }
}
```

| Field | Meaning |
| --- | --- |
| `kind` | Dialect `base_url` speaks: `anthropic` (`/v1/messages`), `openai` (`/chat/completions`), `responses`, `gemini` |
| `routes` | More dialects on the same gateway — a path joined to `base_url`, or an absolute URL |
| `auth.type` | `x-api-key` · `bearer` · `none` (local servers) · `passthrough` (Claude subscription OAuth) |
| `models` | slot → model id |
| `headers` | Sent on every upstream request |
| `one_m` | Adds the `[1m]` marker to Claude ids |

```bash
claude-code providers                      # * = default, credential state, dialects, fallback ladder
claude-code providers litellm              # routes, auth, models
claude-code use litellm                    # pin the session provider (env.d/provider.env)
claude-code models                         # resolved slots and whether the router is needed
claude-code models --pin haiku=openai:gpt-5-nano --pin opus=claude-opus-5
claude-code models --unpin
claude-code models --live --provider openrouter   # what the upstream serves
claude --provider openrouter --model anthropic/claude-sonnet-5   # this launch only
```

Selection order — provider: `--provider` → `HARNESS_PROVIDER` → registry `default`. Slot:
`HARNESS_<SLOT>_MODEL` → `HARNESS_MODEL` → the provider's `models` table. Process env beats
`env.d/provider.env`. A pin of the form `provider:model` crosses providers for that slot.

## Credentials

A provider's credential resolves through a ladder; the first non-empty source wins:

1. **Process env** — `auth.env` (e.g. `OPENROUTER_API_KEY`)
2. **`~/.claude/env.d/secrets.env`** — `KEY=value` lines for the same name
   ([`env.d/secrets.env.example`](env.d/secrets.env.example); chmod 600; gitignored)
3. **`auth.file`** — a file holding the credential (`~` expanded)
4. **`auth.command`** — a shell command that prints it, cached for `auth.ttl` seconds (default 300) and
   re-run after an upstream 401:

```json
"auth": {"type": "bearer", "command": "security find-generic-password -w -s openrouter", "ttl": 3600}
```

Other commands that fit: `op read op://Private/openrouter/credential`, `pass show openrouter`, or a
script that mints a short-lived gateway token. `claude-code token --provider <p>` prints the resolved
credential — it is the `apiKeyHelper` Claude Code calls, so no key is stored in `settings.json`.

## Routing

`claude` launches **direct** when the provider has an `anthropic` route, every slot stays on that provider,
and no `fallback` is configured: `ANTHROPIC_BASE_URL` points at the route and `apiKeyHelper` is
`claude-code token --provider <p>` (omitted for `passthrough`, so subscription login keeps working).

Otherwise it launches through the **loopback router** (started on demand, `127.0.0.1:18765` by default):
`ANTHROPIC_BASE_URL=http://127.0.0.1:<port>/p/<provider>` and `apiKeyHelper` = `claude-code token --router`,
a per-install client secret the router requires. Per request the router:

1. checks the client secret and a loopback `Host`;
2. resolves `model` — a plain id on the session provider, or `provider:model` — and strips `[1m]`;
3. `anthropic` route → byte passthrough with the upstream credential injected; `openai` route →
   Anthropic Messages ⇄ chat-completions translation, including streaming and tool calls
   (`count_tokens` is answered locally);
4. on 401/403 refreshes the credential and replays once;
5. on 429/5xx/connect error **before any byte reaches the client**, moves to the next provider in
   `fallback`, mapping the model to the same slot there (429s honor `Retry-After` as a cooldown).

```bash
claude-code router start | status | stop
claude-code router serve --port 18765      # foreground, for debugging
claude --router                            # force the router for a direct-capable provider
```

Subscription (`passthrough`) auth cannot go through the router; the launch says so.

## Agents

```bash
claude-code agents                                   # runners and whether each is available
claude-code ask --agent codex "Review the diff on this branch for correctness bugs."
claude-code ask --agent gemini --format json "Map every caller of providers.Select."
claude-code run opencode --provider openrouter --model qwen/qwen3-coder
claude-code run codex --print-env                    # show the env/argv wiring, launch nothing
```

Exec runners (codex, gemini, opencode) and the interactive `run` launch share one wiring function, so a
gateway route written once serves both. Inside a session, the subagents in `agents/` (`code-workers`,
`test-driven-dev`, `test-repair`) and the Workflow scripts in `workflows/` handle in-process fan-out; see
`rules/workflow/master-workflow.md`.

## Hooks

Declared in `internal/hookspec/hookspec.go`, implemented in `internal/hooks/`, rendered into
`settings.json` as `$HOME/.claude/bin/claude-code hook <name>`:

| Hook | Event | What it does |
| --- | --- | --- |
| `safety` | PreToolUse (Bash) | Blocks destructive shapes: `rm -rf /`, force-push to main, `curl \| sh`, `sudo`, `DROP TABLE`, … |
| `context-checkpoint` | Stop | Once per session at ≥85% context, blocks Stop so the model can hand off before compaction |
| `stop-format` | Stop | Formats files edited this turn (gofmt, ruff, prettier, shfmt when installed) |

To add one: implement it in `internal/hooks/`, add a `Spec` to `hookspec.Registry`, `make install`.

## Layout

```
cmd/claude-code/     main
internal/            cli (verb table) · providers · router · translate · launch · vendors · agent
                     hooks · hookspec · settings · statusline · models · paths
agents/ commands/ rules/ skills/ workflows/   loaded by Claude Code
env.d/               provider.env (pins) · secrets.env (credentials) — untracked; *.example tracked
docs/plans/          implementation plans
```

## Development

```bash
go test ./internal/<pkg>           # while developing — only what you touched
make lint                          # gofmt check
make test                          # go vet + full suite, once, before pushing
```

`settings.json` is generated — edit `internal/settings/settings.tmpl.json` and re-run `make install`.
