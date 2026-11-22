# rtk — Compact Shell Output

**Every Bash call whose output a model reads is written in its `rtk` form.** `rtk rewrite '<cmd>'` prints that form
(empty output: run the command as typed). In auto mode the `rtk-rewrite` hook only hints (a rewrite stalls it), so
the prefix is written by the caller, never left to the hook (CLAUDE.md guideline 6).

- **Prefixed:** `git` (except `git grep`), `gh`, `go`, `grep`, `rg`, `find`, `ls`, `tree`, `diff`, `make`, `docker`,
  `kubectl`, `oc`, `curl`, `wc`, `ps`, `du`, `df`, `stat`, `jq`, `ssh`, `golangci-lint`; the usual
  misses are `rtk grep -n`, `rtk git diff`, `rtk go test`. One file is the Read tool, never `cat`/`head` (in a
  pipeline `rtk read f`, `--head-lines|--tail-lines N`). A pipeline prefixes its first command
  (`rtk git log --oneline | head`).
- **Truncated or failed output:** a line `[full output: rtk recall <hash>]` means rtk saved the whole output; run that
  command instead of re-running the original.
- **As typed:** `claude-code`, `git grep`, `gofmt -l`, `go vet` and `go test -v|-list` (rtk 1.1.0 reports a failed
  vet as "No issues found" and drops verbose test output), and anything `rtk rewrite` leaves empty.
- **Exact output** (a file list a later step consumes, a count or bytes you compare): `git grep`, or
  `rtk bypass <cmd>`; `rtk grep` groups and truncates matches.
- **Code search:** tracked files first: `git grep -n` (25 ms here) or `rtk rg -n` (27 ms), symbols with
  `claude-code agents find`; `rtk grep -rn` walks every file (126–150 ms), so keep it for ignored or untracked paths.
- **Never in program code:** Go `exec`, Makefile recipes, hook commands, CI and scripts whose output a program parses
  run the bare tool; rtk changes the output format.
- **Docs, skills, agents, commands and brief templates:** a command block an agent will run is written in its rtk form;
  `README.md` steps for a human stay bare.
- **`rtk gain` prints `No hook installed`:** expected, the harness `rtk-rewrite` hook replaces rtk's own; never run
  `rtk init -g`.
