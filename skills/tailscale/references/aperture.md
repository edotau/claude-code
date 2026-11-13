# Aperture by Tailscale

Aperture is a centralized AI gateway between LLM clients (coding agents, apps, scripts) and upstream providers
(OpenAI, Anthropic, Google, Bedrock, Vertex, OpenRouter, others): one control point for API keys, access control,
spending limits, telemetry and audit.

> **Beta; docs change frequently.** The snippets below are orientation only. For provider settings, env vars, config
> keys, supported features or model names, **WebFetch the matching page** below; don't answer specifics from memory.

## Mental model (stable)

1. **Identity** — connections are identified by Tailscale identity (user email or device tag); no per-user API keys.
2. **Routing** — clients name a model (`claude-sonnet-5-5`); Aperture finds the provider, injects upstream
   credentials, forwards.
3. **Telemetry** — request/response, token counts, duration, tool use and session captured asynchronously.
4. **Session tracking** — related requests grouped; client session IDs (Claude Code, Codex) auto-detected.

**Deny-by-default**: without a matching grant, nothing is allowed.

## Canonical shapes (verify fields against live docs)

**A grant.** Grants live in *either* the Aperture configuration *or* the tailnet policy file, both with the
`tailscale.com/cap/aperture` capability. The only difference is `dst`: Aperture configuration grants **omit** it
(destination is the Aperture instance); policy-file grants **require** `dst` targeting the Aperture device (e.g.
`["tag:aperture"]`). Aperture configuration form:

```json
{
  "grants": [{
    "src": ["group:engineering"],
    "app": { "tailscale.com/cap/aperture": [{ "role": "user" }, { "models": "anthropic/**" }] }
  }]
}
```

- `role` and `models` are **separate objects** in the capability array.
- `models` is a **single glob string**, not an array (`**`, `anthropic/**`, `*/claude-sonnet*`); for several
  patterns add more `{"models": "..."}` entries.
- `role` (`user` or `admin`) is required: without it requests get HTTP 403 even when `models` matches.
- A `connectors` entry (array of FQN globs) grants MCP tools and HTTP connector proxies.
- `group:` sources need visible groups enabled for the Aperture device.

**A spending quota** (token bucket; capacity is the cap, rate refills it):

```json
{ "quotas": { "daily:<user>": { "capacity": "$10.00", "rate": "$5.00/day", "on_exceed": "reject" } } }
```

`<user>` expands to the Tailscale login (per person), `<node>` to node ID (per device). `on_exceed: "reject"` returns
HTTP 429.

**A provider** (Anthropic; others differ in `authorization` and `compatibility`):

```json
{
  "providers": {
    "anthropic": {
      "baseurl": "https://api.anthropic.com",
      "apikey": "YOUR_ANTHROPIC_API_KEY",
      "models": ["claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5"],
      "authorization": "x-api-key",
      "compatibility": {"anthropic_messages": true}
    }
  }
}
```

## Quickstart shape

```bash
open https://aperture.tailscale.com   # Create an instance
open http://ai/ui/                    # Dashboard from a tailnet device
curl -s http://ai/v1/messages -H "Content-Type: application/json" \
  -d '{"model":"claude-haiku-5-5","max_tokens":25,"messages":[{"role":"user","content":"hello"}]}'
```

`ai` resolves via MagicDNS (if not, MagicDNS is off). Use `http://`, not `https://`.

## Where to find current information — fetch these

| Topic | Fetch |
|---|---|
| What it is | <https://tailscale.com/docs/aperture/what-is-aperture> |
| Architecture | <https://tailscale.com/docs/aperture/how-aperture-works> |
| Setup | <https://tailscale.com/docs/aperture/get-started> |
| Config reference, JSON schema | <https://tailscale.com/docs/aperture/configuration> |
| Providers (any) | <https://tailscale.com/docs/aperture/set-up-providers> |
| Supported providers/models | <https://tailscale.com/docs/aperture/provider-compatibility> |
| Specific provider — Anthropic | <https://tailscale.com/docs/aperture/how-to/use-anthropic> |
| Specific provider — OpenAI | <https://tailscale.com/docs/aperture/how-to/use-openai> |
| Specific provider — Bedrock | <https://tailscale.com/docs/aperture/how-to/use-amazon-bedrock> |
| Specific provider — Vertex AI | <https://tailscale.com/docs/aperture/how-to/use-vertex-ai> |
| Specific provider — Vertex AI Express | <https://tailscale.com/docs/aperture/how-to/use-vertex-ai-express> |
| Specific provider — Gemini | <https://tailscale.com/docs/aperture/how-to/use-google-gemini> |
| Provider: Microsoft Foundry | <https://tailscale.com/docs/aperture/how-to/use-microsoft-foundry> |
| Specific provider — OpenRouter | <https://tailscale.com/docs/aperture/how-to/use-openrouter> |
| Specific provider — Vercel AI Gateway | <https://tailscale.com/docs/aperture/how-to/use-vercel> |
| Specific provider — OpenAI-compatible | <https://tailscale.com/docs/aperture/how-to/use-openai-compatible-tools> |
| Provider: self-hosted | <https://tailscale.com/docs/aperture/how-to/use-self-hosted> |
| Coding agents — overview | <https://tailscale.com/docs/aperture/use-your-tools> |
| Coding agents — Claude Code | <https://tailscale.com/docs/aperture/how-to/use-claude-code> |
| Coding agents — Claude Code GitHub Action | <https://tailscale.com/docs/aperture/how-to/use-claude-code-action> |
| Coding agents — Codex | <https://tailscale.com/docs/aperture/how-to/use-codex> |
| Coding agents — OpenCode | <https://tailscale.com/docs/aperture/how-to/use-opencode> |
| Aperture CLI (agents, bridges, Gemini CLI, Copilot, Claude Cowork) | <https://tailscale.com/docs/aperture/cli> |
| Devices not on the tailnet (ts-unplug) | <https://tailscale.com/docs/aperture/connect-outside-tailnet> |
| Access control concepts | <https://tailscale.com/docs/aperture/control-access> |
| Config grants vs. policy-file grants (`dst`) | <https://tailscale.com/docs/aperture/reference/aperture-vs-tailnet-grants> |
| Grant model access | <https://tailscale.com/docs/aperture/how-to/grant-model-access> |
| Grant admin role | <https://tailscale.com/docs/aperture/how-to/set-up-admin-access> |
| `group:` in grants (visible groups) | <https://tailscale.com/docs/aperture/visible-groups> |
| Quotas concepts | <https://tailscale.com/docs/aperture/manage-spending> |
| Per-user spending limits | <https://tailscale.com/docs/aperture/how-to/set-per-user-spending-limits> |
| Team-wide budget pool | <https://tailscale.com/docs/aperture/how-to/set-team-budget> |
| Checking & refilling budgets | <https://tailscale.com/docs/aperture/how-to/check-and-refill-budgets> |
| Guardrails concepts | <https://tailscale.com/docs/aperture/guardrails> |
| Setting up guardrails | <https://tailscale.com/docs/aperture/how-to/set-up-guardrails> |
| Webhooks/integrations | <https://tailscale.com/docs/aperture/integrate> |
| Building a custom webhook | <https://tailscale.com/docs/aperture/how-to/build-custom-webhook> |
| Integration — Oso | <https://tailscale.com/docs/aperture/integrate/oso> |
| Integration — Cerbos | <https://tailscale.com/docs/aperture/integrate/cerbos> |
| Integration — Cribl | <https://tailscale.com/docs/aperture/integrate/cribl> |
| Integration — Highflame | <https://tailscale.com/docs/aperture/integrate/highflame> |
| Observability, exports | <https://tailscale.com/docs/aperture/observe-and-export> |
| S3 export | <https://tailscale.com/docs/aperture/how-to/export-usage-data-to-s3> |
| MCP server proxying | <https://tailscale.com/docs/aperture/mcp-server> |
| Granting MCP tool access | <https://tailscale.com/docs/aperture/how-to/grant-mcp-tool-access> |
| Connectors overview | <https://tailscale.com/docs/aperture/connectors> |
| Connectors: get started | <https://tailscale.com/docs/aperture/connectors/get-started> |
| Connectors reference | <https://tailscale.com/docs/aperture/connectors/reference> |
| Connectors: client use | <https://tailscale.com/docs/aperture/connectors/use-a-connector> |
| Connectors: authenticated MCP | <https://tailscale.com/docs/aperture/connectors/set-up-authenticated-mcp-connector> |
| Connectors: HTTP API connector | <https://tailscale.com/docs/aperture/how-to/set-up-http-api-connector> |
| Connectors: per-user OAuth 2.0 | <https://tailscale.com/docs/aperture/how-to/set-up-per-user-oauth2-connector> |
| Chat UI (`chat_models`, `enable_chat_ui`) | <https://tailscale.com/docs/aperture/how-to/set-up-chat-ui> |
| Chat sandbox overview | <https://tailscale.com/docs/aperture/chat-sandbox> |
| Chat sandbox — enable | <https://tailscale.com/docs/aperture/how-to/enable-sandbox> |
| Chat sandbox — manage | <https://tailscale.com/docs/aperture/how-to/manage-sandbox> |
| Dashboard UI | <https://tailscale.com/docs/aperture/reference/dashboard> |
| Admin dashboard | <https://tailscale.com/docs/aperture/reference/dashboard-admin> |
| Reference index | <https://tailscale.com/docs/aperture/reference> |
| How-to index | <https://tailscale.com/docs/aperture/how-to> |
| Troubleshooting | <https://tailscale.com/docs/aperture/troubleshooting> |
| **Anything else** | <https://tailscale.com/docs/aperture> |
| Centralize their team's LLM API keys and track cost and usage per person | <https://tailscale.com/docs/use-cases/ai-infrastructure-access/centralize-llm-access-and-spending> |
| Route AI code reviews through a governed gateway | <https://tailscale.com/docs/solutions/route-ai-code-reviews-through-aperture> |

## Worked examples

| If the user wants to… | Fetch |
|---|---|
| Centralize team LLM keys and track cost/usage per person | <https://tailscale.com/docs/use-cases/ai-infrastructure-access/centralize-llm-access-and-spending> |
| Route AI code reviews through a governed gateway | <https://tailscale.com/docs/solutions/route-ai-code-reviews-through-aperture> |

## Answering pattern

Match the question to a row (or the Aperture index), WebFetch it, and quote config keys, env vars, model names and
prices verbatim. If a value isn't on the fetched page, say so rather than inventing it.
