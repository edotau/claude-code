---
name: dbx-expert
description: How model traffic reaches Databricks in this harness and how to diagnose it. Use for "a Databricks model call failed", "401/403/404/429/503 from Databricks", "stream disconnected", "which endpoint or wire does this model use", "add or check a Databricks model", "dev qa prod profile", "serving endpoint", "why is this slow or throttled", "what does this cost". Not for building Databricks apps, jobs, pipelines or SQL; those are Databricks' own skills, installed separately.
---

# Databricks serving (dbx-expert)

How an agent's model call reaches a Databricks workspace here, and what to run when it fails. Commands and gotchas
live in `rules/harness/providers.md` (Databricks routes, service-principal identity); this is the task-first how-to.
Symptoms: [references/errors.md](references/errors.md). Wires and limits: [references/wires.md](references/wires.md).

## The shape

```text
agent (claude|codex|copilot|gemini|opencode|pi)
  -> loopback proxy http://127.0.0.1:18765/p/<profile>/...   (token never printed)
    -> <workspace-host>/serving-endpoints/...               (dev, qa or prod workspace)
```

- Three workspaces are three profiles: `dev`, `qa`, `prod`. Agents call only the proxy, never the workspace.
- Model traffic uses only workspace `/serving-endpoints/...` routes, never AI Gateway. A row with another
  `options.wire` is refused.
- Credentials sit only in the encrypted vault or a provider's `auth.command`, never in a file or a log line.

## Which profile runs

First match wins; the full ladder is in `rules/harness/providers.md` (Routing precedence).

1. A `<profile>:` model prefix, then `--profile <p>` or an overlay `profile`.
2. `--provider databricks` or `--model auto`: the pool rotates over the workspaces (vault order dev, prod, qa;
   least busy under `auto`).
3. The session profile, then the agent's pin (`claude-code switch <profile>` sets the session one).

Print the decision without running anything: `claude-code gateway resolve <agent> --model <m> [--profile <p>]`.
A model the fresh listing lacks prints `served: no` with the served alternatives.

## Who signs in

- Headless work (`agents ask`, dispatches, batch) runs as the profile's service principal (m2m, a minted token).
- Interactive login is the user (u2m, browser). `claude-code databricks identity --profile <p> auto|user|sp` pins it.
- Doctor's credential rows end with `source=vault`, `source=env <NAME>` or `source=command`. An env var that
  shadows the vault entry is flagged `(shadows the vault entry)`: unset it or accept it.
- Sign in: `claude-code databricks login --profile <p>`. Smoke test: `claude-code databricks check --all`.

## Tokens to the proxy

- A session token is scoped to one profile's `/p/<profile>/` routes: `claude-code resolve --proxy`
  (`--refresh` mints a new one). Agents get theirs at launch.
- An editor needs a named client token: `claude-code gateway proxy --secret --client-token <name>` prints it once;
  `--revoke-client-token <name>` deletes it. It reaches only `/p/<profile>/` serving routes.
- Never print, paste or store a token. Credential verbs print `<redacted: N bytes>` unless `--secret`, which only
  the main session may run.

## Add or check a model

```bash
claude-code models discover --profile dev            # preview what the listing adds
claude-code models discover --profile dev --apply --verify   # save, and drop ids answered "deprecated"
claude-code models discover --profile dev --drop <id> --apply   # remove an id and stop re-adding it
claude-code models discover --undrop <id> --apply    # let discovery add it again
claude-code gateway resolve <agent> --model <id> --profile dev   # does it route, and on which wire
```

- A listing is not proof the model answers. `--verify` and `claude-code databricks check` send one tiny
  non-streaming request, which once passed for a model that then failed in real use with "stream disconnected".
  Always finish with a streamed call: `claude-code agents ask --agent <a> --profile dev --model <id> -- "say pong"`.
- Do not hand-edit generated agent menus: Codex catalog, OpenCode menu, Copilot catalog, Pi `models.json`.
  Change the source (discover, drop, the vault overlay), then `claude-code configure --restore --agents`
  (one agent: `--agent <a>`). Codex and Pi also rebuild theirs at launch from the cached listing.
- Refresh a stale listing: `claude-code doctor --refresh` or `claude-code models preflight --live`.

## Prompt caching

- The 1-hour cache is requested with `"cache_control": {"type": "ephemeral", "ttl": "1h"}` and no beta header.
  Databricks answers 400 to the beta header; the proxy already strips `advanced-tool-use-2025-11-20` on Databricks.
- Cache read and write counts are recorded per response (`cache_read_input_tokens`,
  `cache_creation_input_tokens`, and the 1-hour part). See them with `claude-code usage --days 7`.

## Cost

Today: token counts only, taken from proxy responses and the agents' own stores (`claude-code usage`). There is no DBU
price or dollar figure. A cost report would read Databricks system tables, which needs a SQL warehouse and grants.

## Do not

- Use AI Gateway routes, or call a workspace directly from an agent or with `curl` (probe with `agents ask`).
- Put a secret in a file, a brief, a commit or a log.
- Hand-edit generated agent configs or `settings.json` (`rules/harness/settings-management.md`).
- Change production from a session: it is read-only; deploys go through its pipeline.
