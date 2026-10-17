# Vendor wiring audit — 2026-10-17

Physical audit: one real request through every runner and provider, then codex / copilot / opencode each reviewed
the Go wiring for their own CLI (`claude-code ask --agent <v>` fed `internal/vendors/vendors.go`,
`internal/agent/vendor.go`, `internal/generate/generate.go`). Verified claims against the installed binaries
(`copilot help environment`, `codex exec --help`, `codex login status`).

## Live results (before fixes)

| Runner / provider | Result | Cause |
| --- | --- | --- |
| claude (subscription) | OK | |
| api on subscription | fails by design | passthrough auth; the api runner needs a key |
| api on anthropic | HTTP 401 | stored `ANTHROPIC_API_KEY` is rejected — doctor said "present" |
| api on gemini / gemini runner (opus slot) | HTTP 429, limit 0 | free-tier key cannot serve `gemini-4-argon`; `--model sonnet` (Flash) answers in 4 s |
| api on ollama | connection refused | ollama not running |
| api on openai / openrouter | no credential | keys never set |
| codex (ChatGPT login) | OK | |
| opencode (own config, gemini-3.8-flash) | OK | configure wrongly reported it ABSENT / FIX |
| copilot (GitHub login) | OK | |
| opencode, 97 KB review prompt | exit 0, empty answer | model returned no text; harness reported success |

## Fixed in this pass

- configure: a vendor on its own login/config (`ErrOwnLogin`) is configured, not ABSENT/FIX; duplicate fallback
  notes dropped; stored keys are probed live (`providers.ProbeCredential`: verified / REJECTED / unreachable) and a
  rejected key offers re-entry on a TTY. doctor probes every present credential the same way.
- install re-renders AGENTS.md/GEMINI.md (no more DRIFT after a CLAUDE.md edit).
- codex: login detected via `codex login status` (keyring-aware; auth.json fallback); prompt rides stdin behind
  the `-` operand so "help" is no longer a subcommand.
- copilot: token precedence COPILOT_GITHUB_TOKEN > GH_TOKEN > GITHUB_TOKEN; `[1m]` stripped on the login path;
  provider headers forwarded (COPILOT_PROVIDER_HEADERS); bearer-only on bearer routes; inherited BYOK vars
  blanked; prompt budget = window − output cap.
- runners: a clean exit with no text is now an error, not a blank success.
- memory: linked worktrees share their main checkout's bank (`paths.RepoSlug` → `MainCheckout`); harvest
  receipts count global-bank writes; `memory doctor` flags lost pending / recent failed harvests.

## Open (triaged from the reviews; not fixed)

| Sev | Where | Finding | Note |
| --- | --- | --- | --- |
| HIGH | generate.go renderCodex / spliceBannerFile | Appending the managed block after an existing root `model` duplicates the key; a trailing table absorbs the root keys | Inactive here (codex runs on ChatGPT login, render skipped). Fix = structural TOML merge |
| HIGH | generate.go codex env_key | Rendered `env_key = HARNESS_CODEX_API_KEY` only exists under harness launch; bare `codex` cannot auth | By design (secrets never in config); document, or render a token command |
| HIGH | codexHeadless Tools=false | `--sandbox read-only` limits the filesystem, does not disable tools | README already says "read-only sandbox"; true tools-off would need per-tool denial |
| MED | generate.go splitTopLevelInline | Comma inside a quoted value splits the inline table | Fix with a TOML encoder when the render is next touched |
| MED | vendors.go codexOwnModel | Reuses config.toml `model` across providers (gateway model → ChatGPT) | Compare the model's provider before reuse |
| MED | copilotRunner | SDK errors not wrapped in `*Error{Kind}`; no MaxPromptBytes guard | Wrap + guard |
| MED | copilotHeadless | `--prompt` only: no model/resume/silent; `vendorRunner` path for copilot is dead | Confirm dead, delete or complete |
| MED | copilot BYOK | `COPILOT_MODEL` may be ignored in SDK/server mode; pass model + provider via `copilotsdk.Options` | Verify with a BYOK route first |
| LOW | copilotLoginPresent | A `ghp_` classic PAT passes but lacks Copilot entitlement | Reject `ghp_` prefix |
| — | gemini provider | Opus slot `gemini-4-argon` unusable on the free-tier key | FIXED: hard-quota 429 demotes to the next slot in the router (dead model remembered 30 min) and in `ask` runners |
