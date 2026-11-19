# rtk — Compact Shell Output

**Every Bash call whose output a model reads is written in its `rtk` form.** `rtk rewrite '<cmd>'` prints that form
(empty output: run the command as typed). The `rtk-rewrite` hook is skipped in auto mode and a rewrite stalls it, so
the prefix is written by the caller, never left to the hook (CLAUDE.md guideline 6).

- **Prefixed:** `git` (except `git grep`), `gh`, `go`, `grep`, `rg`, `find`, `ls`, `tree`, `diff`, `make`, `docker`,
  `kubectl`, `oc`, `curl`, `wc`; `cat f` becomes `rtk read f`. A pipeline prefixes its first command
  (`rtk git log --oneline | head`).
- **As typed:** `claude-code`, `git grep`, `gofmt -l` and anything `rtk rewrite` leaves empty.
- **Exact output** (a file list a later step consumes, a count or bytes you compare): `git grep`, or
  `rtk bypass <cmd>`; `rtk grep` groups and truncates matches.
- **Never in program code:** Go `exec`, Makefile recipes, hook commands, CI and scripts whose output a program parses
  run the bare tool; rtk changes the output format.
- **Docs, skills, agents, commands and brief templates:** a command block an agent will run is written in its rtk form;
  `README.md` steps for a human stay bare.
