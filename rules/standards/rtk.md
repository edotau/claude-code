# rtk — Compact Shell Output

**Write every model-read Bash call in its `rtk` form.** `rtk rewrite '<cmd>'` prints it; empty means run as typed.
The `rtk-rewrite` hook rewrites in place in every mode, auto mode included.
Write the prefix yourself; vetoed commands and code-mode runs stay bare.

- **Prefixed:** `git` (except `git grep`), `gh`, `go`, `grep`, `rg`, `find`, `ls`, `tree`, `diff`, `make`, `docker`,
  `kubectl`, `oc`, `curl`, `wc`, `ps`, `du`, `df`, `stat`, `jq`, `ssh`, `golangci-lint`.
  Read one file with Read, never `cat`/`head`; in pipelines use `rtk read f` (`--head-lines|--tail-lines N`).
  Prefix a pipeline's first command (`rtk git log --oneline | head`).
- **No matches:** grep/rg/git grep exit 1 is an answer; never rerun it. End count/existence checks with `|| true`,
  or use `go list -deps -f` / `git grep -c`.
- **Truncated or failed output:** follow `[full output: rtk recall <hash>]`, never rerun the original.
  Batch related commands. Use `rtk bypass <cmd>` only for exact output or unusable results (unexpectedly empty,
  garbled or contradicting the exit code).
- **As typed:** `claude-code`, `git grep`, `gofmt -l`, `go vet`, `go test -v|-list`, and empty `rtk rewrite` results.
- **Exact output:** file lists for later use, counts and compared bytes use `git grep` or `rtk bypass <cmd>`;
  `rtk grep` groups and truncates.
- **Code search:** tracked files first with `git grep -n` or `rtk rg -n`; symbols with `claude-code agents find`.
  Reserve `rtk grep -rn` for ignored/untracked paths: it walks every file.
- **Program code:** program-parsed output stays bare in Go `exec`, Makefiles, hooks, CI and scripts.
  Model-read output in Go uses `rtkwrap.ForModel` (`internal/rtkwrap/form.go`), never hard-coded `rtk`;
  it falls back to bare commands in code mode, without rtk or on a veto.
- **Code mode, the one exception:** `CODEMODE=off|on|only` comes from a launcher, `--codemode` or the vault;
  the agent's vendor variable also counts. Dispatch precedence: typed `--codemode` > vault `CODEMODE` > off
  where the route supports it (`rules/harness/agent.md` → Overlay). `claude-code agents codemode on|off|status`
  controls this session; its explicit off overrides process/vendor mode. On/only disables rewriting by hook,
  shims and `rtkwrap.ForModel`; children keep their own mode.
- **Config:** `rtk make install` renders `config.toml` with telemetry off and `exclude_commands` for
  `go vet`, `go test -v`, `go test -list`.
- **Per agent:** claude/codex/opencode use the `rtk-rewrite` hook; copilot its SDK shell tool; pi PATH shims.
  agy (gemini) gets awareness text only. `claude-code doctor` shows one rtk row per agent.
- **Awareness (agy, in GEMINI.md):** prefix external programs, leave builtins bare; use `rtk bypass <cmd>` for
  exact output, `rtk recall <hash>` for elided output; skip rtk in code mode.
- **Docs, skills, agents, commands and briefs:** agent-run command blocks use rtk; human README steps stay bare.
- **No `No hook installed` banner:** launches and harness rtk calls set `RTK_SUPPRESS_HOOK_WARNING=1` and
  `RTK_TELEMETRY_DISABLED=1`. Never run `rtk init -g` or `rtk init`: the harness owns the hook and instructions.
  Add project filters deliberately, then `rtk trust`.
