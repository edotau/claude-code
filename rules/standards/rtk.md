# rtk — Compact Shell Output

**Every Bash call whose output a model reads is written in its `rtk` form.** `rtk rewrite '<cmd>'` prints that form
(empty output: run the command as typed). The `rtk-rewrite` hook is skipped in auto mode and a rewrite stalls it, so
the prefix is written by the caller, never left to the hook (CLAUDE.md guideline 6).

- **Prefixed:** `git` (except `git grep`), `gh`, `go`, `grep`, `rg`, `find`, `ls`, `tree`, `diff`, `make`, `docker`,
  `kubectl`, `oc`, `curl`, `wc`, `ps`, `du`, `df`, `stat`, `jq`, `ssh`, `golangci-lint`; `cat f` becomes `rtk read f`,
  and `head`/`tail -N f` become `rtk read f --head-lines|--tail-lines N`. A pipeline prefixes its first command
  (`rtk git log --oneline | head`).
- **As typed:** `claude-code`, `git grep`, `gofmt -l` and anything `rtk rewrite` leaves empty.
- **Exact output** (a file list a later step consumes, a count or bytes you compare): `git grep`, or
  `rtk bypass <cmd>`; `rtk grep` groups and truncates matches.
- **Never in program code:** Go `exec`, Makefile recipes, hook commands, CI and scripts whose output a program parses
  run the bare tool; rtk changes the output format.
- **Docs, skills, agents, commands and brief templates:** a command block an agent will run is written in its rtk form;
  `README.md` steps for a human stay bare.
- **`rtk gain` prints `No hook installed`:** expected, the harness `rtk-rewrite` hook replaces rtk's own; never run
  `rtk init -g`.
