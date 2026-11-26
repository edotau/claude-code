# Dispatch policy details

Verbatim bullets moved from `rules/harness/agent.md` → Dispatch behavior.

## Overlay

- For the single overlay key catalogue (Go types, launch acceptance, child inheritance and tightening), the two-step
  implementation boundary and source precedence, see `rules/harness/agent.md` → Overlay. For route and fallback
  precedence, see `rules/harness/providers.md` → Routing precedence. `--job`/`--background`/`--batch` relaunches
  carry the document; `--retry` replays the recorded workspace unless a typed overlay names a provider. `--host`
  refuses a non-empty env layer, as does `--live`. Interactive launches read the env layer; an inherited route is a
  confinement, while an explicit `--overlay` may supply launch routing. Env layer: `set` values may be `@secret:<NAME>`
  (harness-internal secrets refused); a `set` name the launch already sets for its route is refused (`--env` still
  overrides); endpoint, proxy and TLS names (`*_BASE_URL`, `*_API_BASE`, `*_ENDPOINT`, `*_GATEWAY_URL`, `*_PROXY_URL`,
  `*_CA_CERT`, `DATABRICKS_HOST`, `HTTP(S)_PROXY`, `ALL_PROXY`, TLS overrides, `NODE_OPTIONS`) are refused in `set`,
  `keep` and `unset`, as are agent-config locations (`OPENCODE_CONFIG`, `CODEX_HOME`, `COPILOT_HOME`, `HOME`,
  `GH_CONFIG_DIR`, …): use overlay `provider` and `profile`; `keep` refuses credential and `HARNESS_*` names,
  `unset` `HARNESS_*` names
  (`HARNESS_KEEP_ENV` stays the escape hatch). `--env` / `--keep-env` are its one-line shorthand (the same
  endpoint/proxy/TLS refusals; credential names in `--keep-env`, ask and interactive).

## Tools

The `ask --format json` bullet continues here (its envelope and isolation flags stay in the rule). An ask with no
tools setting gets tools on.

A dispatch carries no permission policy. Overlay `tools: true` runs every agent unprompted, and `safety-scan` (the
`settings.json` deny rules, the AI-attribution block) applies to all of them. claude with `tools: true` off Anthropic
runs under `bypassPermissions` with the same hook.
