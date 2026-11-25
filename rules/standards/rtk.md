# rtk — Compact Shell Output

**Every Bash call whose output a model reads is written in its `rtk` form.** `rtk rewrite '<cmd>'` prints that form
(empty output: run the command as typed). The `rtk-rewrite` hook enforces it: in every mode, auto included, it
rewrites the call in place (no permission decision, so auto mode's classifier still judges it), so write
the prefix yourself (CLAUDE.md guideline 6). Its vetoes (below) pass as typed, and so does every code-mode run
(`CODEMODE`, below).

- **Prefixed:** `git` (except `git grep`), `gh`, `go`, `grep`, `rg`, `find`, `ls`, `tree`, `diff`, `make`, `docker`,
  `kubectl`, `oc`, `curl`, `wc`, `ps`, `du`, `df`, `stat`, `jq`, `ssh`, `golangci-lint`; the usual
  misses are `rtk grep -n`, `rtk git diff`, `rtk go test`. One file is the Read tool, never `cat`/`head` (in a
  pipeline `rtk read f`, `--head-lines|--tail-lines N`). A pipeline prefixes its first command
  (`rtk git log --oneline | head`).
- **grep exit 1 is an answer:** `grep`, `rg` and `git grep` exit 1 on no match, not a failure: never re-run it; end a
  count or existence check with `|| true` (or use `go list -deps -f`, `git grep -c`).
- **Truncated or failed output:** a line `[full output: rtk recall <hash>]` means rtk saved the whole output; run that
  command instead of re-running the original. rtk output is condensed but sufficient: run commands normally and batch
  related ones into one call; re-run as `rtk bypass <cmd>` only when the result is unusable (empty where output was
  expected, contradicting its exit code, or garbled) or exact output is required (see Exact output).
- **As typed:** `claude-code`, `git grep`, `gofmt -l`, `go vet` and `go test -v|-list` (rtk 1.1.0 reports a failed
  vet as "No issues found" and drops verbose test output), and anything `rtk rewrite` leaves empty.
- **Exact output** (a file list a later step consumes, a count or bytes you compare): `git grep`, or
  `rtk bypass <cmd>`; `rtk grep` groups and truncates matches.
- **Code search:** tracked files first: `git grep -n` (25 ms here) or `rtk rg -n` (27 ms), symbols with
  `claude-code agents find`; `rtk grep -rn` walks every file (126–150 ms), so keep it for ignored or untracked paths.
- **Program code:** output a program parses (exit code, JSON, file list, bytes) stays bare in Go `exec`, Makefile
  recipes, hook commands, CI and scripts; rtk changes the output format. Harness Go code whose output a model reads
  calls `rtkwrap.ForModel` (`internal/rtkwrap/form.go`), never a hard-coded `rtk`: it returns the rtk form, or the
  bare command in code mode, with no rtk binary, or on a veto.
- **Code mode, the one exception:** `CODEMODE` (off|on|only) is set by a code-mode launcher, by `--codemode`, or in
  the vault; the running agent's own vendor variable counts too. Precedence (D2): typed `--codemode` > vault `CODEMODE`
  > off, so a session keeps rtk unless `/codemode on` (coming). While it is on or only nothing is rewritten to rtk: not
  the hook, not the shims, not `rtkwrap.ForModel`.
- **Config:** `make install` renders rtk's `config.toml` with `exclude_commands` for `go vet`, `go test -v` and
  `go test -list`, and telemetry off.
- **Per agent:** claude, codex and opencode rewrite through the `rtk-rewrite` hook; copilot through its SDK shell
  tool; pi through the `rtkwrap` PATH shims; agy (gemini) cannot rewrite, so it only gets the awareness text below.
  `claude-code doctor` shows one rtk row per agent.
- **Awareness (agy, rendered into its GEMINI.md):** prefix external programs with `rtk`, leave shell builtins bare,
  use `rtk bypass <cmd>` for exact output and `rtk recall <hash>` for elided output, and skip rtk in code mode.
- **Docs, skills, agents, commands and brief templates:** a command block an agent will run is written in its rtk form;
  `README.md` steps for a human stay bare.
- **No `No hook installed` banner:** every agent launch and harness rtk call sets `RTK_SUPPRESS_HOOK_WARNING=1` and
  `RTK_TELEMETRY_DISABLED=1`; the harness `rtk-rewrite` hook replaces rtk's own, so never run `rtk init -g`; plain
  `rtk init` is never run either: it appends instructions to the repo's CLAUDE.md and creates `.rtk/filters.toml`
  (project filters are added deliberately, then `rtk trust`).
