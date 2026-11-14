# Dispatch policy details

Verbatim bullets moved from `rules/harness/agent.md` → Dispatch behavior.

## Overlay

- `agents ask --overlay <value>` / `gateway --overlay <value>` / `COWORKER_OVERLAY` (read when the flag is absent; the
  process env first, then the vault env for a dispatch that is not nested; interactive launches never read the vault's):
  ONE keyed JSON document (`schema` 1, all keys optional; `gateway --overlay` previews only the confinement plus `model`
  and `fallback`, ignoring `effort`, `host`, `remote` and the env layer): `provider` (`anthropic`, `openai`, `github`,
  `google`, `databricks`; omitted = the agent's pin; an agent that cannot use it is refused: claude anthropic, codex
  openai, copilot github, gemini google, opencode databricks only, every agent databricks; `databricks` confines only
  the Databricks hop, so unless `fallback` is `none` the dispatch may still fall back to the agent's own login, then
  `claude`), `profile` (one under it; databricks: or `auto`), `model`, `effort`, `fallback`, `host`, `remote` (like
  global `--remote`, `auto` allowed; refused with a host) and the env layer `set`/`unset`/`keep`. Value: a file path
  (regular, yours, not group/world-writable, <= 64 KiB), inline `'{...}'` or `-` (stdin; the task is then an argument);
  an unknown key or bad value exits 2. Typed flags win, except a typed `--profile`/`--provider`/`--model <p>:<alias>`
  outside the overlay's provider/profile is refused (exit 2). Precedence: typed flag > `--overlay`, else
  `COWORKER_OVERLAY` > `--retry`/`--job` record > process env > vault env > agent definition > agent pin > table
  default. Nested dispatches inherit only the confinement (`COWORKER_OVERLAY` cut to `provider`/`profile`; a standing
  default is the vault env `COWORKER_OVERLAY`); the rest of the document is one hop, and
  `--job`/`--background`/`--batch` relaunches carry it. `--host` carries the confinement (`provider`/`profile`) to the
  far machine and refuses a non-empty `set`/`unset`/`keep`; `--live` refuses the env layer too. `--retry` replays the
  recorded workspace unless the overlay names a provider. Interactive launches read it for `set`/`unset`/`keep` and
  refuse routing keys. Env layer: `files` reserved, refused; `set` values may be `@secret:<NAME>` (harness-internal
  secrets refused); a `set` name the launch already sets for its route is refused (`--env` still overrides); endpoint,
  proxy and TLS names (`*_BASE_URL`, `*_API_BASE`, `*_ENDPOINT`, `*_GATEWAY_URL`, `*_PROXY_URL`, `*_CA_CERT`,
  `DATABRICKS_HOST`, `HTTP(S)_PROXY`, `ALL_PROXY`, TLS overrides, `NODE_OPTIONS`) are refused in `set`, `keep` and
  `unset`, as are agent-config locations (`OPENCODE_CONFIG`, `CODEX_HOME`, `COPILOT_HOME`, `HOME`, `GH_CONFIG_DIR`, …):
  use `--profile`; `keep` refuses credential and `HARNESS_*` names, `unset` `HARNESS_*` names (`HARNESS_KEEP_ENV`
  stays the escape hatch). `--env` / `--keep-env` are its one-line shorthand (the same endpoint/proxy/TLS refusals;
  credential names in `--keep-env`, ask and interactive).

## Read-only and tools

The `ask --format json` bullet continues here from `--read-only` (its envelope, isolation flags and no-`--tools`
behavior stay in the rule):

  `--read-only` (implies `--tools`): reads, URL fetches and a vetted list of read-only shell commands run (plain
  `git log|show|diff|status|blame|ls-files|rev-parse|grep|branch --list`, `rg`, `grep`, `ls`, `cat`, `go doc|list|env`,
  read-only `claude-code` verbs; no `-c`, redirect, substitution or `agents diff --check`); every other Bash call and
  all Write/Edit stay denied, and a `Read(...)` deny also blocks a listed command that may reach that path. copilot
  enforces it in its SDK handler; gemini (agy hook), opencode (plugin) and claude (gated launch on every route, tools
  `Read,Glob,Grep,Bash`) use the `permission-gate`; opencode and claude refuse `--safe-mode`, gemini refuses a missing
  hook; codex drops its tools and keeps its read-only sandbox shell. codex and gemini refuse `--tools` with
  `--mode plan`. An `--allow` adds but can never reopen a matching deny. copilot file rules are judged as both Edit
  and Write, in absolute and workdir-relative form. copilot `--tools` approves every request like the other agents'
  `--tools`, still subject to the dispatch's permission rules (a Deny returns feedback and the run continues) and
  the native hooks. claude `--tools` off
  Anthropic is an allow list (go/git/read commands, `sed -n <range>p FILE`, `cd` into `--workdir` or below, env-prefixed
  and `time` forms of listed commands, the `rtk` spellings the rewrite hook emits, the read-only
  `claude-code agents find|diff|result|status|tree`, `memory recall|search|show`, `review simplify` and
  `network websearch` verbs, Edit/Write under `--workdir`; `--allow` adds,
  `--deny`/`--ask` refuse; bare `--autonomous` takes this same gated policy; `--mode plan` and `--safe-mode` keep the
  old launch, and `--safe-mode` with bare `--autonomous` is refused), not approve-all: no judge, so anything unlisted is
  denied. It runs gated like a memory job (`bypassPermissions`, because `dontAsk` hard-denies `.claude/` writes; tools
  `Read,Glob,Grep,Edit,Write,MultiEdit,Bash`, no MCP/Agent/WebFetch; file redirects refused, `2>&1` allowed).
