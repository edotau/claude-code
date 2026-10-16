# Port notes — secrets verb

Ported from claude-dbx (`internal/secrets/{secrets,scan}.go`, `internal/secretstore/secretstore.go`,
`internal/secrets/secretscmd/secrets.go`) into this repo's `internal/secrets` + `internal/cli/secrets.go`.
Vault, SP creds, the encrypted store, Bitbucket/Jenkins/devops, migrate, probe, and push/pull were
dropped — this harness has no Databricks workspace to back them.

## Section to paste into README.md `## Credentials` (replaces the "planned" paragraph, lines 107-113)

```markdown
`claude-code secrets` owns the `env.d/secrets.env` plane directly: `secrets set <key>` reads the
value on stdin and writes it at 0600 (`--keyring` stores it in the OS keyring instead — `secret-tool`
on Linux, `security` on macOS); `secrets get <key>` resolves keyring -> file -> process env
(`--export` for an eval-able line). `secrets status` reports each provider's credential source
without ever printing a value. `secrets scan`/`secrets git`/`secrets doctor` audit the config
surface, git history, and the store's own coherence (`secrets all` runs all three); each exits 1 on
a finding.
```

## Adaptation notes

- **Keyring plane gained macOS.** dbx's `secretstore` only shells out to `secret-tool` (Linux); this
  port adds a `runtime.GOOS` branch to `security add-generic-password`/`find-generic-password` for
  macOS, since this harness (unlike dbx) has no Linux-only deployment assumption. Simplified dbx's
  `atomic.Value` cross-call "latch" (for a many-sites-per-process loop) to a plain package `bool` —
  this CLI's `status` loop is single-threaded, so there is no concurrent-write race to guard against.
- **`secrets git` changed shape.** dbx's `ScanGit` runs `git grep` over currently-TRACKED file
  content (a point-in-time sweep). The task spec here asked for `git log -p` over history instead —
  it flags a secret-shaped line a commit ever ADDED, even one since deleted, since a rotated/removed
  secret is still sitting in history until a rebase or filter-repo scrubs it. Only the commit
  (12-char short hash) and file are reported, never the matched line — unlike a bare `git grep -n`
  report of file:line:content, which would otherwise echo the credential into the audit output.
- **`secrets scan` merges dbx's JSON-only scanner with a KEY=value line scanner.** The target file
  list mixes JSON configs (`settings.json`, `providers.json`, `.mcp.json`, `~/.claude.json`) with a
  plain `env.d/provider.env` pin file. `ScanFile` tries `json.Unmarshal` first and falls back to
  `providers.ReadEnvFile`'s KEY=value parser — one scanner, no per-file type tagging needed.
- **No git-tracked severity escalation.** dbx's scanner raises an inline-JSON finding from `local` to
  `TRACKED` when the file is committed. Dropped: the task asked for a flat exit-1-on-finding scan,
  and `settings.json`/`providers.json` are gitignored in this harness already (only `services.env`-
  style tracked files would need it, and none are in the default target list).
- **Known gap, not fixed here (out of scope for this port):** `secrets status`/`get` resolve a
  provider's credential as keyring -> file -> env, but `internal/providers/creds.go`'s `Credential()`
  (the ladder an actual launch uses) only checks env -> `env.d/secrets.env` -> `auth.file` ->
  `auth.command` — it never consults the keyring. So `secrets set --keyring ANTHROPIC_API_KEY` stores
  a credential a launch will NOT find; `status` reports this case as `source: keyring` with an
  explicit note ("provider launch does not read this plane yet") rather than claiming it would serve
  the provider. Wiring the keyring into `Credential()` would need `internal/providers` to import
  `internal/secrets`, which is a direction change outside this port's file list
  (`internal/secrets` + `internal/cli/secrets.go` only) — flagging for a follow-up, not applying it.
- **Credential regex not shared with `internal/mcpx/doctor.go`.** Its `looksLikeSecret`/`matchSlack`
  are unexported, so per the task's own fallback instruction this port defines its own heuristic in
  `internal/secrets` and leaves `mcpx` untouched. One pre-existing inconsistency worth a maintainer's
  eye (not touched here, outside this port's scope): `mcpx`'s `matchSlack` only recognizes `xoxb-`/
  `xoxp-`, while dbx's (and this port's) recognizes the full `xox[baprs]-` family (app/refresh/legacy
  tokens too) — the two detectors will disagree on a Slack app/legacy token.

## Bugs found in dbx source

None. Reviewed `LooksLikeSecret`/`matchSlack` (prefix/placeholder/JWT/base64 branches), the
`secretstore.Keyring` timeout + latch logic, and the JSON leaf-walk (`walkStrings`) — no incorrect
field read, dead branch, or off-by-one found in the parts read for this port.
