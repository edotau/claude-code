# Plan — standalone `claude-code` harness (minimal fork of claude-dbx, no Databricks)

Status: in progress · 2026-09-16 · source: `/Users/edotau/claude-dbx` (read-only reference)

Replaces `claude-dbx/docs/plans/plan-solo-edition.md` ("flag, not fork"): a clean fork that keeps the
routing/gateway/agent features and drops every Databricks, Apollo, employer, cockpit and research subsystem.
Target: ~6–8k LOC Go (from ~150k), stdlib-first, external SDKs boxed one package each.

## Decisions (user, 2026-09-16)

- Binary + module: `claude-code` / `github.com/edotau/claude-code`; installs to `~/.claude/bin` with shims.
- Agent legs: claude (stream-json), Anthropic/OpenAI HTTP, codex, gemini, opencode (exec), copilot (SDK).
- Routing v1: provider registry + loopback router + Anthropic⇄OpenAI chat-completions translation.
- `settings.json`: build only — the live file is never touched by development; `install` backs up and
  preserves foreign (non-harness) hooks when the user runs it.

## Layout

```
cmd/claude-code/            main → cli.Run
internal/paths              ConfigDir, EnvDir, StateDir, AtomicWrite, WithFileLock          [done]
internal/models             ParseClaudeID, MaybeAdd1M/Strip1M, ContextWindow, ThinkingKnobs  [done]
internal/providers          registry (embedded defaults.json + ~/.claude/providers.json), Route(dialect),
                            Credential ladder, Authorize, pins (env.d/provider.env), Select/ClientModel,
                            NeedsRouter, FailoverTarget                                        [done]
internal/cli                verb table + argv0 shims; per-area verb files                     [skeleton]
internal/translate          Anthropic Messages ⇄ OpenAI chat-completions (req, resp, SSE)     [workstream A]
internal/router             loopback proxy: target, auth inject, retry-on-401, 429/5xx failover [workstream B]
internal/launch             claude binary discovery, launch env, --settings overlay, vendor launch [C]
internal/hookspec, hooks    safety, stop-format, context-checkpoint                           [C]
internal/settings           template render + install merge                                   [C]
internal/statusline         one style: model, provider, context %, git                        [C]
internal/agent              Runner interface, dispatch (errors/retry/spawn), adapters         [D]
internal/anthropicsdk, copilotsdk   boxed SDKs                                                 [D]
agents/ commands/ rules/ workflows/ CLAUDE.md README.md Makefile .gitignore env.d/*.example    [E]
```

## Routing model (the contract every workstream shares)

- **Provider** = `{kind, base_url, routes{dialect→path|url}, auth{type,env,file,command,ttl}, models{slot→id}, headers, one_m}`.
  Dialects: `anthropic` (`/v1/messages`), `openai` (`/chat/completions`), `responses`, `gemini`. A gateway
  (LiteLLM, OpenRouter, a corporate AI gateway) is just a provider with several `routes`.
- **Selection**: provider = `--provider` → `HARNESS_PROVIDER` → registry default. Slot = `HARNESS_<SLOT>_MODEL`
  → `HARNESS_MODEL` → provider table. A pin may be `provider:model`, crossing providers per tier.
- **Claude Code sees** `selection.ClientModel(slot)`: plain id (+`[1m]` if `one_m`) on the session provider, or
  `provider:model` for a cross-provider slot.
- **Direct launch** when `!NeedsRouter`: `ANTHROPIC_BASE_URL` = anthropic route; `apiKeyHelper` =
  `claude-code token --provider <p>` (omitted for `passthrough`, so subscription OAuth works).
- **Router launch** otherwise: `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>/p/<provider>`; `apiKeyHelper` =
  `claude-code token --router` (a per-install client secret the router requires — the source router accepted
  any loopback caller while injecting live keys). Per request the router:
  1. authenticates the client secret (`x-api-key` or bearer), validates Host is loopback;
  2. parses `model` from the body → `providers.ParseTarget` (session provider from the `/p/<name>` path prefix);
  3. strips `[1m]`, rewrites `model` to the wire id;
  4. anthropic route → byte passthrough with `providers.Authorize`; else openai route → `translate`;
  5. on 401/403 → `providers.Refresh` + replay once; on 429/5xx/connect error **before any byte is written** →
     next provider in `registry.fallback` via `providers.FailoverTarget`;
  6. streams flush per chunk; 10-minute idle cutoff; `count_tokens` answered locally for translated legs.

## Agent runners

```go
type Request struct{ Prompt, Model, Provider, Effort, WorkDir, SessionID string; Tools bool; Timeout time.Duration }
type Result  struct{ Agent, Provider, Model, SessionID, Answer string; Usage Usage; Duration time.Duration }
type Runner  interface{ Name() string; Available() error; Run(ctx context.Context, req Request, stream io.Writer) (Result, error) }
```
One registry, one retry loop (`dispatch.Do`), `ask --format json` marshals `Result` directly (no stdout
scraping). Exec adapters (codex/gemini/opencode) share one `Env/Argv(provider, model)` function with the
interactive `claude-code run <vendor>` launch so gateway wiring is written once.

## Dropped on purpose

dbxcfg/dbxauth/dbxsql/databricks/apollo/identity/pki/vault/devops · U2M/SP/PAT minting · workspace env
buckets and spill pools · model discovery cache · mux/board/agenttui cockpits · memory bank, evidence, ledger,
knowledge corpus (Claude Code's native auto-memory covers it) · settings history/overlay sync targets ·
secretstore (Linux-only; `auth.command` covers keychain/1Password) · crossgen vendor mirrors · employer
scripts, MCP servers, themes.

## Verification

Scoped `go test ./internal/<pkg>` per workstream; live checks: `claude-code providers`, `models`, router
against an `httptest` upstream in both dialects, `claude-code settings` render diffed against the live file
(never written). Full `make test` once at the end.
