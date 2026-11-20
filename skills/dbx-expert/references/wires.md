# Databricks wires and limits

Each model rides one wire. The harness picks it from the cached workspace listing, not from a call.
Listing: `GET /api/2.0/serving-endpoints` (one endpoint: `/<name>`). Per endpoint it keeps `id`, `task`, `ready`,
`api_types`, `model_class`, `context` and `output` (`/Users/edotau/.claude/internal/router/served.go`).

| Wire | Proxy path under `http://127.0.0.1:18765/p/<profile>/` | Upstream under `<workspace-host>/serving-endpoints` | Models | Decided by |
| --- | --- | --- | --- | --- |
| anthropic | `v1/messages` (the root) | `/anthropic/v1/messages` | Claude | Claude family and an anthropic route |
| responses | `responses/...` | `/responses` | GPT and others the listing marks Responses | `api_types` has `openai/v1/responses` |
| openai (chat) | `openai/...` | `/chat/completions` | open-weights, Grok, GPT with no Responses | the fallback when neither above applies |
| gemini | `gemini/...` | `/gemini` | Gemini | agy only, native Gemini ids |

- Without `api_types` the rule falls back to the family: GPT that is not chat-only counts as Responses
  (`ResponsesServable`, `/Users/edotau/.claude/internal/providers/resolve.go`). Codex also refuses Grok.
- Pi sends Grok to chat: Databricks does not stream it on Responses.

## Who speaks what to the proxy

- claude: anthropic wire (`/v1/messages`). A Responses-only model is refused (no translation).
- codex: responses only (`wire_api = "responses"`).
- copilot: BYOK to the proxy; the Responses wire (`wire_api`) only when the model's wire is Responses.
- gemini (agy): `gemini/` for Gemini ids; every other family goes to `openai/`; Responses-only is refused.
- opencode: its menu splits Responses, native Claude and chat; each entry gets its own base.
- pi: three providers, `dbx-claude` (anthropic), `dbx-gpt` (responses), `dbx-chat` (openai).

## Limits

- Context and output come from the tracked catalog per field, else the listing's `context` and `output`
  (`LimitsFor`). The listing's numbers are parsed from the endpoint description.
- Nothing known: Codex assumes 128,000 (`fallbackContextWindow`) and warns; OpenCode writes no `limit` block (it needs
  both numbers); Pi omits `contextWindow` and `maxTokens`; Copilot does not read the listing and uses the catalog
  window with its own output cap.
- No verb prints them. `claude-code models --live --profile <p>` lists what a profile serves; read the numbers in
  `/Users/edotau/.claude/state/harness/preflight/served-<profile>.json` (refresh: `claude-code doctor --refresh`).

## Known gotchas

- GPT models on the chat wire reject `store` (HTTP 400); Pi sets `supportsStore: false` on `dbx-chat`.
- The 1-hour prompt cache is `ttl: "1h"` in `cache_control`, with no beta header.

## Print one wire decision

`claude-code gateway resolve <agent> --model <id> --profile <p>` prints the overlay and wire id and sends nothing.
`--format json` for scripts; exit 2 means the agent cannot run that model.
