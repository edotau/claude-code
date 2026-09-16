# Competition Tournament

N leaves attempt the **same** task independently in isolated worktrees; the manager measures the
results and merges one. Use it when the objective is measurable (latency, size, pass rate, coverage)
or the approaches genuinely diverge and the best is not predictable.

Do not use it when one approach is obviously right, when the task has no comparable outcome, or when
the win would be smaller than the N× cost. This pattern multiplies tokens and 429 weight by N — cap N
by the concurrency tier (lean leaves ~8–10, deep-context ≤4).

## Process

1. **Baseline first.** Run the eval command on the base branch and record the number. Without a
   baseline, "improvement" is unverifiable and the tournament cannot be scored.
2. **Write one handoff, reused verbatim** for every competitor — same objective, same acceptance
   criteria, same eval command. Differing prompts measure the prompts, not the approaches. To force
   divergence, vary a single named constraint per leaf ("no new deps", "no schema change") and say so.
3. **Dispatch all leaves in one message** — `subagent_type: "code-workers"`, `isolation: "worktree"`,
   one branch per competitor: `compete/<slug>/<n>`. Each leaf commits its own work and reports the
   eval output it measured.
4. **Re-measure yourself.** Run the eval command in each worktree from the manager; a leaf's
   self-reported number is a claim, not evidence (`skills/approval-gate`). Discard leaves whose tests are red.
5. **Rank** — see modes below.
6. **Merge the winner** (`/workflow:worktree merge`), then **re-run the
   gates on the merged tree**: green in isolation is not green merged.
7. **Archive the losers** before removing anything: `git tag compete/<slug>/<n>` keeps every approach
   reachable, then drop the worktrees (`skills/using-git-worktrees`).

## Ranking modes

| Mode | Use for | How |
| --- | --- | --- |
| Metric | benchmarks, bundle size, pass rate, coverage | same eval command in each worktree; compare against baseline |
| Judge | readability, structure, API shape | read `git diff <base>...<branch>` per leaf; rank on correctness → simplicity → quality |
| Hybrid | metric with near-ties | metric first; judge only the leaves within ~10% of the leader |

Rank on the diff, never on the leaf's own summary of it.

## Stop conditions

- **Every leaf red** — merge nothing; report the common failure. A task all N missed is
  underspecified or wrong, not unlucky.
- **No leaf beats the baseline** — keep the baseline, archive the branches, and say what was tried.
  A tie with the baseline is not a win.
- **Worktrees left behind** — clean them before the next run; stale worktrees make the next
  tournament's branch state ambiguous.

## Rules

- Competitors never see or read each other's branches. Independence is the whole measurement.
- Never rebase or force-push a competitor branch — the losing approaches stay auditable via their tags.
- Leaves are terminal: a competitor cannot spawn its own tournament (`agents/code-workers/agent.md`).
- Cross-provider variant: when the point is comparing **model families**, run each competitor as
  `claude-code ask --agent <leg> --format json "<the same handoff>"` in its own worktree; the JSON
  envelope is its report (see `cross-model-agent-teams.md`).

Apply the dispatch and safety rules from [SKILL.md](../SKILL.md).
